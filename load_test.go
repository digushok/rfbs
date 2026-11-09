package main

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

// Нагрузочный сценарий: 100 заказов в час в течение 12 часов, окна доставки — общие таймслоты
// (у десятков заказов одинаковый конец окна). Проверяем, что каждый целевой заказ переведён
// в «Условно доставлен» в последние 3 минуты окна, и считаем запросы к Ozon.
func runLoad(t *testing.T, lastMile [2]string) {
	start := time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)
	ck := &clock{t: start}
	m := NewMockOzon(ck.now)
	m.OrderEvery = 21 * time.Second // + случайные 0–30 с ≈ 100 в час
	m.nextOrder = start
	m.stock[mockSKU1], m.stock[mockSKU2] = 1<<30, 1<<30
	m.LastMileState = lastMile
	// Таймслоты по 2 часа; заказ попадает в слот, который начинается не раньше чем через час.
	m.WindowFn = func(now time.Time) (time.Time, time.Time) {
		b := now.Truncate(2 * time.Hour).Add(2 * time.Hour)
		if b.Sub(now) < time.Hour {
			b = b.Add(2 * time.Hour)
		}
		return b, b.Add(2 * time.Hour)
	}
	prof := Profile{ID: "load", Demo: true, WarehouseID: mockWarehouseID, SKU1: mockSKU1, SKU2: mockSKU2, Live: true}
	prof.normalize()
	prof.MarginMin = 3
	c := &Client{base: "http://demo", http: &http.Client{Transport: m}}
	dir := t.TempDir()
	w := NewWorker(prof, c, LoadJournal(filepath.Join(dir, "j.json")), NewShopLog(dir, "load"))
	w.clock = ck.now
	ctx := context.Background()
	stopOrders := start.Add(12 * time.Hour)
	end := stopOrders.Add(6 * time.Hour)
	ticks := 0
	for ck.t.Before(end) {
		if !ck.t.Before(stopOrders) {
			m.OrderEvery = 0
		}
		next := w.Tick(ctx)
		ticks++
		wait := time.Duration(prof.PollSec) * time.Second
		if !next.IsZero() {
			if d := next.Sub(ck.t) + time.Second; d < wait {
				wait = d
			}
		}
		if wait < 2*time.Second {
			wait = 2 * time.Second
		}
		ck.add(wait + 700*time.Millisecond) // плюс время на сами запросы
	}
	targets, late, early, notDone := 0, 0, 0, 0
	byEnd := map[time.Time][]time.Time{} // конец окна → когда переведены
	for _, p := range m.postings {
		if checkTarget(p, &prof) != "" {
			if p.Status != "awaiting_packaging" {
				t.Fatalf("нецелевой заказ %s изменён: %s", p.PostingNumber, p.Status)
			}
			continue
		}
		targets++
		e := w.journal.Peek(p.PostingNumber)
		if !delivered(p) || e == nil || e.DoneAt[StageDelivered].IsZero() {
			notDone++
			continue
		}
		at, wEnd := e.DoneAt[StageDelivered], p.AnalyticsData.DeliveryDateEnd.Time
		if at.After(wEnd) {
			late++
		}
		byEnd[wEnd] = append(byEnd[wEnd], at)
	}
	// Расписание от конца: пачки по 20, последняя — в срок (конец окна − 3 мин), каждая предыдущая на 10 с раньше.
	for end, ats := range byEnd {
		target := end.Add(-3 * time.Minute)
		earliest := target.Add(-time.Duration((len(ats)+19)/20) * slotGap)
		last := ats[0]
		for _, at := range ats {
			if at.Before(earliest.Add(-5 * time.Second)) {
				early++
			}
			if at.After(last) {
				last = at
			}
		}
		if last.Before(target) || last.After(target.Add(5*time.Second)) {
			t.Fatalf("окно до %s: последний заказ должен уйти в срок %s, а ушёл %s (заказов %d)",
				end.Format("15:04"), target.Format("15:04:05"), last.Format("15:04:05"), len(ats))
		}
	}
	t.Logf("заказов: %d, целевых: %d, циклов: %d, сроков: %d, запросов: %v", len(m.postings), targets, ticks, len(byEnd), m.Calls)
	if len(m.postings) < 1150 {
		t.Fatalf("мало заказов: %d", len(m.postings))
	}
	if notDone > 0 || late > 0 || early > 0 {
		t.Fatalf("не доставлено: %d, опоздали: %d, раньше расписания: %d", notDone, late, early)
	}
	if n, want := m.Calls["/v2/fbs/posting/delivered"], (targets+19)/20+len(byEnd); n > want {
		t.Fatalf("«доставлено» не объединяется в пачки: %d запросов на %d заказов", n, targets)
	}
}

func TestLoad100PerHour(t *testing.T) {
	runLoad(t, [2]string{"driver_pickup", "posting_driver_pick_up"})
}

// После «курьер в пути» Ozon показывает статус, которого программа не знает и который не попадает в списки.
func TestLoadUnknownLastMileStatus(t *testing.T) {
	runLoad(t, [2]string{"some_new_status", "posting_some_new"})
}

// Несколько заказов, которые нужно перевести в «Условно доставлен» в пределах одной минуты:
// концы окон отличаются на 6 секунд. Каждый должен уйти в свой момент, а не раньше и не позже.
func TestManyDeliveriesWithinOneMinute(t *testing.T) {
	start := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	ck := &clock{t: start}
	m := NewMockOzon(ck.now)
	m.OrderEvery = 0
	prof := Profile{ID: "min", Demo: true, WarehouseID: mockWarehouseID, SKU1: mockSKU1, SKU2: mockSKU2, Live: true}
	prof.normalize()
	prof.MarginMin = 3
	c := &Client{base: "http://demo", http: &http.Client{Transport: m}}
	dir := t.TempDir()
	w := NewWorker(prof, c, LoadJournal(filepath.Join(dir, "j.json")), NewShopLog(dir, "min"))
	w.clock = ck.now
	base := start.Add(30 * time.Minute)
	var ps []*Posting
	for i := 0; i < 10; i++ {
		p := m.AddPosting("target")
		p.AnalyticsData.DeliveryDateBegin.Time = base.Add(-2 * time.Hour)
		p.AnalyticsData.DeliveryDateEnd.Time = base.Add(time.Duration(i*6) * time.Second)
		ps = append(ps, p)
	}
	ctx := context.Background()
	for ck.t.Before(base.Add(5 * time.Minute)) {
		next := w.Tick(ctx)
		wait := time.Duration(prof.PollSec) * time.Second
		if !next.IsZero() {
			if d := next.Sub(ck.t) + time.Second; d < wait {
				wait = d
			}
		}
		if wait < 2*time.Second {
			wait = 2 * time.Second
		}
		ck.add(wait + 1500*time.Millisecond) // запас на время ответа Ozon
	}
	for i, p := range ps {
		e := w.journal.Peek(p.PostingNumber)
		want := p.AnalyticsData.DeliveryDateEnd.Time.Add(-3 * time.Minute)
		got := e.DoneAt[StageDelivered]
		if !delivered(p) || got.Before(want) || got.Sub(want) > 10*time.Second {
			t.Fatalf("заказ %d: план %v, переведён %v (%s)", i, want.Format("15:04:05"), got.Format("15:04:05"), p.Status)
		}
		t.Logf("заказ %d: конец окна %s, план %s, переведён %s (+%s)", i, p.AnalyticsData.DeliveryDateEnd.Time.Format("15:04:05"),
			want.Format("15:04:05"), got.Format("15:04:05"), got.Sub(want).Round(100*time.Millisecond))
	}
}
