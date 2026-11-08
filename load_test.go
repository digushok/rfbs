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
	for _, p := range m.postings {
		if checkTarget(p, &prof) != "" {
			if p.Status != "awaiting_packaging" {
				t.Fatalf("нецелевой заказ %s изменён: %s", p.PostingNumber, p.Status)
			}
			continue
		}
		targets++
		e := w.journal.Peek(p.PostingNumber)
		if p.Status != "delivered" || e == nil || e.DoneAt[StageDelivered].IsZero() {
			notDone++
			continue
		}
		at, wEnd := e.DoneAt[StageDelivered], p.AnalyticsData.DeliveryDateEnd.Time
		if at.After(wEnd) {
			late++
		}
		if at.Before(wEnd.Add(-3*time.Minute - 5*time.Second)) {
			early++
		}
	}
	t.Logf("заказов: %d, целевых: %d, циклов: %d, запросов: %v", len(m.postings), targets, ticks, m.Calls)
	if len(m.postings) < 1150 {
		t.Fatalf("мало заказов: %d", len(m.postings))
	}
	if notDone > 0 || late > 0 || early > 0 {
		t.Fatalf("не доставлено: %d, опоздали: %d, слишком рано: %d", notDone, late, early)
	}
	if n := m.Calls["/v2/fbs/posting/delivered"]; n > targets/10 {
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
