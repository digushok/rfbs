package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, live bool) (*Worker, *MockOzon, *clock) {
	t.Helper()
	ck := &clock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	m := NewMockOzon(ck.now)
	m.OrderEvery = 0
	prof := Profile{ID: "t", Demo: true, WarehouseID: mockWarehouseID, SKU1: mockSKU1, SKU2: mockSKU2, Live: live}
	prof.normalize()
	prof.MarginMin = 3
	c := &Client{base: "http://demo", http: &http.Client{Transport: m}}
	dir := t.TempDir()
	w := NewWorker(prof, c, LoadJournal(filepath.Join(dir, "j.json")), NewShopLog(dir, "t"))
	w.now = ck.now
	return w, m, ck
}

func TestLiveFullChain(t *testing.T) {
	w, m, ck := setup(t, true)
	target := m.AddPosting("target")
	other := m.AddPosting("other")
	single := m.AddPosting("single")
	double := m.AddPosting("double")
	otherWh := m.AddPosting("other_wh")
	end := target.AnalyticsData.DeliveryDateEnd.Time
	ctx := context.Background()

	w.Tick(ctx)
	if target.Status != "awaiting_deliver" {
		t.Fatalf("после первого цикла ожидали awaiting_deliver, получили %s", target.Status)
	}
	// Пауза ещё не прошла — ничего не меняется.
	ck.add(30 * time.Second)
	w.Tick(ctx)
	if target.Status != "awaiting_deliver" {
		t.Fatalf("до паузы статус не должен меняться: %s", target.Status)
	}
	ck.add(31 * time.Second)
	w.Tick(ctx)
	if target.Status != "delivering" {
		t.Fatalf("ожидали delivering, получили %s", target.Status)
	}
	ck.add(61 * time.Second)
	next := w.Tick(ctx)
	if target.Status != "driver_pickup" {
		t.Fatalf("ожидали «курьер в пути», получили %s", target.Status)
	}
	want := end.Add(-3 * time.Minute)
	if !next.Equal(want) {
		t.Fatalf("«условно доставлен» должен быть запланирован на %v, а запланирован на %v", want, next)
	}
	// За минуту до плана — ещё рано.
	ck.t = want.Add(-time.Minute)
	w.Tick(ctx)
	if target.Status != "driver_pickup" {
		t.Fatalf("слишком рано перевели в доставлен")
	}
	ck.t = want
	w.Tick(ctx)
	if target.Status != "delivered" || target.Substatus != "posting_conditionally_delivered" {
		t.Fatalf("ожидали условно доставлен, получили %s/%s", target.Status, target.Substatus)
	}
	for _, p := range []*Posting{other, single, double, otherWh} {
		if p.Status != "awaiting_packaging" {
			t.Fatalf("нецелевой заказ %s изменён: %s", p.PostingNumber, p.Status)
		}
	}
	st := w.Status()
	if len(st.Postings) != 1 || st.Postings[0].Final == "" {
		t.Fatalf("ожидали 1 завершённый целевой заказ в списке: %+v", st.Postings)
	}
	if len(st.Skipped) != 3 { // other_wh отфильтрован запросом по складу
		t.Fatalf("ожидали 3 пропущенных, получили %d", len(st.Skipped))
	}
}

func TestTestModeChangesNothing(t *testing.T) {
	w, m, ck := setup(t, false)
	target := m.AddPosting("target")
	end := target.AnalyticsData.DeliveryDateEnd.Time
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		w.Tick(ctx)
		ck.add(61 * time.Second)
	}
	if target.Status != "awaiting_packaging" {
		t.Fatalf("тестовый режим изменил статус в Ozon: %s", target.Status)
	}
	e := w.journal.Get(target.PostingNumber)
	if e.Stage != StageDelivered {
		t.Fatalf("симуляция должна дойти до конца, этап %d", e.Stage)
	}
	if got, want := e.DoneAt[StageDelivered], end.Add(-3*time.Minute); got.Before(want) || got.Sub(want) > 70*time.Second {
		t.Fatalf("симулированная доставка в %v, ожидали около %v", got, want)
	}
	// Изменяющие запросы заблокированы на уровне клиента.
	if err := w.client.Ship(ctx, target); err != ErrReadOnly {
		t.Fatalf("ожидали блокировку запроса, получили %v", err)
	}
}

