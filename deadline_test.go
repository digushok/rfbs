package main

import (
	"context"
	"testing"
	"time"
)

// Случай из боевого журнала: 21 заказ с одним концом окна, запас 7 минут.
// Раньше пачка из 21 заказа целиком отклонялась Ozon (лимит — 20).
func TestTwentyOneSameDeadline(t *testing.T) {
	w, m, ck := setup(t, true)
	w.prof.MarginMin = 7
	end := ck.t.Add(40 * time.Minute)
	var ps []*Posting
	for i := 0; i < 21; i++ {
		p := m.AddPosting("target")
		p.Status, p.Substatus = "driver_pickup", "posting_driver_pick_up"
		p.AnalyticsData.DeliveryDateEnd.Time = end
		ps = append(ps, p)
	}
	ctx := context.Background()
	target := end.Add(-7 * time.Minute)
	for ck.t.Before(end) {
		next := w.Tick(ctx)
		wait := 30 * time.Second
		if !next.IsZero() && next.Sub(ck.t)+time.Second < wait {
			wait = next.Sub(ck.t) + time.Second
		}
		if wait < 2*time.Second {
			wait = 2 * time.Second
		}
		ck.add(wait)
	}
	var first, last time.Time
	for _, p := range ps {
		e := w.journal.Peek(p.PostingNumber)
		if !delivered(p) {
			t.Fatalf("заказ %s не переведён: %s, ошибка: %s", p.PostingNumber, p.Status, e.LastError)
		}
		at := e.DoneAt[StageDelivered]
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	if last.Before(target) || last.After(target.Add(3*time.Second)) {
		t.Fatalf("последняя пачка должна уйти в срок %s, ушла %s", target.Format("15:04:05"), last.Format("15:04:05"))
	}
	if first.Before(target.Add(-slotGap - 3*time.Second)) {
		t.Fatalf("первая пачка ушла слишком рано: %s", first.Format("15:04:05"))
	}
	if n := m.Calls["/v2/fbs/posting/delivered"]; n != 2 {
		t.Fatalf("ожидали 2 запроса (20 + 1), было %d", n)
	}
}

// Ozon отклоняет пачку из-за одного заказа — остальные уходят по одному, проблемный повторяется через 15 с.
func TestBatchRejectedFallbackSingle(t *testing.T) {
	w, m, ck := setup(t, true)
	end := ck.t.Add(10 * time.Minute)
	var ps []*Posting
	for i := 0; i < 5; i++ {
		p := m.AddPosting("target")
		p.Status, p.Substatus = "driver_pickup", "posting_driver_pick_up"
		p.AnalyticsData.DeliveryDateEnd.Time = end
		ps = append(ps, p)
	}
	w.Tick(context.Background()) // программа видит заказы заранее
	m.BadPostings = map[string]bool{ps[2].PostingNumber: true}
	ck.t = end.Add(-3 * time.Minute)
	w.Tick(context.Background())
	for i, p := range ps {
		if i == 2 {
			e := w.journal.Peek(p.PostingNumber)
			if delivered(p) || e.NextTry.Sub(ck.t) != deadlineRetry {
				t.Fatalf("проблемный заказ: статус %s, повтор через %s", p.Status, e.NextTry.Sub(ck.t))
			}
			continue
		}
		if !delivered(p) {
			t.Fatalf("заказ %d должен уйти по одному: %s", i, p.Status)
		}
	}
	// Ozon «починился» — через 15 секунд проблемный заказ уходит.
	m.BadPostings = nil
	ck.add(deadlineRetry)
	w.Tick(context.Background())
	if !delivered(ps[2]) {
		t.Fatalf("повтор не сработал: %s", ps[2].Status)
	}
}

// Заказ уже доставлен вручную: Ozon оставляет статус delivering, но подстатус — условно доставлен.
func TestConditionallyDeliveredSubstatus(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	p.Status, p.Substatus = "delivering", "posting_conditionally_delivered"
	ck.t = p.AnalyticsData.DeliveryDateEnd.Time.Add(time.Minute) // уже просрочен
	w.Tick(context.Background())
	e := w.journal.Peek(p.PostingNumber)
	if e.Final == "" || m.Calls["/v2/fbs/posting/delivered"] != 0 {
		t.Fatalf("доставленный вручную заказ трогать нельзя: final=%q, запросов %d", e.Final, m.Calls["/v2/fbs/posting/delivered"])
	}
}

// Ozon дважды отвечает «invalid transition» — программа прекращает попытки и просит проверить вручную.
func TestInvalidTransitionStops(t *testing.T) {
	w, m, ck := setup(t, true)
	p := m.AddPosting("target")
	p.Status, p.Substatus = "driver_pickup", "posting_driver_pick_up"
	ok := m.AddPosting("target")
	ok.Status, ok.Substatus = "driver_pickup", "posting_driver_pick_up"
	w.Tick(context.Background())
	m.NoTransition = map[string]bool{p.PostingNumber: true}
	ck.t = p.AnalyticsData.DeliveryDateEnd.Time.Add(-3 * time.Minute)
	for i := 0; i < 10; i++ {
		w.Tick(context.Background())
		ck.add(16 * time.Second)
	}
	e := w.journal.Peek(p.PostingNumber)
	if e.Final != "проверьте вручную" || e.Warning == "" {
		t.Fatalf("ожидали «проверьте вручную»: final=%q", e.Final)
	}
	if n := m.Calls["/v2/fbs/posting/delivered"]; n > 3 {
		t.Fatalf("слишком много попыток: %d", n)
	}
	if !delivered(ok) {
		t.Fatalf("соседний заказ должен быть переведён: %s", ok.Status)
	}
}
