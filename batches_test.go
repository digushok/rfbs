package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// Магазин с тремя партиями; работник читает партии из настроек (как в программе).
func setupBatches(t *testing.T, live bool, qty [3]int, keep bool) (*Worker, *MockOzon, *clock, *Store, string) {
	t.Helper()
	ck := &clock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	m := NewMockOzon(ck.now)
	m.OrderEvery = 0
	m.stock[mockSKU1], m.stock[mockSKU2] = 0, 0
	dir := t.TempDir()
	store := LoadStore(dir)
	id, _ := store.Snapshot()
	prof, _ := store.Update(id, func(p *Profile) {
		p.Live, p.KeepOldStock, p.MarginMin = live, keep, 3
		for i := range p.Batches {
			p.Batches[i].Qty = qty[i]
		}
	})
	c := &Client{base: "http://demo", http: &http.Client{Transport: m}}
	w := NewWorker(prof, c, LoadJournal(filepath.Join(dir, "j.json")), NewShopLog(dir, "b"))
	w.store = store
	w.clock = ck.now
	return w, m, ck, store, id
}

func states(store *Store, id string) []string {
	p, _ := store.Get(id)
	var s []string
	for _, b := range p.Batches {
		s = append(s, b.State)
	}
	return s
}

func run(w *Worker, ck *clock, n int) {
	for i := 0; i < n; i++ {
		w.Tick(context.Background())
		ck.add(61 * time.Second)
	}
}

func TestBatchAutoSwitch(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, true, [3]int{2, 3, 4}, false)
	w.Tick(context.Background())
	if st := states(store, id); st[0] != BatchActive || st[1] != BatchQueued {
		t.Fatalf("первая партия должна запуститься: %v", st)
	}
	if m.stock[mockSKU1] != 2 || m.stock[mockSKU2] != 2 {
		t.Fatalf("остаток первой партии не выставлен: %v", m.stock)
	}
	// Раскупили первую партию.
	o1, o2 := m.AddPosting("target"), m.AddPosting("target")
	w.Tick(context.Background())
	if st := states(store, id); st[0] != BatchDone || st[1] != BatchActive {
		t.Fatalf("после распродажи должна запуститься вторая партия: %v", st)
	}
	if m.stock[mockSKU3] != 3 || m.stock[mockSKU4] != 3 {
		t.Fatalf("остаток второй партии не выставлен: %v", m.stock)
	}
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU2] != 0 {
		t.Fatalf("остаток первой пары не обнулён: %v", m.stock)
	}
	// Заказы второй партии — целевые; заказы первой продолжают путь.
	o3 := m.AddPosting("target")
	if o3.Products[0].SKU != mockSKU3 {
		t.Fatalf("демо должно заказывать вторую пару")
	}
	// Поздний заказ первой пары (например, после отмены вернулся остаток) — тоже обрабатывается.
	m.stock[mockSKU3], m.stock[mockSKU4] = 0, 0
	late := m.AddPosting("target")
	m.stock[mockSKU3], m.stock[mockSKU4] = 2, 2
	if late.Products[0].SKU != mockSKU1 {
		t.Fatalf("ожидали поздний заказ первой пары")
	}
	for _, p := range []*Posting{o1, o2, o3, late} {
		p.AnalyticsData.DeliveryDateEnd.Time = ck.t.Add(10 * time.Minute)
	}
	run(w, ck, 15)
	for _, p := range []*Posting{o1, o2, o3, late} {
		if p.Status != "delivered" {
			t.Fatalf("заказ %s (%s) не доведён: %s", p.PostingNumber, productsText(p), p.Status)
		}
	}
	p, _ := store.Get(id)
	if p.Batches[0].Sold != 3 || p.Batches[1].Sold != 1 {
		t.Fatalf("неверный счёт продаж: %d, %d", p.Batches[0].Sold, p.Batches[1].Sold)
	}
}

func TestBatchKeepOldStock(t *testing.T) {
	w, m, _, store, id := setupBatches(t, true, [3]int{1, 3, 4}, true)
	w.Tick(context.Background())
	m.stock[mockSKU1], m.stock[mockSKU2] = 5, 5 // остаток больше, чем количество партии
	m.AddPosting("target")
	w.Tick(context.Background())
	if st := states(store, id); st[0] != BatchDone || st[1] != BatchActive {
		t.Fatalf("ожидали переключение: %v", st)
	}
	if m.stock[mockSKU1] != 4 {
		t.Fatalf("с настройкой «не обнулять» остаток старой пары трогать нельзя: %d", m.stock[mockSKU1])
	}
}

func TestBatchManualSwitch(t *testing.T) {
	w, m, _, store, id := setupBatches(t, true, [3]int{10, 3, 4}, false)
	w.Tick(context.Background())
	w.RequestSwitch()
	w.Tick(context.Background())
	if st := states(store, id); st[0] != BatchDone || st[1] != BatchActive {
		t.Fatalf("ручное переключение не сработало: %v", st)
	}
	if m.stock[mockSKU1] != 0 || m.stock[mockSKU3] != 3 {
		t.Fatalf("остатки после ручного переключения: %v", m.stock)
	}
	// Последняя партия → очередь пуста, активной нет, но ничего не ломается.
	w.RequestSwitch()
	w.Tick(context.Background())
	w.RequestSwitch()
	w.Tick(context.Background())
	if st := states(store, id); st[2] != BatchDone {
		t.Fatalf("ожидали все партии завершёнными: %v", st)
	}
}

func TestBatchTestModeNotPersisted(t *testing.T) {
	w, m, ck, store, id := setupBatches(t, false, [3]int{1, 3, 4}, false)
	m.stock[mockSKU1], m.stock[mockSKU2] = 1, 1 // в тесте остаток не выставляется — задаём сами
	w.Tick(context.Background())
	m.AddPosting("target")
	run(w, ck, 2)
	st := w.Status()
	if st.Batches[0].State != BatchDone || st.Batches[1].State != BatchActive {
		t.Fatalf("в тесте переключение должно симулироваться: %+v", st.Batches)
	}
	if s := states(store, id); s[0] != BatchQueued || s[1] != BatchQueued {
		t.Fatalf("тестовый режим не должен менять настройки партий: %v", s)
	}
	if m.stock[mockSKU3] != 0 {
		t.Fatalf("тестовый режим изменил остаток")
	}
}

func TestBatchMigrationFromSinglePair(t *testing.T) {
	p := &Profile{SKU1: 1, SKU2: 2, StockQty: 25}
	p.normalize()
	if len(p.Batches) != 1 || p.Batches[0].State != BatchActive || p.Batches[0].Qty != 25 || p.SKU1 != 0 {
		t.Fatalf("старые настройки не перенесены: %+v", p)
	}
}