func TestOverdueDeliveredImmediately(t *testing.T) {
	w, m, ck := setup(t, true)
	target := m.AddPosting("target")
	ctx := context.Background()
	ck.add(time.Hour) // окно уже прошло
	for i := 0; i < 4; i++ {
		w.Tick(ctx)
		ck.add(61 * time.Second)
	}
	if target.Status != "delivered" {
		t.Fatalf("ожидали delivered, получили %s", target.Status)
	}
}

func TestNoWindowNoDelivered(t *testing.T) {
	w, m, ck := setup(t, true)
	target := m.AddPosting("target")
	target.AnalyticsData = nil
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		w.Tick(ctx)
		ck.add(61 * time.Second)
	}
	if target.Status != "driver_pickup" {
		t.Fatalf("без окна доставки должны остановиться на «курьер в пути», получили %s", target.Status)
	}
}

func TestResumeFromOzonStatus(t *testing.T) {
	// Заказ уже «доставляется» (например, после переустановки программы без журнала).
	w, m, ck := setup(t, true)
	target := m.AddPosting("target")
	target.Status = "delivering"
	w.Tick(context.Background())
	if target.Status != "driver_pickup" {
		t.Fatalf("ожидали продолжение цепочки, получили %s", target.Status)
	}
	_ = ck
}

func TestCheckTarget(t *testing.T) {
	prof := &Profile{WarehouseID: 1, SKU1: 10, SKU2: 20}
	mk := func(wh int64, prods ...PostingProduct) *Posting {
		p := &Posting{Products: prods}
		p.DeliveryMethod.WarehouseID = wh
		return p
	}
	cases := []struct {
		p  *Posting
		ok bool
	}{
		{mk(1, PostingProduct{SKU: 10, Quantity: 1}, PostingProduct{SKU: 20, Quantity: 1}), true},
		{mk(1, PostingProduct{SKU: 20, Quantity: 1}, PostingProduct{SKU: 10, Quantity: 1}), true},
		{mk(2, PostingProduct{SKU: 10, Quantity: 1}, PostingProduct{SKU: 20, Quantity: 1}), false},
		{mk(1, PostingProduct{SKU: 10, Quantity: 2}, PostingProduct{SKU: 20, Quantity: 1}), false},
		{mk(1, PostingProduct{SKU: 10, Quantity: 1}, PostingProduct{SKU: 10, Quantity: 1}), false},
		{mk(1, PostingProduct{SKU: 10, Quantity: 1}), false},
		{mk(1, PostingProduct{SKU: 10, Quantity: 1}, PostingProduct{SKU: 20, Quantity: 1}, PostingProduct{SKU: 30, Quantity: 1}), false},
		{mk(1, PostingProduct{SKU: 10, Quantity: 1}, PostingProduct{SKU: 30, Quantity: 1}), false},
	}
	for i, c := range cases {
		if got := checkTarget(c.p, prof) == ""; got != c.ok {
			t.Errorf("случай %d: ожидали %v, получили %v (%s)", i, c.ok, got, checkTarget(c.p, prof))
		}
	}
}

func TestStockApply(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir)
	mocks := NewMockRegistry()
	mgr := NewManager(dir, store, mocks)
	active, _ := store.Snapshot()
	m := mocks.Get(active)
	m.OrderEvery = 0
	// Тест: остатки не меняются.
	if _, err := mgr.ApplyStock(context.Background(), active, 9); err != nil {
		t.Fatal(err)
	}
	if m.stock[mockSKU1] != 5 {
		t.Fatalf("тестовый режим изменил остаток")
	}
	store.Update(active, func(p *Profile) { p.Live = true })
	if _, err := mgr.ApplyStock(context.Background(), active, 9); err != nil {
		t.Fatal(err)
	}
	if m.stock[mockSKU1] != 9 || m.stock[mockSKU2] != 9 {
		t.Fatalf("остатки не выставлены: %v", m.stock)
	}
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{{"1.0.0", "1.0.1", true}, {"1.2.0", "1.10.0", true}, {"1.0.0", "1.0.0", false}, {"2.0.0", "1.9.9", false}, {"dev", "1.0.0", true}}
	for _, c := range cases {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("%s < %s: ожидали %v", c.a, c.b, c.want)
		}
	}
}
