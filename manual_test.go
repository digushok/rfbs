package main

import (
	"context"
	"testing"
	"time"
)

// Пока программа работает, «Доставляется» нажали вручную между проверками.
func TestManualStepWhileRunning(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	end := p.AnalyticsData.DeliveryDateEnd.Time
	ctx := context.Background()
	w.Tick(ctx) // программа: «Собрать»
	if p.Status != "awaiting_deliver" {
		t.Fatalf("ожидали сборку, получили %s", p.Status)
	}
	ck.add(20 * time.Second)
	p.Status = "delivering" // вручную, раньше программы
	w.Tick(ctx)
	e := w.journal.Peek(p.PostingNumber)
	if !e.Manual[StageDelivering] || e.Manual[StageShipped] || !e.Picked {
		t.Fatalf("неверные пометки: manual=%v picked=%v", e.Manual, e.Picked)
	}
	if p.Status != "delivering" {
		t.Fatalf("сразу после ручного шага нужно ждать паузу, а статус %s", p.Status)
	}
	ck.add(61 * time.Second)
	w.Tick(ctx)
	if p.Status != "driver_pickup" || e.Manual[StageLastMile] {
		t.Fatalf("ожидали «Курьер в пути» от программы, получили %s (manual=%v)", p.Status, e.Manual)
	}
	ck.t = end.Add(-3 * time.Minute)
	w.Tick(ctx)
	if p.Status != "delivered" || e.Manual[StageDelivered] {
		t.Fatalf("ожидали «Условно доставлен» от программы в срок, получили %s", p.Status)
	}
	v := w.Status().Postings[0]
	if !v.Picked || !v.Manual[StageDelivering] {
		t.Fatalf("в таблице должна быть пометка «вручную»: %+v", v)
	}
}

// Заказ уже «Курьер в пути», когда программу запустили.
func TestPickedBeforeStart(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	p.Status, p.Substatus = "driver_pickup", "posting_driver_pick_up"
	end := p.AnalyticsData.DeliveryDateEnd.Time
	w.Tick(context.Background())
	e := w.journal.Peek(p.PostingNumber)
	if !e.Manual[StageShipped] || !e.Manual[StageDelivering] || !e.Manual[StageLastMile] || e.Stage != StageLastMile {
		t.Fatalf("все этапы до «Курьер в пути» должны быть помечены вручную: %+v", e)
	}
	ck.t = end.Add(-3*time.Minute - time.Second)
	w.Tick(context.Background())
	if p.Status != "driver_pickup" {
		t.Fatalf("рано")
	}
	ck.t = end.Add(-3 * time.Minute)
	w.Tick(context.Background())
	if p.Status != "delivered" {
		t.Fatalf("ожидали «Условно доставлен» за 3 минуты до конца окна, получили %s", p.Status)
	}
}

// «Условно доставлен» нажали вручную раньше программы.
func TestManualDelivered(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		w.Tick(ctx)
		ck.add(61 * time.Second)
	}
	if p.Status != "driver_pickup" {
		t.Fatalf("подготовка: %s", p.Status)
	}
	p.Status, p.Substatus = "delivered", "posting_conditionally_delivered"
	ck.t = p.AnalyticsData.DeliveryDateEnd.Time.Add(-3 * time.Minute) // программа перепроверит заказ к сроку
	w.Tick(ctx)
	e := w.journal.Peek(p.PostingNumber)
	if e.Final == "" || !e.Manual[StageDelivered] || m.Calls["/v2/fbs/posting/delivered"] != 0 {
		t.Fatalf("ожидали «доставлен вручную» без запроса от программы: final=%q manual=%v calls=%d", e.Final, e.Manual, m.Calls["/v2/fbs/posting/delivered"])
	}
}

// Наш запрос ушёл, ответ не дошёл, но Ozon статус сменил — это не «вручную».
func TestLostResponseIsNotManual(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	m.Fail = map[string]bool{"/v4/posting/fbs/ship": true}
	w.Tick(context.Background())
	m.Fail = nil
	p.Status = "awaiting_deliver"
	ck.add(31 * time.Second)
	w.Tick(context.Background())
	e := w.journal.Peek(p.PostingNumber)
	if e.Manual[StageShipped] || e.DoneAt[StageShipped].IsZero() || e.LastError != "" {
		t.Fatalf("сборка должна считаться выполненной программой: %+v", e)
	}
}

// Пара из партии в очереди: тронутый вручную заказ берём (без учёта в «продано»), нетронутый — нет.
func TestQueuedBatchManualOrder(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	ctx := context.Background()
	w.Tick(ctx) // запускается партия 1
	mk := func(status string) *Posting {
		p := m.AddPosting("target")
		p.Products[0].SKU, p.Products[1].SKU = mockSKU5, mockSKU6 // пара из партии 3 (в очереди)
		p.Status = status
		return p
	}
	untouched := mk("awaiting_packaging")
	manual := mk("delivering")
	w.Tick(ctx)
	if untouched.Status != "awaiting_packaging" {
		t.Fatalf("нетронутый заказ пары из очереди трогать нельзя: %s", untouched.Status)
	}
	e := w.journal.Peek(manual.PostingNumber)
	if e == nil || !e.NoCount || !e.Picked {
		t.Fatalf("тронутый вручную заказ должен быть взят вне счёта: %+v", e)
	}
	ck.add(61 * time.Second)
	w.Tick(ctx)
	if manual.Status != "driver_pickup" {
		t.Fatalf("ожидали продолжение цепочки: %s", manual.Status)
	}
	p, _ := store.Get(id)
	if p.Batches[2].Sold != 0 || p.Batches[2].State != BatchQueued || p.Batches[0].Sold != 0 {
		t.Fatalf("счёт партий не должен меняться: %+v", p.Batches)
	}
}
