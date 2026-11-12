package main

import (
	"context"
	"testing"
	"time"
)

// Нецелевой заказ со SKU пары в продаже: остаток возвращается (будто заказа не было), один раз.
func TestForeignReplenish(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	ctx := context.Background()
	w.Tick(ctx)
	ck.add(time.Second)
	m.stock[mockSKUOther] = 7
	o := m.AddPosting("double") // SKU1×2 + SKU2×1
	o2 := m.AddPosting("other") // SKU1×1 + другой товар
	if m.stock[mockSKU1] != 2 || m.stock[mockSKU2] != 4 || m.stock[mockSKUOther] != 6 {
		t.Fatalf("подготовка: %v", m.stock)
	}
	w.Tick(ctx)
	if m.stock[mockSKU1] != 5 || m.stock[mockSKU2] != 5 {
		t.Fatalf("остаток пары должен вернуться к 5: %v", m.stock)
	}
	if m.stock[mockSKUOther] != 6 {
		t.Fatalf("другой товар не из очереди пополнять нельзя: %v", m.stock)
	}
	if o.Status != "awaiting_packaging" || o2.Status != "awaiting_packaging" || len(m.Cancelled) != 0 {
		t.Fatalf("без настройки отмены заказы не трогаем")
	}
	if p, _ := store.Get(id); p.Batches[0].Sold != 0 {
		t.Fatalf("нецелевой заказ не считается в «продано»")
	}
	calls := m.Calls["/v2/products/stocks"]
	w.Tick(ctx)
	if m.Calls["/v2/products/stocks"] != calls {
		t.Fatalf("пополнение повторилось")
	}
}

// Пара следующей партии (выставлена заранее) тоже пополняется; пара в очереди — нет.
func TestForeignReplenishNextNotQueued(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{3, 4, 5}, false)
	setPrestock(store, id, 3)
	ctx := context.Background()
	w.Tick(ctx) // партия 1 основная, партия 2 выставлена заранее
	if st := states(store, id); st[1] != BatchNext {
		t.Fatalf("подготовка: %v", st)
	}
	ck.add(time.Second)
	p := m.AddPosting("single")
	p.Products[0].SKU = mockSKU3
	m.stock[mockSKU1]++
	m.stock[mockSKU3]--
	q := m.AddPosting("single")
	q.Products[0].SKU = mockSKU5
	m.stock[mockSKU1]++
	m.stock[mockSKU5] = 9
	w.Tick(ctx)
	if m.stock[mockSKU3] != 4 {
		t.Fatalf("пара «заранее» должна пополниться: %v", m.stock)
	}
	if m.stock[mockSKU5] != 9 {
		t.Fatalf("пару из очереди пополнять нельзя: %v", m.stock)
	}
}

// Отмена нецелевого заказа: причина «товар закончился», остаток всех SKU — будто заказа не было.
func TestForeignCancel(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.CancelForeign = true })
	ctx := context.Background()
	w.Tick(ctx)
	ck.add(time.Second)
	m.stock[mockSKUOther] = 7
	o := m.AddPosting("other") // SKU1×1 + другой товар×1
	shipped := m.AddPosting("single")
	shipped.Status = "awaiting_deliver" // уже нажали «Собрать» — всё равно отменяем
	late := m.AddPosting("single")
	late.Status = "delivering" // дальше — не отменяем, только пополняем
	w.Tick(ctx)
	if o.Status != "cancelled" || shipped.Status != "cancelled" || m.CancelReason != 352 {
		t.Fatalf("заказы должны быть отменены с причиной 352: %s %s, причина %d", o.Status, shipped.Status, m.CancelReason)
	}
	if late.Status != "delivering" {
		t.Fatalf("заказ дальше «Ожидает отгрузки» отменять нельзя: %s", late.Status)
	}
	if m.stock[mockSKU1] != 5 || m.stock[mockSKU2] != 5 {
		t.Fatalf("остаток пары после отмены — будто заказов не было (5): %v", m.stock)
	}
	if m.stock[mockSKUOther] != 7 {
		t.Fatalf("остаток другого товара после отмены — будто заказа не было (7): %v", m.stock)
	}
	// Ozon обнулил остаток с задержкой — программа вернёт его при проверке.
	m.stock[mockSKUOther] = 0
	ck.add(61 * time.Second)
	w.Tick(ctx)
	if m.stock[mockSKUOther] != 7 {
		t.Fatalf("после запоздалого обнуления остаток должен вернуться: %v", m.stock)
	}
}

// Заказ, сделанный до начала очереди, не отменяется.
func TestForeignCancelOnlyAfterQueueStart(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	old := m.AddPosting("other")
	ck.add(time.Minute)
	store.Update(id, func(p *Profile) { p.CancelForeign = true; p.QueueStarted = ck.t })
	w.Tick(context.Background())
	if old.Status != "awaiting_packaging" {
		t.Fatalf("заказ до начала очереди отменять нельзя")
	}
}

func TestForeignCancelTestMode(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, false, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.CancelForeign = true })
	m.stock[mockSKU1], m.stock[mockSKU2] = 5, 5
	w.Tick(context.Background())
	ck.add(time.Second)
	o := m.AddPosting("other")
	w.Tick(context.Background())
	if o.Status != "awaiting_packaging" || len(m.Cancelled) != 0 {
		t.Fatalf("в тесте отменять по-настоящему нельзя")
	}
	if w.journal.ForeignGet(o.PostingNumber) == nil {
		t.Fatalf("тестовая отмена должна запомниться, чтобы не повторяться")
	}
}

// Частота проверки остатков завершённых пар — из настроек.
func TestDoneStockCheckInterval(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{1, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.StockCheckMin = 2 })
	ctx := context.Background()
	w.Tick(ctx)
	w.RequestSwitch()
	w.Tick(ctx)
	m.stock[mockSKU1] = 2
	ck.add(2*time.Minute + time.Second)
	w.Tick(ctx)
	if m.stock[mockSKU1] != 0 {
		t.Fatalf("проверка раз в 2 минуты не сработала: %v", m.stock)
	}
}

// Перед завершением очереди — последняя проверка остатков всех пар, даже если интервал не прошёл.
func TestFinalStockCheck(t *testing.T) {
	w, m, _, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.Running = true; p.StockCheckMin = 60 })
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		w.RequestSwitch()
		w.Tick(ctx)
	}
	m.stock[mockSKU1] = 3 // у первой пары остаток вернулся
	w.RequestSwitch()
	w.Tick(ctx)
	if !w.completed || m.stock[mockSKU1] != 0 {
		t.Fatalf("очередь должна завершиться после финальной проверки: completed=%v stock=%v", w.completed, m.stock)
	}
}
