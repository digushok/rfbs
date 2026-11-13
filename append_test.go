package main

import (
	"context"
	"testing"
)

// «+ Добавить очередь», когда все пары уже распроданы (заказы доезжают): первая новая пара выставляется сразу.
func TestAppendWhenNothingOnSale(t *testing.T) {
	w, m, _, store, id := setupBatches(t, true, [3]int{5, 3, 4}, false)
	store.Update(id, func(p *Profile) {
		p.Running = true
		for i := range p.Batches {
			p.Batches[i].State, p.Batches[i].ZeroDone = BatchDone, true
		}
		p.Batches = append(p.Batches, Batch{ID: "new1", SKU1: mockSKU3, SKU2: mockSKU4, Qty: 6, State: BatchQueued})
	})
	w.Tick(context.Background())
	if st := states(store, id); st[3] != BatchActive {
		t.Fatalf("новая пара должна сразу встать в продажу: %v", st)
	}
	if m.stock[mockSKU3] != 6 || m.stock[mockSKU4] != 6 {
		t.Fatalf("остаток новой пары: %v", m.stock)
	}
}

// Пока до конца пары в продаже больше заказов, чем порог из настроек, новая пара ждёт; на пороге — выставляется заранее.
func TestAppendWaitsForPrestock(t *testing.T) {
	w, m, _, store, id := setupBatches(t, true, [3]int{10, 3, 4}, false)
	three := 3
	store.Update(id, func(p *Profile) {
		p.Running, p.PrestockLeft = true, &three
		p.Batches = p.Batches[:1]
		p.Batches[0].State, p.Batches[0].Sold = BatchActive, 2
		p.Batches = append(p.Batches, Batch{ID: "new1", SKU1: mockSKU3, SKU2: mockSKU4, Qty: 6, State: BatchQueued})
	})
	w.Tick(context.Background())
	if st := states(store, id); st[1] != BatchQueued || m.stock[mockSKU3] != 0 {
		t.Fatalf("осталось 8 из 10 — новая пара должна ждать: %v %v", st, m.stock)
	}
	store.Update(id, func(p *Profile) { p.Batches[0].Sold = 7 })
	w.Tick(context.Background())
	if st := states(store, id); st[1] != BatchNext || m.stock[mockSKU3] != 6 {
		t.Fatalf("осталось 3 — новая пара выставляется заранее: %v %v", st, m.stock)
	}
}

// Очередь закончилась и программа остановлена: «+ Добавить очередь» продолжает её — старые пары остаются, номера идут дальше.
func TestAppendQueueAfterComplete(t *testing.T) {
	mgr, store, m, id := newMgr(t)
	store.Update(id, func(p *Profile) {
		for i := range p.Batches {
			p.Batches[i].State, p.Batches[i].ZeroDone = BatchDone, true
		}
		p.QueueName = "Первая"
		p.Templates = []QueueTemplate{{ID: "t1", Name: "Вторая", Rows: []TplRow{{SKU1: mockSKU3, SKU2: mockSKU4, Qty: 2}, {SKU1: mockSKU1, SKU2: mockSKU2, Qty: 3}}}}
	})
	if _, err := mgr.AppendQueue(id, "", ""); err == nil {
		t.Fatalf("без новых партий продолжать нечего")
	}
	if n, err := mgr.AppendQueue(id, "t1", ""); err != nil || n != 2 {
		t.Fatalf("добавление: %d %v", n, err)
	}
	if !mgr.IsRunning(id) {
		t.Fatalf("очередь должна продолжиться")
	}
	waitFor(t, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.stock[mockSKU3] == 2 })
	p, _ := store.Get(id)
	if len(p.Batches) != 5 || p.Batches[0].State != BatchDone || p.QueueName != "Первая + Вторая" {
		t.Fatalf("партии: %v, имя %q", states(store, id), p.QueueName)
	}
	if err := mgr.ClearQueue(id); err == nil {
		t.Fatalf("очищать идущую очередь нельзя")
	}
	// Пока идёт — ещё одна очередь встаёт в конец.
	if n, err := mgr.AppendQueue(id, "t1", ""); err != nil || n != 2 {
		t.Fatalf("добавление на ходу: %d %v", n, err)
	}
	p, _ = store.Get(id)
	if len(p.Batches) != 7 || p.Batches[5].State != BatchQueued || p.Batches[6].State != BatchQueued {
		t.Fatalf("на ходу: %v", states(store, id))
	}
	mgr.StopAll()
	store.Update(id, func(p *Profile) { p.Running = false })
	mgr.Stop(id, true)
	if err := mgr.ClearQueue(id); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.Get(id); len(p.Batches) != 0 || p.QueueName != "" {
		t.Fatalf("после очистки: %v", p.Batches)
	}
}

// Таблицу заполнили сохранённой очередью и нажали «Начать» / «Добавить очередь»: имя очереди сохраняется.
func TestQueueNameFromFilledTable(t *testing.T) {
	mgr, store, _, id := newMgr(t)
	if err := mgr.StartQueueNamed(id, "", "Первая"); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.Get(id); p.QueueName != "Первая" {
		t.Fatalf("имя: %q", p.QueueName)
	}
	mgr.Stop(id, true)
	store.Update(id, func(p *Profile) {
		for i := range p.Batches {
			p.Batches[i].State, p.Batches[i].ZeroDone = BatchDone, true
		}
		p.Batches = append(p.Batches, Batch{ID: "n1", SKU1: mockSKU3, SKU2: mockSKU4, Qty: 2, State: BatchQueued})
	})
	if _, err := mgr.AppendQueue(id, "", "Вторая"); err != nil {
		t.Fatal(err)
	}
	if p, _ := store.Get(id); p.QueueName != "Первая + Вторая" || !mgr.IsRunning(id) {
		t.Fatalf("имя: %q, идёт: %v", p.QueueName, mgr.IsRunning(id))
	}
}
