package main

import (
	"context"
	"testing"
	"time"
)

// Сброс очереди: автоматизация выключается, пары в продаже обнуляются, недоставленные заказы бросаются.
func TestResetQueue(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir)
	mocks := NewMockRegistry()
	mgr := NewManager(dir, store, mocks)
	id, _ := store.Snapshot()
	m := mocks.Get(id)
	m.OrderEvery = 0
	store.Update(id, func(p *Profile) { p.Live = true })
	if err := mgr.Start(id); err != nil {
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
	if _, warn, err := mgr.ResetQueue(context.Background(), id); err != nil || warn != "" {
		t.Fatalf("сброс: %v %s", err, warn)
	}
	p, _ := store.Get(id)
	if mgr.IsRunning(id) || p.Running || len(p.Batches) != 0 {
		t.Fatalf("после сброса автоматизация должна быть выключена, партий нет: running=%v batches=%d", mgr.IsRunning(id), len(p.Batches))
	}
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU2] != 0 {
		t.Fatalf("остаток пары в продаже не обнулён: %v", m.stock)
	}
	if err := mgr.Start(id); err == nil {
		t.Fatalf("без партий запуск должен быть невозможен")
	}
	// Новая очередь с той же парой: брошенный заказ программа не подхватывает.
	store.Update(id, func(p *Profile) {
		p.Batches = []Batch{{SKU1: mockSKU1, SKU2: mockSKU2, Qty: 5}}
	})
	if err := mgr.Start(id); err != nil {
		t.Fatal(err)
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

func TestResetKeepStockSetting(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir)
	mocks := NewMockRegistry()
	mgr := NewManager(dir, store, mocks)
	id, _ := store.Snapshot()
	m := mocks.Get(id)
	m.OrderEvery = 0
	store.Update(id, func(p *Profile) { p.Live, p.KeepOnReset = true, true; p.Batches[0].State = BatchActive })
	m.stock[mockSKU1] = 5
	if _, _, err := mgr.ResetQueue(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if m.stock[mockSKU1] != 5 {
		t.Fatalf("с настройкой «не обнулять» остаток трогать нельзя")
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
