package main

import (
	"context"
	"testing"
	"time"
)

func newMgr(t *testing.T) (*Manager, *Store, *MockOzon, string) {
	t.Helper()
	dir := t.TempDir()
	store := LoadStore(dir)
	mocks := NewMockRegistry()
	mgr := NewManager(dir, store, mocks)
	t.Cleanup(mgr.StopAll)
	id, _ := store.Snapshot()
	m := mocks.Get(id)
	m.OrderEvery = 0
	store.Update(id, func(p *Profile) { p.Live = true })
	return mgr, store, m, id
}

// «Закончить» → «Бросить сейчас»: работа останавливается, пары в продаже обнуляются, недоставленные заказы бросаются.
func TestFinishAbandon(t *testing.T) {
	mgr, store, m, id := newMgr(t)
	if err := mgr.StartQueue(id, ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { p, _ := store.Get(id); return p.ActiveBatch() != nil })
	m.mu.Lock()
	o := m.AddPosting("target")
	m.mu.Unlock()
	mgr.Kick(id)
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return o.Status == "awaiting_deliver" })

	info, err := mgr.ResetPreview(id)
	if err != nil || len(info.OnSale) == 0 || len(info.Undeliver) != 1 || !info.Zero {
		t.Fatalf("предупреждение: %+v %v", info, err)
	}
	if _, warn, err := mgr.FinishAbandon(context.Background(), id); err != nil || warn != "" {
		t.Fatalf("завершение: %v %s", err, warn)
	}
	p, _ := store.Get(id)
	if mgr.IsRunning(id) || p.Running {
		t.Fatalf("после завершения работа должна быть остановлена")
	}
	if p.Batches[0].State != BatchDone || p.Batches[1].State != BatchSkip || p.Batches[2].State != BatchSkip {
		t.Fatalf("партии: %v", states(store, id))
	}
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU2] != 0 {
		t.Fatalf("остаток пары в продаже не обнулён: %v", m.stock)
	}
	if err := mgr.StartQueue(id, ""); err == nil {
		t.Fatalf("без новых партий начать нельзя")
	}
	// Новая очередь с той же парой: брошенный заказ программа не подхватывает.
	store.Update(id, func(p *Profile) {
		p.Templates = []QueueTemplate{{ID: "t1", Name: "Снова", Rows: []TplRow{{SKU1: mockSKU1, SKU2: mockSKU2, Qty: 5}}}}
	})
	if err := mgr.StartQueue(id, "t1"); err != nil {
		t.Fatal(err)
	}
	p, _ = store.Get(id)
	if len(p.Batches) != 1 || p.QueueName != "Снова" {
		t.Fatalf("очередь из шаблона должна заменить прошлую: %+v", p.Batches)
	}
	time.Sleep(300 * time.Millisecond)
	mgr.StopAll()
	m.mu.Lock()
	st := o.Status
	m.mu.Unlock()
	if st != "awaiting_deliver" {
		t.Fatalf("брошенный заказ трогать нельзя, а статус %s", st)
	}
}

