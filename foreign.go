package main

// Нецелевые заказы со SKU из очереди:
//   - пополнение: если в заказе есть SKU пары в продаже, её остаток возвращается (будто заказа не было);
//   - отмена (настройка, по умолчанию выключена): заказ в «Ожидает сборки» / «Ожидает отгрузки» отменяется
//     с причиной «товар закончился», после чего остаток всех SKU заказа возвращается, будто заказа не было.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

const fallbackCancelReason = 352 // «Товар закончился на складе продавца»

func postingWarehouse(p *Posting) int64 {
	wh := p.DeliveryMethod.WarehouseID
	if wh == 0 && p.AnalyticsData != nil {
		wh = p.AnalyticsData.WarehouseID
	}
	return wh
}

// queueSince — когда начата очередь: более ранние заказы программа не отменяет.
func (w *Worker) queueSince() time.Time {
	if !w.prof.QueueStarted.IsZero() {
		return w.prof.QueueStarted
	}
	var t time.Time
	for _, b := range w.bt {
		if !b.StartedAt.IsZero() && (t.IsZero() || b.StartedAt.Before(t)) {
			t = b.StartedAt
		}
	}
	return t
}

// saleBatch — партия в продаже (основная или заранее), в паре которой есть SKU.
func (w *Worker) saleBatch(sku int64) *Batch {
	for i := range w.bt {
		b := &w.bt[i]
		if (b.State == BatchActive || b.State == BatchNext) && (b.SKU1 == sku || b.SKU2 == sku) {
			return b
		}
	}
	return nil
}

func (w *Worker) inQueue(sku int64) bool {
	for _, b := range w.bt {
		if b.SKU1 == sku || b.SKU2 == sku {
			return true
		}
	}
	return false
}

// pairDesired — какой остаток должен быть у SKU, которым управляет очередь (ok=false — SKU очередь не ведёт).
func (w *Worker) pairDesired(sku int64) (int, bool) {
	if b := w.saleBatch(sku); b != nil {
		if w.prof.Paused || w.prof.StockOff {
			return 0, true
		}
		q := b.Qty - b.Sold
		if q < 0 {
			q = 0
		}
		return q, true
	}
	for _, b := range w.bt {
		if b.State == BatchQueued && (b.SKU1 == sku || b.SKU2 == sku) {
			return 0, false
		}
	}
	for _, b := range w.bt {
		if b.State == BatchDone && !b.Keep && (b.SKU1 == sku || b.SKU2 == sku) {
			return 0, true
		}
	}
	return 0, false
}

