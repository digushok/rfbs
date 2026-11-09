package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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
	w.clock = ck.now
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
	if target.Status != "delivering" {
		t.Fatalf("после подхвата нужно выждать паузу, а статус уже %s", target.Status)
	}
	ck.add(61 * time.Second)
	w.Tick(context.Background())
	if target.Status != "driver_pickup" {
		t.Fatalf("ожидали продолжение цепочки, получили %s", target.Status)
	}
}

func TestCheckTarget(t *testing.T) {
	prof := &Profile{WarehouseID: 1, Batches: []Batch{{ID: "a", SKU1: 10, SKU2: 20, State: BatchActive}, {ID: "q", SKU1: 30, SKU2: 40, State: BatchQueued}}}
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
		{mk(1, PostingProduct{SKU: 30, Quantity: 1}, PostingProduct{SKU: 40, Quantity: 1}), false}, // партия ещё в очереди
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
	if _, err := mgr.ApplyStock(context.Background(), active, 9); err == nil {
		t.Fatalf("без активной партии остаток выставлять нельзя")
	}
	store.Update(active, func(p *Profile) { p.Batches[0].State = BatchActive })
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

func TestClockOffset(t *testing.T) {
	c := &Client{}
	sent := time.Now()
	server := sent.Add(4 * time.Minute)
	c.noteDate(server.UTC().Format(http.TimeFormat), sent, sent.Add(200*time.Millisecond))
	off, ok := c.ClockOffset()
	if !ok || off < 4*time.Minute-2*time.Second || off > 4*time.Minute+2*time.Second {
		t.Fatalf("ожидали разницу около 4 минут, получили %v", off)
	}
}

func TestRetryDelay(t *testing.T) {
	if retryDelay(1) != 30*time.Second || retryDelay(2) != time.Minute || retryDelay(20) != maxRetryDelay {
		t.Fatalf("неверные паузы повтора: %v %v %v", retryDelay(1), retryDelay(2), retryDelay(20))
	}
}

func TestErrorBackoff(t *testing.T) {
	w, m, ck := setup(t, true)
	target := m.AddPosting("target")
	m.Fail = map[string]bool{"/v4/posting/fbs/ship": true}
	ctx := context.Background()
	// 10 минут циклов каждые 15 секунд: при паузах 30с, 1м, 2м, 4м — не больше 5 попыток.
	for i := 0; i < 40; i++ {
		w.Tick(ctx)
		ck.add(15 * time.Second)
	}
	if n := m.Calls["/v4/posting/fbs/ship"]; n < 3 || n > 5 {
		t.Fatalf("ожидали 3–5 попыток «Собрать» с нарастающей паузой, было %d", n)
	}
	e := w.journal.Peek(target.PostingNumber)
	if e.LastError == "" || e.NextTry.IsZero() {
		t.Fatalf("ошибка должна быть видна в журнале")
	}
	// Ozon «починился» — заказ продолжает путь.
	m.Fail = nil
	ck.t = e.NextTry
	w.Tick(ctx)
	if target.Status != "awaiting_deliver" {
		t.Fatalf("после восстановления ожидали awaiting_deliver, получили %s", target.Status)
	}
	if e.LastError != "" || e.Fails != 0 {
		t.Fatalf("ошибка должна сброситься после успеха")
	}
}

func TestConcurrentStartSingleWorker(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir)
	mgr := NewManager(dir, store, NewMockRegistry())
	id, _ := store.Snapshot()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() { mgr.Start(id); done <- struct{}{} }()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	mgr.mu.Lock()
	n := len(mgr.workers)
	mgr.mu.Unlock()
	mgr.StopAll()
	if n != 1 {
		t.Fatalf("ожидали одного работника, получили %d", n)
	}
}

// Ответ /v4/posting/fbs/list в формате Ozon: postings/has_next/cursor на верхнем уровне.
func TestV4ListFormat(t *testing.T) {
	body := `{"postings":[{"posting_number":"1-1-1","status":"awaiting_packaging","products":[{"sku":5,"quantity":1}],
	"delivery_method":{"warehouse_id":7},"analytics_data":{"delivery_date_begin":"2026-11-09T10:00:00Z","delivery_date_end":"2026-11-09T14:00:00Z"}}],
	"has_next":false,"cursor":""}`
	var gotReq map[string]any
	c := &Client{base: "http://x", http: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		json.NewDecoder(r.Body).Decode(&gotReq)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}}
	ps, err := c.ListPostings(context.Background(), 7, "awaiting_packaging", time.Now().Add(-time.Hour), time.Now())
	if err != nil || len(ps) != 1 {
		t.Fatalf("разбор ответа: %v, %d", err, len(ps))
	}
	if _, end := ps[0].Window(); end.IsZero() {
		t.Fatalf("окно доставки не разобрано")
	}
	f := gotReq["filter"].(map[string]any)
	if f["statuses"] == nil || f["warehouse_ids"] == nil || gotReq["limit"].(float64) > 100 {
		t.Fatalf("неверный запрос: %v", gotReq)
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