func TestFinishAbandonKeepStockSetting(t *testing.T) {
	mgr, store, m, id := newMgr(t)
	store.Update(id, func(p *Profile) { p.KeepOnReset = true; p.Batches[0].State = BatchActive })
	m.stock[mockSKU1] = 5
	if _, _, err := mgr.FinishAbandon(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if m.stock[mockSKU1] != 5 {
		t.Fatalf("с настройкой «не обнулять» остаток трогать нельзя")
	}
	if p, _ := store.Get(id); !p.Batches[0].Keep {
		t.Fatalf("партия должна запомнить, что остаток не обнуляется (иначе его обнулит проверка остатков)")
	}
}

// «Начать» с шаблоном, когда с прошлого раза осталась пара в продаже: она завершается и обнуляется.
func TestStartTemplateOverActive(t *testing.T) {
	mgr, store, m, id := newMgr(t)
	store.Update(id, func(p *Profile) {
		p.Batches[0].State = BatchActive
		p.Templates = []QueueTemplate{{ID: "t1", Name: "Новая", Rows: []TplRow{{SKU1: mockSKU3, SKU2: mockSKU4, Qty: 2}}}}
	})
	m.stock[mockSKU1], m.stock[mockSKU2] = 4, 4
	if err := mgr.StartQueue(id, "t1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.stock[mockSKU3] == 2 && m.stock[mockSKU1] == 0 })
	if err := mgr.StartQueue(id, "t1"); err == nil {
		t.Fatalf("повторное «Начать» при идущей очереди должно отклоняться")
	}
}

// Пауза: остаток пар в продаже обнуляется, статусы не меняются; «Продолжить» возвращает оставшееся количество.
func TestPauseResume(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	ctx := context.Background()
	w.Tick(ctx)
	o := m.AddPosting("target")
	store.Update(id, func(p *Profile) { p.Paused = true })
	w.Tick(ctx)
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU2] != 0 {
		t.Fatalf("на паузе остаток должен быть обнулён: %v", m.stock)
	}
	if p, _ := store.Get(id); !p.StockOff || p.Batches[0].Sold != 1 {
		t.Fatalf("пауза: %+v", p)
	}
	for i := 0; i < 3; i++ {
		ck.add(61 * time.Second)
		w.Tick(ctx)
	}
	if o.Status != "awaiting_packaging" {
		t.Fatalf("на паузе статусы менять нельзя: %s", o.Status)
	}
	store.Update(id, func(p *Profile) { p.Paused = false })
	w.Tick(ctx)
	if m.stock[mockSKU1] != 4 || m.stock[mockSKU2] != 4 {
		t.Fatalf("после «Продолжить» должен вернуться остаток 5−1=4: %v", m.stock)
	}
	if p, _ := store.Get(id); p.StockOff {
		t.Fatalf("флаг паузы остатка не снят")
	}
	if o.Status != "awaiting_deliver" {
		t.Fatalf("после «Продолжить» заказ должен пойти дальше: %s", o.Status)
	}
}

// Раз в 10 минут: у завершённых пар остаток снова обнуляется, если появился.
func TestDoneStockCheck(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{1, 3, 4}, false)
	ctx := context.Background()
	w.Tick(ctx)
	m.AddPosting("target")
	w.Tick(ctx)
	if st := states(store, id); st[0] != BatchDone {
		t.Fatalf("подготовка: %v", st)
	}
	m.stock[mockSKU1] = 2 // вернулся после отмены
	ck.add(5 * time.Minute)
	w.Tick(ctx)
	if m.stock[mockSKU1] != 2 {
		t.Fatalf("проверка раньше чем через 10 минут")
	}
	ck.add(6 * time.Minute)
	w.Tick(ctx)
	if m.stock[mockSKU1] != 0 {
		t.Fatalf("остаток завершённой пары не обнулён проверкой: %v", m.stock)
	}
	if m.stock[mockSKU3] != 3 {
		t.Fatalf("пару в продаже трогать нельзя: %v", m.stock)
	}
}

// Пара завершённой партии снова в очереди — её остаток проверка не трогает.
func TestDoneStockCheckSkipsReusedPair(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{1, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.Batches[2].SKU1, p.Batches[2].SKU2 = mockSKU1, mockSKU2 })
	ctx := context.Background()
	w.Tick(ctx)
	m.AddPosting("target")
	w.Tick(ctx)
	m.stock[mockSKU1] = 2
	ck.add(11 * time.Minute)
	w.Tick(ctx)
	if m.stock[mockSKU1] != 2 {
		t.Fatalf("пара ещё в очереди — её остаток трогать нельзя")
	}
}

// «Закончить» → «Доставить оставшиеся»: новые партии не запускаются, заказы доводятся, потом остановка.
func TestFinishDeliver(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.Running = true })
	ctx := context.Background()
	w.Tick(ctx)
	o := m.AddPosting("target")
	o.AnalyticsData.DeliveryDateEnd.Time = ck.t.Add(10 * time.Minute)
	w.Tick(ctx)
	store.Update(id, func(p *Profile) { p.Finishing = true })
	w.Tick(ctx)
	if st := states(store, id); st[0] != BatchDone || st[1] != BatchSkip || st[2] != BatchSkip {
		t.Fatalf("партии: %v", st)
	}
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU3] != 0 {
		t.Fatalf("остаток: %v", m.stock)
	}
	if w.completed {
		t.Fatalf("рано: заказ ещё не доставлен")
	}
	run(w, ck, 10)
	if !delivered(o) {
		t.Fatalf("оставшийся заказ должен быть доставлен: %s", o.Status)
	}
	p, _ := store.Get(id)
	if !w.completed || p.Running || p.Finishing {
		t.Fatalf("после доставки очередь должна закончиться: completed=%v running=%v", w.completed, p.Running)
	}
}

// Все партии распроданы и доставлены — очередь выполнена, программа останавливается сама.
func TestQueueCompletes(t *testing.T) {
	w, _, _, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) { p.Running = true })
	done := 0
	w.onDone = func() { done++ }
	for i := 0; i < 4; i++ {
		w.RequestSwitch()
		w.Tick(context.Background())
	}
	p, _ := store.Get(id)
	if done != 1 || p.Running {
		t.Fatalf("очередь должна завершиться: done=%d running=%v states=%v", done, p.Running, states(store, id))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("условие не выполнилось")
}