// setStocks выставляет остатки (в тесте — только пишет в журнал).
func (w *Worker) setStocks(ctx context.Context, want map[int64]int) error {
	skus := make([]int64, 0, len(want))
	for k := range want {
		skus = append(skus, k)
	}
	sort.Slice(skus, func(i, j int) bool { return skus[i] < skus[j] })
	if !w.prof.Live {
		var parts []string
		for _, k := range skus {
			parts = append(parts, fmt.Sprintf("SKU %d → %d шт.", k, want[k]))
		}
		w.log.Add("test", "[ТЕСТ] Выставил бы остаток: %s. В Ozon ничего не отправлено.", strings.Join(parts, ", "))
		return nil
	}
	var errs []string
	for _, k := range skus {
		if err := w.setSKUStock(ctx, []int64{k}, want[k]); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// available — свободный остаток (без резерва) на складе магазина.
func (w *Worker) available(ctx context.Context, skus []int64) (map[int64]int, error) {
	rows, err := w.client.StocksFBS(ctx, skus)
	if err != nil {
		return nil, err
	}
	out := map[int64]int{}
	for _, s := range skus {
		out[s] = 0
	}
	for _, r := range rows {
		if r.WarehouseID != w.prof.WarehouseID {
			continue
		}
		if _, ok := out[r.SKU]; ok {
			if a := r.Present - r.Reserved; a > 0 {
				out[r.SKU] += int(a)
			}
		}
	}
	return out, nil
}

func (w *Worker) cancelReasonID(ctx context.Context) (int64, string) {
	if w.cancelReason != 0 {
		return w.cancelReason, w.cancelReasonTitle
	}
	id, title := int64(fallbackCancelReason), "Товар закончился на складе продавца"
	if list, err := w.client.CancelReasons(ctx); err == nil {
		for _, r := range list {
			if strings.Contains(strings.ToLower(r.Title), "закончил") {
				id, title = r.ID, r.Title
				break
			}
		}
	}
	w.cancelReason, w.cancelReasonTitle = id, title
	return id, title
}

// handleForeign обрабатывает нецелевые заказы со склада магазина (каждый — один раз).
func (w *Worker) handleForeign(ctx context.Context, list []*Posting, now time.Time) {
	since := w.queueSince()
	for _, p := range list {
		if ctx.Err() != nil {
			return
		}
		if w.journal.ForeignGet(p.PostingNumber) != nil {
			continue
		}
		qty := map[int64]int{}
		hasQ := false
		for _, pr := range p.Products {
			qty[pr.SKU] += pr.Quantity
			if w.inQueue(pr.SKU) {
				hasQ = true
			}
		}
		if !hasQ {
			continue // заказ других товаров — не наше дело
		}
		cancellable := p.Status == "awaiting_packaging" || p.Status == "awaiting_deliver"
		if w.prof.CancelForeign && cancellable && !since.IsZero() && !p.InProcessAt.Time.Before(since) {
			if w.cancelForeign(ctx, p, qty, now) {
				continue
			}
		}
		w.replenish(ctx, p, qty, now)
	}
}

// replenish возвращает остаток SKU пар в продаже, которые купили в нецелевом заказе.
func (w *Worker) replenish(ctx context.Context, p *Posting, qty map[int64]int, now time.Time) {
	want := map[int64]int{}
	var parts []string
	for sku, q := range qty {
		b := w.saleBatch(sku)
		if b == nil || b.StartedAt.IsZero() || p.InProcessAt.Time.Before(b.StartedAt.Add(-time.Minute)) {
			continue // пара не в продаже или заказ сделан до того, как её выставили
		}
		d, _ := w.pairDesired(sku)
		want[sku] = d
		parts = append(parts, fmt.Sprintf("SKU %d (куплено %d)", sku, q))
	}
	if len(want) == 0 {
		return
	}
	sort.Strings(parts)
	if w.prof.Paused || w.prof.StockOff {
		w.journal.ForeignSet(p.PostingNumber, &FEntry{Action: "replenished", At: now})
		w.log.Add("info", "Нецелевой заказ %s (%s): очередь на паузе — остаток вернётся при «Продолжить».", p.PostingNumber, productsText(p))
		return
	}
	if err := w.setStocks(ctx, want); err != nil {
		w.logOnce("repl|"+p.PostingNumber+"|"+err.Error(), "err", "Нецелевой заказ %s: не удалось пополнить остаток: %v (повторю).", p.PostingNumber, err)
		return
	}
	w.journal.ForeignSet(p.PostingNumber, &FEntry{Action: "replenished", At: now})
	w.log.Add("ok", "Нецелевой заказ %s (%s): остаток пополнен на купленное — %s.", p.PostingNumber, productsText(p), strings.Join(parts, ", "))
}

// cancelForeign отменяет нецелевой заказ и возвращает остаток всех его SKU. false — отменить не вышло.
func (w *Worker) cancelForeign(ctx context.Context, p *Posting, qty map[int64]int, now time.Time) bool {
	var skus []int64
	for sku := range qty {
		skus = append(skus, sku)
	}
	sort.Slice(skus, func(i, j int) bool { return skus[i] < skus[j] })
	avail, err := w.available(ctx, skus)
	if err != nil {
		w.logOnce("cancelstock|"+p.PostingNumber+"|"+err.Error(), "err", "Нецелевой заказ %s: не удалось узнать остаток перед отменой: %v (повторю).", p.PostingNumber, err)
		return true // повторим в следующем цикле, не пополняя
	}
	restore := map[int64]int{} // остаток, будто заказа не было, для SKU вне пар очереди
	want := map[int64]int{}
	for _, sku := range skus {
		if d, ok := w.pairDesired(sku); ok {
			want[sku] = d
		} else {
			restore[sku] = avail[sku] + qty[sku]
			want[sku] = restore[sku]
		}
	}
	id, title := w.cancelReasonID(ctx)
	if !w.prof.Live {
		w.journal.ForeignSet(p.PostingNumber, &FEntry{Action: "cancelled", At: now})
		w.log.Add("test", "[ТЕСТ] Отменил бы нецелевой заказ %s (%s) с причиной «%s» и вернул остаток, будто заказа не было. В Ozon ничего не отправлено.",
			p.PostingNumber, productsText(p), title)
		w.setStocks(ctx, want)
		return true
	}
	if err := w.client.Cancel(ctx, p.PostingNumber, id, "Товар закончился на складе"); err != nil {
		w.log.Add("err", "Нецелевой заказ %s (%s): отменить не удалось: %v. Пополняю остаток вместо отмены.", p.PostingNumber, productsText(p), err)
		return false
	}
	f := &FEntry{Action: "cancelled", At: now, Restore: restore, SKUs: skus, VerifyAt: now.Add(time.Minute), VerifyLeft: 2}
	w.journal.ForeignSet(p.PostingNumber, f)
	w.log.Add("warn", "Нецелевой заказ %s (%s) отменён с причиной «%s».", p.PostingNumber, productsText(p), title)
	if err := w.setStocks(ctx, want); err != nil {
		w.log.Add("err", "Заказ %s отменён, но вернуть остаток не удалось: %v (повторю через минуту).", p.PostingNumber, err)
	} else {
		w.log.Add("ok", "После отмены %s остаток возвращён, будто заказа не было: %s.", p.PostingNumber, stocksText(want))
	}
	return true
}

func stocksText(m map[int64]int) string {
	var keys []int64
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("SKU %d → %d шт.", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

// verifyRestores: Ozon может обнулить остаток после отмены с задержкой — через 1 и 5 минут проверяем
// и, если остаток снова нулевой, выставляем ещё раз.
func (w *Worker) verifyRestores(ctx context.Context, now time.Time) {
	if !w.prof.Live {
		return
	}
	for pn, f := range w.journal.ForeignAll() {
		if f.Action != "cancelled" || f.VerifyLeft <= 0 || now.Before(f.VerifyAt) {
			continue
		}
		avail, err := w.available(ctx, f.SKUs)
		if err != nil {
			return
		}
		want := map[int64]int{}
		for _, sku := range f.SKUs {
			d, ok := w.pairDesired(sku)
			if !ok {
				d = f.Restore[sku]
			}
			if avail[sku] == 0 && d > 0 {
				want[sku] = d
			}
		}
		if len(want) > 0 {
			if err := w.setStocks(ctx, want); err != nil {
				w.log.Add("err", "После отмены %s: не удалось вернуть остаток: %v.", pn, err)
			} else {
				w.log.Add("warn", "После отмены %s Ozon обнулил остаток — вернул снова: %s.", pn, stocksText(want))
			}
		}
		f.VerifyLeft--
		f.VerifyAt = now.Add(4 * time.Minute)
		w.journal.MarkDirty()
	}
}

func foreignNote(f *FEntry) string {
	if f == nil {
		return ""
	}
	switch f.Action {
	case "cancelled":
		return " · отменён программой, остаток возвращён"
	case "replenished":
		return " · остаток пополнен"
	}
	return ""
}
