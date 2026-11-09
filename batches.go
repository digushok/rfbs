package main

// Партии: очередь пар SKU. Активная партия продаётся; когда её количество распродано
// (или нажата кнопка «Следующая партия»), старая пара обнуляется (если включено в настройках),
// следующей паре выставляется её количество. Заказы всех начатых партий доводятся до конца.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type BatchView struct {
	Batch
	Num       int `json:"num"`
	InWork    int `json:"in_work"`   // в работе: собраны/в доставке
	Delivered int `json:"delivered"` // условно доставлены (за последние 7 дней)
}

// loadBatches берёт партии из настроек (их можно менять на ходу), в тестовом режиме —
// поверх накладывает симулированное состояние.
func (w *Worker) loadBatches() {
	var base []Batch
	if w.store != nil {
		if p, ok := w.store.Get(w.prof.ID); ok {
			base = p.Batches
			w.prof.KeepOldStock = p.KeepOldStock
			w.prof.PrestockLeft = p.PrestockLeft
		}
	} else {
		base = w.prof.Batches
	}
	bt := make([]Batch, len(base))
	copy(bt, base)
	if !w.prof.Live {
		for i := range bt {
			if s, ok := w.sim[bt[i].ID]; ok {
				bt[i].State, bt[i].Sold, bt[i].StartedAt, bt[i].FinishedAt, bt[i].ZeroDone = s.State, s.Sold, s.StartedAt, s.FinishedAt, s.ZeroDone
			}
		}
	}
	w.bt = bt
	w.prof.Batches = bt
}

func (w *Worker) batch(id string) *Batch {
	for i := range w.bt {
		if w.bt[i].ID == id {
			return &w.bt[i]
		}
	}
	return nil
}

func (w *Worker) addSold(id string, d int) {
	if b := w.batch(id); b != nil {
		b.Sold += d
		if b.Sold < 0 {
			b.Sold = 0
		}
		w.btDirty[id] = true
	}
}

// flushBatches сохраняет изменения состояния партий: в настройки (боевой режим) или в симуляцию (тест).
func (w *Worker) flushBatches() {
	if len(w.btDirty) == 0 {
		return
	}
	changed := map[string]Batch{}
	for id := range w.btDirty {
		if b := w.batch(id); b != nil {
			changed[id] = *b
		}
	}
	w.btDirty = map[string]bool{}
	if !w.prof.Live {
		for id, b := range changed {
			w.sim[id] = b
		}
		return
	}
	apply := func(list []Batch) {
		for i := range list {
			if b, ok := changed[list[i].ID]; ok {
				// Только состояние: SKU и количество в очереди пользователь мог поменять.
				list[i].State, list[i].Sold, list[i].StartedAt, list[i].FinishedAt, list[i].ZeroDone = b.State, b.Sold, b.StartedAt, b.FinishedAt, b.ZeroDone
			}
		}
	}
	if w.store != nil {
		w.store.Update(w.prof.ID, func(p *Profile) { apply(p.Batches) })
	}
}

func batchName(num int, b *Batch) string {
	return fmt.Sprintf("Партия %d (SKU %d + %d)", num, b.SKU1, b.SKU2)
}

func (w *Worker) num(id string) int {
	for i := range w.bt {
		if w.bt[i].ID == id {
			return i + 1
		}
	}
	return 0
}

// RequestSwitch — кнопка «Следующая партия»: выполнится в ближайшем цикле.
func (w *Worker) RequestSwitch() {
	w.mu.Lock()
	w.switchReq = true
	w.mu.Unlock()
	w.Kick()
}

func (w *Worker) manageBatches(ctx context.Context, now time.Time) {
	w.mu.Lock()
	manual := w.switchReq
	w.switchReq = false
	w.mu.Unlock()

	var active, early *Batch
	for i := range w.bt {
		switch w.bt[i].State {
		case BatchActive:
			if active == nil {
				active = &w.bt[i]
			}
		case BatchNext:
			if early == nil {
				early = &w.bt[i]
			}
		}
	}
	// 1. Завершаем активную партию: распродана или переключение вручную.
	if active != nil && (manual || (active.Qty > 0 && active.Sold >= active.Qty)) {
		n := w.num(active.ID)
		if manual {
			w.log.Add("info", "%s завершена вручную (продано %d из %d).", batchName(n, active), active.Sold, active.Qty)
		} else {
			w.log.Add("ok", "%s распродана: %d из %d. Её заказы будут доведены до «Условно доставлен».", batchName(n, active), active.Sold, active.Qty)
		}
		active.State, active.FinishedAt = BatchDone, now
		w.btDirty[active.ID] = true
		active = nil
	} else if manual && active == nil {
		w.log.Add("info", "Нет активной партии — запускаю следующую из очереди.")
	}

	// 2. Обнуляем остаток завершённых партий (если включено).
	for i := range w.bt {
		b := &w.bt[i]
		if b.State != BatchDone || b.ZeroDone {
			continue
		}
		if w.prof.KeepOldStock {
			b.ZeroDone = true
			w.btDirty[b.ID] = true
			continue
		}
		if err := w.setPairStock(ctx, b, 0); err != nil {
			w.logOnce("zero|"+b.ID+"|"+err.Error(), "err", "%s: не удалось обнулить остаток: %v (повторю).", batchName(i+1, b), err)
			continue
		}
		b.ZeroDone = true
		w.btDirty[b.ID] = true
	}

	// 3. Нет основной партии: следующая, выставленная заранее, становится основной.
	if active == nil && early != nil {
		early.State = BatchActive
		w.btDirty[early.ID] = true
		w.log.Add("ok", "%s стала основной (продано %d из %d).", batchName(w.num(early.ID), early), early.Sold, early.Qty)
		active, early = early, nil
	}
	// 4. Выставляем следующую из очереди: сразу, если основной нет, или заранее —
	// когда до конца основной осталось не больше порога. Заранее — не больше одной партии.
	prestock := w.prof.Prestock()
	ahead := false
	switch {
	case active == nil:
	case early == nil && prestock > 0 && active.Qty-active.Sold <= prestock:
		ahead = true
	default:
		return
	}
	var next *Batch
	for i := range w.bt {
		if w.bt[i].State == BatchQueued {
			next = &w.bt[i]
			break
		}
	}
	if next == nil {
		if len(w.bt) > 0 {
			last := w.bt[len(w.bt)-1].ID
			w.logOnce("noqueue|"+last, "warn", "Партии в очереди закончились — добавьте следующую пару SKU. Заказы начатых партий продолжают обрабатываться.")
		}
		return
	}
	if now.Before(w.activateAt) {
		return
	}
	n := w.num(next.ID)
	if err := next.Validate(); err != nil {
		w.logOnce("bad|"+next.ID, "err", "%s: %v — исправьте партию.", batchName(n, next), err)
		return
	}
	if err := w.setPairStock(ctx, next, next.Qty); err != nil {
		w.activateAt = now.Add(time.Minute)
		w.log.Add("err", "%s: не удалось выставить остаток %d шт.: %v (повторю через минуту).", batchName(n, next), next.Qty, err)
		return
	}
	next.StartedAt, next.FinishedAt = now, time.Time{}
	w.btDirty[next.ID] = true
	if ahead {
		next.State = BatchNext
		w.log.Add("ok", "%s выставлена заранее: по %d шт. (в партии %d осталось %d из %d).", batchName(n, next), next.Qty,
			w.num(active.ID), active.Qty-active.Sold, active.Qty)
		return
	}
	next.State = BatchActive
	w.log.Add("ok", "%s запущена: выставлено по %d шт.", batchName(n, next), next.Qty)
	// Маленькая партия: порог достигнут сразу — выставляем и следующую.
	if prestock > 0 && next.Qty-next.Sold <= prestock {
		w.manageAhead(ctx, now, next)
	}
}

// manageAhead выставляет заранее одну партию из очереди после основной.
func (w *Worker) manageAhead(ctx context.Context, now time.Time, active *Batch) {
	var next *Batch
	for i := range w.bt {
		if w.bt[i].State == BatchQueued {
			next = &w.bt[i]
			break
		}
	}
	if next == nil || next.Validate() != nil {
		return
	}
	n := w.num(next.ID)
	if err := w.setPairStock(ctx, next, next.Qty); err != nil {
		w.activateAt = now.Add(time.Minute)
		w.log.Add("err", "%s: не удалось выставить остаток %d шт.: %v (повторю через минуту).", batchName(n, next), next.Qty, err)
		return
	}
	next.State, next.StartedAt = BatchNext, now
	w.btDirty[next.ID] = true
	w.log.Add("ok", "%s выставлена заранее: по %d шт. (в партии %d осталось %d из %d).", batchName(n, next), next.Qty, w.num(active.ID), active.Qty-active.Sold, active.Qty)
}

// setPairStock выставляет обоим SKU пары одинаковый остаток (в тесте — только пишет в журнал).
func (w *Worker) setPairStock(ctx context.Context, b *Batch, qty int) error {
	if !w.prof.Live {
		w.log.Add("test", "[ТЕСТ] Выставил бы остаток %d шт. для SKU %d и %d на складе %d. В Ozon ничего не отправлено.", qty, b.SKU1, b.SKU2, w.prof.WarehouseID)
		return nil
	}
	var missing []int64
	for _, sku := range []int64{b.SKU1, b.SKU2} {
		if w.productInfo[sku] == nil {
			missing = append(missing, sku)
		}
	}
	if len(missing) > 0 {
		infos, err := w.client.ProductsBySKU(ctx, missing)
		if err != nil {
			return err
		}
		for k, v := range infos {
			w.productInfo[k] = v
		}
	}
	var errs []string
	for _, sku := range []int64{b.SKU1, b.SKU2} {
		pi := w.productInfo[sku]
		if pi == nil {
			errs = append(errs, fmt.Sprintf("товар с SKU %d не найден в магазине", sku))
			continue
		}
		if err := w.client.SetStock(ctx, w.prof.WarehouseID, pi, qty); err != nil {
			errs = append(errs, fmt.Sprintf("SKU %d: %v", sku, err))
			continue
		}
		w.log.Add("ok", "Остаток SKU %d (артикул «%s») на складе %d: %d шт.", sku, pi.OfferID, w.prof.WarehouseID, qty)
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

func (w *Worker) batchViews() []BatchView {
	inWork, delivered := map[string]int{}, map[string]int{}
	for _, e := range w.journal.All() {
		if e.Batch == "" {
			continue
		}
		switch {
		case e.Final == "" && e.Stage >= 0:
			inWork[e.Batch]++
		case strings.Contains(e.Final, "доставлен"):
			delivered[e.Batch]++
		}
	}
	out := make([]BatchView, len(w.bt))
	for i, b := range w.bt {
		out[i] = BatchView{Batch: b, Num: i + 1, InWork: inWork[b.ID], Delivered: delivered[b.ID]}
	}
	return out
}
