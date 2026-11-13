package main

// Автоматизация: опрос заказов и перевод целевых отправлений по статусам.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var stageNames = [5]string{"Ожидает сборки", "Собран", "Доставляется", "Курьер в пути", "Условно доставлен"}
var actionNames = [5]string{"", "Собрать", "Доставляется", "Курьер в пути", "Условно доставлен"}
var actionPaths = [5]string{"", "/v4/posting/fbs/ship", "/v2/fbs/posting/delivering", "/v2/fbs/posting/last-mile", "/v2/fbs/posting/delivered"}

var lastMileSubstatuses = map[string]bool{
	"posting_transferred_to_courier_service": true,
	"posting_in_courier_service":             true,
	"posting_driver_pick_up":                 true,
	"posting_last_mile":                      true,
	"posting_in_last_mile":                   true,
}

// realStage — этап по статусу в Ozon. ok=false — статус не наш (не трогаем),
// final — заказ завершён (доставлен, отменён, арбитраж).
func realStage(p *Posting) (stage int, ok bool, final string) {
	// rFBS: после «Условно доставлен» Ozon может оставлять статус delivering — смотрим подстатус.
	switch p.Substatus {
	case "posting_conditionally_delivered":
		return StageDelivered, true, "условно доставлен"
	case "posting_delivered", "posting_received", "posting_delivered_to_customer":
		return StageDelivered, true, "доставлен"
	}
	switch p.Status {
	case "awaiting_packaging":
		return StageNew, true, ""
	case "awaiting_deliver":
		return StageShipped, true, ""
	case "delivering", "sent_by_seller":
		if lastMileSubstatuses[p.Substatus] {
			return StageLastMile, true, ""
		}
		return StageDelivering, true, ""
	case "driver_pickup":
		return StageLastMile, true, ""
	case "delivered":
		if p.Substatus == "posting_conditionally_delivered" {
			return StageDelivered, true, "условно доставлен"
		}
		return StageDelivered, true, "доставлен"
	case "cancelled", "cancelled_from_split_pending", "not_accepted":
		return -1, false, "отменён"
	case "arbitration", "client_arbitration":
		return -1, false, "арбитраж"
	}
	return -1, false, ""
}

// checkTarget возвращает "" для целевого заказа или причину пропуска.
func checkTarget(p *Posting, prof *Profile) string {
	_, queued, reason := targetBatch(p, prof)
	if reason == "" && queued {
		return "пара из партии, которая ещё в очереди"
	}
	return reason
}

// targetBatch — партия, к которой подходит заказ: склад, ровно 2 SKU по 1 шт., пара из партии.
// queued=true — пара из партии, которая ещё в очереди (такие заказы берутся, только если их уже продвинули вручную).
func targetBatch(p *Posting, prof *Profile) (id string, queued bool, reason string) {
	wh := p.DeliveryMethod.WarehouseID
	if wh == 0 && p.AnalyticsData != nil {
		wh = p.AnalyticsData.WarehouseID
	}
	if wh != prof.WarehouseID {
		return "", false, fmt.Sprintf("другой склад (%d)", wh)
	}
	if len(p.Products) != 2 {
		return "", false, fmt.Sprintf("в заказе %d поз., нужно ровно 2", len(p.Products))
	}
	for _, pr := range p.Products {
		if pr.Quantity != 1 {
			return "", false, fmt.Sprintf("SKU %d в количестве %d шт., нужно 1", pr.SKU, pr.Quantity)
		}
	}
	a, c := p.Products[0].SKU, p.Products[1].SKU
	if a == c {
		return "", false, fmt.Sprintf("SKU %d дважды", a)
	}
	for i := range prof.Batches {
		if b := &prof.Batches[i]; b.Started() && b.Matches(a, c) {
			return b.ID, false, ""
		}
	}
	for i := range prof.Batches {
		if b := &prof.Batches[i]; b.Matches(a, c) {
			return b.ID, true, ""
		}
	}
	s := []string{fmt.Sprint(a), fmt.Sprint(c)}
	sort.Strings(s)
	return "", false, "другой состав: SKU " + strings.Join(s, ", ") + " (нет такой партии)"
}

type PostingView struct {
	PostingNumber string       `json:"posting_number"`
	OrderNumber   string       `json:"order_number"`
	Created       time.Time    `json:"created"`
	Status        string       `json:"status"`
	Substatus     string       `json:"substatus,omitempty"`
	Stage         int          `json:"stage"`
	Final         string       `json:"final,omitempty"`
	WindowBegin   time.Time    `json:"window_begin"`
	WindowEnd     time.Time    `json:"window_end"`
	Plan          [5]time.Time `json:"plan"` // факт (для выполненных) или план
	Done          [5]bool      `json:"done"`
	NextAction    string       `json:"next_action"`
	NextAt        time.Time    `json:"next_at"`
	Warning       string       `json:"warning,omitempty"`
	Error         string       `json:"error,omitempty"`
	Batch         int          `json:"batch,omitempty"` // номер партии
	Manual        [5]bool      `json:"manual"`          // этапы, нажатые вручную
	Picked        bool         `json:"picked,omitempty"`
	NoCount       bool         `json:"no_count,omitempty"`
}

type SkipView struct {
	PostingNumber string    `json:"posting_number"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	Created       time.Time `json:"created"`
	Products      string    `json:"products"`
}

// Статусы, в которых заказ ещё может требовать действий программы.
var listStatuses = []string{"awaiting_packaging", "awaiting_deliver", "delivering", "driver_pickup"}

const (
	batchSize     = 20               // отправлений в одном запросе смены статуса (ограничение Ozon: от 1 до 20)
	slotGap       = 10 * time.Second // расписание «Условно доставлен»: пачки к одному сроку идут с таким шагом
	deadlineRetry = 15 * time.Second // повтор «Условно доставлен» после ошибки — быстро, у него есть срок
	recheckEvery  = time.Hour        // как часто перепроверять заказ, пропавший из списков (и всегда — когда подошёл шаг)
	maxRetryDelay = 10 * time.Minute
)

type Worker struct {
	prof    Profile
	client  *Client
	journal *Journal
	log     *ShopLog
	cancel  context.CancelFunc
	done    chan struct{}
	kick    chan struct{}
	clock   func() time.Time

	mu        sync.Mutex
	started   time.Time
	lastPoll  time.Time
	nextPoll  time.Time
	lastErr   string
	views     []PostingView
	skipped   []SkipView
	bviews    []BatchView
	loggedFor map[string]bool // что уже записано в лог, чтобы не повторяться
	badStatus map[string]bool // статусы, которые Ozon не принимает в фильтре

	store       *Store           // откуда читать партии (nil в тестах — берутся из prof)
	sim         map[string]Batch // тестовый режим: симулированное состояние партий (в настройки не пишется)
	bt          []Batch          // партии на время цикла
	btDirty     map[string]bool
	switchReq   bool // вручную: перейти к следующей партии
	activateAt  time.Time
	productInfo map[int64]*ProductInfo

	stockCheckAt      time.Time // следующая проверка остатков завершённых пар
	completed         bool      // очередь окончена — работник ждёт остановки
	cancelReason      int64
	cancelReasonTitle string
	onDone            func() // вызывается, когда очередь окончена
}

func NewWorker(prof Profile, client *Client, journal *Journal, log *ShopLog) *Worker {
	client.readOnly = !prof.Live
	return &Worker{prof: prof, client: client, journal: journal, log: log, clock: time.Now,
		kick: make(chan struct{}, 1), loggedFor: map[string]bool{}, badStatus: map[string]bool{},
		sim: map[string]Batch{}, btDirty: map[string]bool{}, productInfo: map[int64]*ProductInfo{}}
}

// now — текущее время по часам Ozon (часы компьютера + измеренная разница).
func (w *Worker) now() time.Time {
	t := w.clock()
	if off, ok := w.client.ClockOffset(); ok {
		t = t.Add(off)
	}
	return t
}

func (w *Worker) Start() {
	// После перезапуска не ждём паузы прошлых ошибок — пробуем сразу.
	for _, e := range w.journal.All() {
		if e.Final == "" && !e.NextTry.IsZero() {
			e.NextTry = time.Time{}
			w.journal.MarkDirty()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = w.now()
	mode := "ТЕСТОВЫЙ режим: в Ozon ничего не меняется, действия только симулируются"
	if w.prof.Live {
		mode = "БОЕВОЙ режим: статусы в Ozon меняются по-настоящему"
	}
	w.log.Add("info", "Автоматизация запущена. %s. Склад %d, партий: %d, пауза %d сек, запас %d мин.",
		mode, w.prof.WarehouseID, len(w.prof.Batches), w.prof.PauseSec, w.prof.MarginMin)
	go func() {
		defer close(w.done)
		for {
			next := w.Tick(ctx)
			wait := time.Duration(w.prof.PollSec) * time.Second
			if !next.IsZero() {
				if d := next.Sub(w.now()) + time.Second; d < wait {
					wait = d
				}
			}
			if wait < 2*time.Second {
				wait = 2 * time.Second
			}
			w.mu.Lock()
			w.nextPoll = w.now().Add(wait)
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			case <-w.kick:
			}
		}
	}()
}

func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
		<-w.done
		w.journal.Save()
		w.log.Add("info", "Автоматизация остановлена.")
	}
}

func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Worker) logOnce(key, level, format string, args ...any) {
	w.mu.Lock()
	if w.loggedFor[key] {
		w.mu.Unlock()
		return
	}
	if len(w.loggedFor) > 50000 {
		w.loggedFor = map[string]bool{}
	}
	w.loggedFor[key] = true
	w.mu.Unlock()
	w.log.Add(level, format, args...)
}

func (w *Worker) setErr(err error) {
	w.mu.Lock()
	changed := w.lastErr != err.Error()
	w.lastErr = err.Error()
	w.mu.Unlock()
	if changed {
		w.log.Add("err", "%v", err)
	}
}

func productsText(p *Posting) string {
	var s []string
	for _, pr := range p.Products {
		s = append(s, fmt.Sprintf("%d×%d", pr.SKU, pr.Quantity))
	}
	return strings.Join(s, " + ")
}

func retryDelay(fails int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < fails && d < maxRetryDelay; i++ {
		d *= 2
	}
	if d > maxRetryDelay {
		d = maxRetryDelay
	}
	return d
}

type pendingAction struct {
	p    *Posting
	e    *JEntry
	next int
}

// fetch — активные отправления склада и отслеживаемые заказы, пропавшие из списков.
func (w *Worker) fetch(ctx context.Context, now time.Time) ([]*Posting, error) {
	since := now.AddDate(0, 0, -w.prof.LookbackDays)
	to := now.Add(24 * time.Hour)
	seen := map[string]bool{}
	var out []*Posting
	for _, st := range listStatuses {
		if w.badStatus[st] {
			continue
		}
		ps, err := w.client.ListPostings(ctx, w.prof.WarehouseID, st, since, to)
		if err != nil {
			var oe *OzonError
			switch {
			case ctx.Err() != nil:
				return nil, ctx.Err()
			case len(ps) > 0: // список обрезан — работаем с тем, что есть
				w.logOnce("trunc|"+st, "warn", "Внимание: %v", err)
			case st != "awaiting_packaging" && errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500 && oe.Status != 429 && oe.Status != 401 && oe.Status != 403:
				w.badStatus[st] = true
				w.log.Add("warn", "Ozon не принимает фильтр по статусу %s (%v). Такие заказы будут проверяться по одному.", st, err)
				continue
			default:
				return nil, fmt.Errorf("не удалось получить заказы (%s): %w", st, err)
			}
		}
		for i := range ps {
			if !seen[ps[i].PostingNumber] {
				seen[ps[i].PostingNumber] = true
				out = append(out, &ps[i])
			}
		}
	}
	// Заказы, которые программа ведёт, но которых нет в списках (неизвестный статус после «курьер в пути» и т.п.):
	// перепроверяем по одному — редко, но обязательно, когда подошло время следующего шага.
	checked := 0
	for _, e := range w.journal.All() {
		if seen[e.PostingNumber] || e.Final != "" || e.Stage < 0 || checked >= 100 {
			continue
		}
		dueNow := !e.DueAt.IsZero() && !e.DueAt.After(now)
		if !dueNow && now.Sub(e.SeenAt) < recheckEvery {
			continue
		}
		checked++
		p, err := w.client.GetPosting(ctx, e.PostingNumber)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			e.SeenAt = now // не долбим каждые 30 секунд
			e.LastError = "не удалось получить заказ из Ozon: " + err.Error()
			w.journal.MarkDirty()
			continue
		}
		seen[p.PostingNumber] = true
		out = append(out, p)
	}
	return out, nil
}

// Tick — один цикл: прочитать заказы и выполнить всё, что пора. Возвращает время ближайшего дела.
func (w *Worker) Tick(ctx context.Context) time.Time {
	if w.completed {
		return time.Time{}
	}
	now := w.now()
	postings, err := w.fetch(ctx, now)
	w.mu.Lock()
	w.lastPoll = now
	w.mu.Unlock()
	if err != nil {
		if ctx.Err() == nil {
			w.setErr(err)
		}
		return time.Time{}
	}
	w.mu.Lock()
	if w.lastErr != "" {
		w.log.Add("ok", "Связь с Ozon восстановлена.")
	}
	w.lastErr = ""
	w.mu.Unlock()
	if off, ok := w.client.ClockOffset(); ok && (off > time.Minute || off < -time.Minute) {
		w.logOnce("clock", "warn", "Часы компьютера расходятся с Ozon на %s. Программа учитывает разницу, но лучше включить синхронизацию времени в Windows.", off.Round(time.Second))
	}

	w.loadBatches()
	var skipped []SkipView
	var pend []pendingAction
	var foreign []*Posting
	for _, p := range postings {
		if ctx.Err() != nil {
			return time.Time{}
		}
		e := w.journal.Peek(p.PostingNumber)
		if e != nil && e.Abandoned {
			if len(skipped) < 300 {
				skipped = append(skipped, SkipView{PostingNumber: p.PostingNumber, Status: p.Status, Reason: "брошен при сбросе очереди", Created: p.InProcessAt.Time, Products: productsText(p)})
			}
			continue
		}
		batchID, queued, reason := targetBatch(p, &w.prof)
		if reason == "" && queued && (e == nil || e.Stage < 0) {
			// Пара из партии в очереди: берём, только если заказ уже продвинули вручную дальше «Ожидает сборки».
			if rs, ok, _ := realStage(p); !ok || rs < StageShipped {
				reason = fmt.Sprintf("пара из партии %d, которая ещё в очереди", w.num(batchID))
			}
		}
		if reason != "" {
			// Заказ, который программа уже взяла в работу (например, партию потом удалили), доводим до конца.
			if e != nil && e.Stage >= 0 {
				reason, batchID, queued = "", e.Batch, false
			}
		}
		if reason != "" {
			if postingWarehouse(p) == w.prof.WarehouseID {
				foreign = append(foreign, p)
			}
			if _, ok, final := realStage(p); ok && final == "" && len(skipped) < 300 {
				skipped = append(skipped, SkipView{PostingNumber: p.PostingNumber, Status: p.Status, Reason: reason, Created: p.InProcessAt.Time, Products: productsText(p)})
			}
			w.logOnce("skip|"+p.PostingNumber, "info", "Заказ %s пропущен — не целевой: %s.", p.PostingNumber, reason)
			continue
		}
		if a, ok := w.update(p, now, batchID, queued); ok {
			pend = append(pend, a)
		}
	}
	if w.pauseStock(ctx) { // на паузе статусы не меняются и партии не переключаются
		pend = append(pend, w.planDelivered(now)...)
		w.execute(ctx, pend, now)
		w.manageBatches(ctx, now)
	}
	w.handleForeign(ctx, foreign, now)
	w.verifyRestores(ctx, now)
	for i := range skipped {
		skipped[i].Reason += foreignNote(w.journal.ForeignGet(skipped[i].PostingNumber))
	}
	w.checkDoneStock(ctx, now, false)
	w.flushBatches()
	w.journal.Save()
	if w.checkComplete(ctx, now) {
		w.flushBatches()
		w.journal.Save()
	}

	views, nextDue := w.buildViews(now)
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Created.After(skipped[j].Created) })
	bv := w.batchViews()
	w.mu.Lock()
	w.views, w.skipped, w.bviews = views, skipped, bv
	w.mu.Unlock()
	return nextDue
}

// update синхронизирует запись журнала с данными Ozon и решает, пора ли делать следующий шаг.
func (w *Worker) update(p *Posting, now time.Time, batchID string, noCount bool) (pendingAction, bool) {
	begin, end := p.Window()
	e := w.journal.Get(p.PostingNumber)
	isNew := e.SeenAt.IsZero()
	e.SeenAt, e.OrderNumber, e.Products, e.Status, e.Substatus = now, p.OrderNumber, productsText(p), p.Status, p.Substatus
	e.Created, e.WindowBegin, e.WindowEnd = p.InProcessAt.Time, begin, end
	if strings.HasPrefix(e.LastError, "не удалось получить заказ") {
		e.LastError = ""
	}
	w.journal.MarkDirty()
	if e.Final != "" {
		return pendingAction{}, false // программа с этим заказом закончила
	}

	rs, ok, final := realStage(p)
	if ok && rs > e.Stage {
		w.syncManual(p, e, rs, now)
	}
	if e.Batch == "" && batchID != "" && final == "" {
		e.Batch = batchID
		if noCount {
			e.NoCount = true
			w.log.Add("info", "Заказ %s — пара из партии %d, которая ещё в очереди: веду его до конца, но в «продано» не считаю.", p.PostingNumber, w.num(batchID))
		} else {
			w.addSold(batchID, 1)
		}
	}
	if final == "отменён" && e.Batch != "" && !e.Uncounted && !e.NoCount {
		e.Uncounted = true
		w.addSold(e.Batch, -1)
	}
	if final != "" {
		if e.Final == "" {
			e.Final, e.FinalAt = final, now
			if final == "отменён" || final == "арбитраж" {
				w.log.Add("warn", "Заказ %s: %s в Ozon, программа его больше не трогает.", p.PostingNumber, final)
			}
		}
		return pendingAction{}, false
	}
	if !ok && e.Stage >= StageDelivering {
		// Незнакомый статус у заказа, который программа уже вела (например, так Ozon показывает
		// «курьер в пути»): продолжаем по журналу, чтобы не пропустить «Условно доставлен».
		ok, rs = true, e.Stage
	}
	if !ok {
		e.Warning = "статус Ozon «" + p.Status + "» — программа такой заказ не трогает"
		e.DueAt = time.Time{}
		w.logOnce("unknown|"+p.PostingNumber+"|"+p.Status, "warn", "Заказ %s: %s.", p.PostingNumber, e.Warning)
		return pendingAction{}, false
	}
	if e.Stage >= StageDelivered {
		if e.Final == "" {
			e.Final, e.FinalAt = "условно доставлен", now
		}
		return pendingAction{}, false
	}
	if isNew {
		w.logOnce("found|"+p.PostingNumber, "info", "Найден целевой заказ %s (%s), статус Ozon: %s, окно доставки: %s.",
			p.PostingNumber, productsText(p), p.Status, fmtWindow(begin, end))
	}

	next := e.Stage + 1
	due, warn := w.dueFor(next, e, now)
	e.DueAt, e.Warning = due, warn
	if warn != "" {
		w.logOnce("nowindow|"+p.PostingNumber, "warn", "Заказ %s: %s.", p.PostingNumber, warn)
	} else if !w.prof.Live && e.Stage > rs {
		e.Warning = "этапы после «" + stageNames[rs] + "» выполнены только в тесте"
	}
	if next == StageDelivered {
		return pendingAction{}, false // время «Условно доставлен» назначает planDelivered — для всех заказов сразу
	}
	if due.IsZero() || due.After(now) || now.Before(e.NextTry) {
		return pendingAction{}, false
	}
	return pendingAction{p: p, e: e, next: next}, true
}

// planDelivered — расписание «Условно доставлен» для всех заказов, которые его ждут.
// Каждый заказ — как можно позже (конец окна минус запас), но в одном запросе Ozon принимает не больше 20 заказов,
// поэтому, если к одному сроку их больше, расписание строится от конца: последняя пачка уходит ровно в срок,
// каждая предыдущая — на slotGap раньше. Так последний заказ переводится в крайнее время, а все успевают.
func (w *Worker) planDelivered(now time.Time) []pendingAction {
	type item struct {
		e      *JEntry
		target time.Time
	}
	var items []item
	for _, e := range w.journal.All() {
		if e.Final != "" || e.Abandoned || e.Stage != StageLastMile || e.SeenAt.Before(now) {
			continue // только заказы, данные по которым получены в этом цикле
		}
		t, warn := w.dueFor(StageDelivered, e, now)
		if warn != "" || t.IsZero() {
			continue
		}
		items = append(items, item{e, t})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].target.After(items[j].target) })
	var slot time.Time
	inSlot := 0
	var pend []pendingAction
	for _, it := range items {
		switch {
		case slot.IsZero() || it.target.Before(slot):
			slot, inSlot = it.target, 0
		case inSlot >= batchSize:
			slot, inSlot = slot.Add(-slotGap), 0
		}
		inSlot++
		due := slot
		if due.Before(now) {
			due = now
		}
		it.e.DueAt = due
		if due.After(now) || now.Before(it.e.NextTry) {
			continue
		}
		if !it.e.WindowEnd.IsZero() && now.After(it.e.WindowEnd) {
			w.logOnce("late|"+it.e.PostingNumber, "warn", "Заказ %s: окно доставки уже закончилось (%s), перевожу в «Условно доставлен» сразу.",
				it.e.PostingNumber, it.e.WindowEnd.Local().Format("02.01 15:04"))
		}
		pend = append(pend, pendingAction{p: &Posting{PostingNumber: it.e.PostingNumber}, e: it.e, next: StageDelivered})
	}
	if len(items) > 0 {
		w.journal.MarkDirty()
	}
	return pend
}

// syncManual поднимает этап по данным Ozon. Этапы, которые программа не выполняла, помечаются как ручные.
func (w *Worker) syncManual(p *Posting, e *JEntry, rs int, now time.Time) {
	first := e.Stage < 0
	var manual []string
	for i := max(e.Stage+1, StageShipped); i <= rs; i++ {
		if !e.DoneAt[i].IsZero() {
			continue
		}
		if i == e.Stage+1 && e.Fails > 0 && e.LastError != "" && strings.Contains(e.LastError, "«"+actionNames[i]+"»") {
			// Наш запрос, ответ на который не дошёл, — Ozon его всё-таки выполнил.
			e.DoneAt[i], e.Fails, e.LastError, e.NextTry = now, 0, "", time.Time{}
			w.log.Add("ok", "Заказ %s: «%s» — выполнено (подтверждено по статусу Ozon).", p.PostingNumber, actionNames[i])
			continue
		}
		e.Manual[i] = true
		manual = append(manual, "«"+actionNames[i]+"»")
	}
	e.Stage = rs
	if len(manual) == 0 {
		return
	}
	e.ManualAt, e.Picked = now, true
	e.Fails, e.LastError, e.NextTry = 0, "", time.Time{}
	steps := strings.Join(manual, ", ")
	switch {
	case rs >= StageDelivered:
		w.log.Add("info", "Заказ %s: %s выставлено вручную (не программой). Заказ завершён.", p.PostingNumber, steps)
	case first:
		w.log.Add("info", "Заказ %s подхвачен в работу: в Ozon уже «%s», этапы %s выполнены вручную (не программой). Дальше веду сам: следующий шаг — «%s» через паузу.",
			p.PostingNumber, stageNames[rs], steps, actionNames[rs+1])
	default:
		w.log.Add("info", "Заказ %s: этап %s выполнен вручную (не программой). Продолжаю: следующий шаг — «%s» через паузу.",
			p.PostingNumber, steps, actionNames[rs+1])
	}
}

// dueFor — когда можно выполнить шаг next.
func (w *Worker) dueFor(next int, e *JEntry, now time.Time) (time.Time, string) {
	pause := time.Duration(w.prof.PauseSec) * time.Second
	earliest := now
	last := e.LastActionAt()
	if e.ManualAt.After(last) {
		last = e.ManualAt // после ручного шага ждём паузу от момента, когда его заметили
	}
	if !last.IsZero() && last.Add(pause).After(now) {
		earliest = last.Add(pause)
	}
	if next < StageDelivered {
		return earliest, ""
	}
	if e.WindowEnd.IsZero() {
		return time.Time{}, "Ozon не вернул окно доставки — «Условно доставлен» не запланирован, переведите вручную"
	}
	target := e.WindowEnd.Add(-time.Duration(w.prof.MarginMin) * time.Minute)
	if target.Before(earliest) {
		target = earliest
	}
	return target, ""
}

// execute выполняет накопленные шаги: «Собрать» — по одному, смену статусов — пачками.
func (w *Worker) execute(ctx context.Context, pend []pendingAction, now time.Time) {
	if len(pend) == 0 {
		return
	}
	sort.Slice(pend, func(i, j int) bool { // сначала самые срочные
		if pend[i].next != pend[j].next {
			return pend[i].next > pend[j].next
		}
		return pend[i].e.DueAt.Before(pend[j].e.DueAt)
	})
	if !w.prof.Live {
		for _, a := range pend {
			w.log.Add("test", "[ТЕСТ] Заказ %s: «%s» — сделал бы запрос %s. В Ozon ничего не отправлено.", a.p.PostingNumber, actionNames[a.next], actionPaths[a.next])
			w.markDone(a, now)
		}
		return
	}
	// Сначала самые срочные шаги («Условно доставлен» привязан к концу окна), «Собрать» — в конце.
	groups := map[int][]pendingAction{}
	for _, a := range pend {
		groups[a.next] = append(groups[a.next], a)
	}
	for _, next := range []int{StageDelivered, StageLastMile, StageDelivering} {
		list := groups[next]
		for i := 0; i < len(list); i += batchSize {
			if ctx.Err() != nil {
				return
			}
			chunk := list[i:min(i+batchSize, len(list))]
			pns := make([]string, len(chunk))
			for k, a := range chunk {
				pns[k] = a.p.PostingNumber
			}
			res, err := w.client.SetStatusBatch(ctx, actionPaths[next], pns)
			var oe *OzonError
			if err != nil && len(chunk) > 1 && errors.As(err, &oe) && oe.Status >= 400 && oe.Status < 500 && oe.Status != 429 {
				// Ozon отклонил пачку целиком — отправляем по одному, чтобы один заказ не держал остальные.
				w.log.Add("warn", "Ozon отклонил пачку «%s» из %d заказов (%v) — отправляю по одному.", actionNames[next], len(chunk), err)
				for _, a := range chunk {
					if ctx.Err() != nil {
						return
					}
					w.result(a, w.client.SetStatus(ctx, actionPaths[next], a.p.PostingNumber), now)
				}
				continue
			}
			if err != nil {
				for _, a := range chunk {
					w.fail(a, err, now, true)
				}
				w.log.Add("err", "Не удалось «%s» для %d заказов (%s): %v. Повтор через %s.", actionNames[next], len(chunk),
					strings.Join(pns, ", "), err, chunk[0].e.NextTry.Sub(now).Round(time.Second))
				continue
			}
			failed := map[string][]string{} // текст ошибки → заказы
			for _, a := range chunk {
				if err := res[a.p.PostingNumber]; err != nil {
					w.fail(a, err, now, true)
					if a.e.Final == "" { // не «проверьте вручную»
						failed[err.Error()] = append(failed[err.Error()], a.p.PostingNumber)
					}
					continue
				}
				w.result(a, nil, now)
			}
			for msg, pns := range failed {
				w.log.Add("err", "Не удалось «%s» для %d заказов (%s): %s. Повтор через %s.", actionNames[next], len(pns),
					strings.Join(pns, ", "), msg, w.journal.Peek(pns[0]).NextTry.Sub(now).Round(time.Second))
			}
		}
	}
	for _, a := range groups[StageShipped] {
		if ctx.Err() != nil {
			return
		}
		w.result(a, w.client.Ship(ctx, a.p), now)
	}
}

func (w *Worker) markDone(a pendingAction, now time.Time) {
	e := a.e
	e.Stage, e.DoneAt[a.next], e.Fails, e.LastError, e.NextTry = a.next, now, 0, "", time.Time{}
	if a.next >= StageDelivered {
		e.Final, e.FinalAt, e.DueAt = "условно доставлен", now, time.Time{}
	} else {
		e.DueAt, _ = w.dueFor(a.next+1, e, now)
	}
	w.journal.MarkDirty()
}

func (w *Worker) result(a pendingAction, err error, now time.Time) {
	name := actionNames[a.next]
	if err == nil {
		w.markDone(a, now)
		w.log.Add("ok", "Заказ %s: «%s» — выполнено.", a.p.PostingNumber, name)
		return
	}
	w.fail(a, err, now, false)
}

// fail записывает ошибку шага и время повтора. quiet — без строки в журнале (её пишет вызывающий за всю пачку).
func (w *Worker) fail(a pendingAction, err error, now time.Time, quiet bool) {
	if errors.Is(err, context.Canceled) {
		return
	}
	name := actionNames[a.next]
	e := a.e
	e.Fails++
	e.LastError = fmt.Sprintf("«%s»: %v", name, err)
	delay := retryDelay(e.Fails)
	if a.next == StageDelivered {
		delay = deadlineRetry // у «Условно доставлен» есть срок — повторяем быстро
		if strings.Contains(strings.ToLower(err.Error()), "invalid transition") {
			// Ozon не даёт перевести: заказ уже в другом состоянии (чаще всего — уже доставлен вручную).
			e.Invalid++
			if e.Invalid >= 2 {
				e.Final, e.FinalAt, e.DueAt, e.NextTry = "проверьте вручную", now, time.Time{}, time.Time{}
				e.Warning = "Ozon не даёт перевести в «Условно доставлен» (" + err.Error() + "). Скорее всего, заказ уже доставлен вручную — проверьте в кабинете."
				w.journal.MarkDirty()
				w.log.Add("warn", "Заказ %s: Ozon дважды ответил, что перевод в «Условно доставлен» невозможен (%v). Больше не пытаюсь — проверьте заказ в кабинете (скорее всего, он уже доставлен вручную). Подстатус Ozon: %s.",
					a.p.PostingNumber, err, e.Substatus)
				return
			}
		} else {
			e.Invalid = 0
		}
	}
	e.NextTry = now.Add(delay)
	w.journal.MarkDirty()
	if !quiet {
		w.log.Add("err", "Заказ %s: не удалось «%s» (попытка %d, повтор через %s): %v", a.p.PostingNumber, name, e.Fails,
			delay.Round(time.Second), err)
	}
	// Если «Курьер в пути» стабильно не принимается (например, уже выставлен вручную), идём дальше.
	if a.next == StageLastMile && e.Fails >= 3 {
		w.log.Add("warn", "Заказ %s: считаю этап «Курьер в пути» выполненным и перехожу к ожиданию конца окна.", a.p.PostingNumber)
		w.markDone(a, now)
	}
}

// buildViews — таблица для интерфейса из журнала и время ближайшего дела.
func (w *Worker) buildViews(now time.Time) ([]PostingView, time.Time) {
	var views []PostingView
	var nextDue time.Time
	pause := time.Duration(w.prof.PauseSec) * time.Second
	for _, e := range w.journal.All() {
		if e.Final != "" {
			if now.Sub(e.FinalAt) > 24*time.Hour {
				continue
			}
		} else if e.Stage < 0 && e.Warning == "" || now.Sub(e.SeenAt) > 3*24*time.Hour {
			continue
		}
		v := PostingView{PostingNumber: e.PostingNumber, OrderNumber: e.OrderNumber, Created: e.Created, Status: e.Status, Substatus: e.Substatus,
			Stage: e.Stage, Final: e.Final, WindowBegin: e.WindowBegin, WindowEnd: e.WindowEnd, Warning: e.Warning, Error: e.LastError,
			Batch: w.num(e.Batch), Manual: e.Manual, Picked: e.Picked, NoCount: e.NoCount}
		for i := 0; i <= StageDelivered; i++ {
			v.Done[i] = e.Stage >= 0 && i <= e.Stage
			v.Plan[i] = e.DoneAt[i]
		}
		if e.Final == "" && e.Stage >= 0 && e.Stage < StageDelivered {
			due := e.DueAt
			if e.NextTry.After(due) && !due.IsZero() {
				due = e.NextTry
			}
			v.NextAction, v.NextAt = actionNames[e.Stage+1], due
			// план оставшихся шагов
			t := due
			for i := e.Stage + 1; i <= StageDelivered && !t.IsZero(); i++ {
				if i == StageDelivered && i > e.Stage+1 && !e.WindowEnd.IsZero() {
					if dt := e.WindowEnd.Add(-time.Duration(w.prof.MarginMin) * time.Minute); dt.After(t) {
						t = dt
					}
				}
				v.Plan[i] = t
				t = t.Add(pause)
			}
			if !due.IsZero() && (nextDue.IsZero() || due.Before(nextDue)) {
				nextDue = due
			}
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool {
		fi, fj := views[i].Final != "", views[j].Final != ""
		if fi != fj {
			return !fi
		}
		if !fi {
			return views[i].NextAt.Before(views[j].NextAt)
		}
		return views[i].Created.After(views[j].Created)
	})
	if len(views) > 300 {
		views = views[:300]
	}
	return views, nextDue
}

func fmtWindow(b, e time.Time) string {
	if e.IsZero() {
		return "не указано"
	}
	if b.IsZero() {
		return "до " + e.Local().Format("02.01 15:04")
	}
	return b.Local().Format("02.01 15:04") + " – " + e.Local().Format("15:04")
}

type WorkerStatus struct {
	Running  bool          `json:"running"`
	Live     bool          `json:"live"`
	Started  time.Time     `json:"started"`
	LastPoll time.Time     `json:"last_poll"`
	NextPoll time.Time     `json:"next_poll"`
	LastErr  string        `json:"last_error"`
	Postings []PostingView `json:"postings"`
	Batches  []BatchView   `json:"batches"`
	Skipped  []SkipView    `json:"skipped"`

	Paused    bool   `json:"paused"`
	Finishing bool   `json:"finishing"`
	StockOff  bool   `json:"stock_off"`
	QueueName string `json:"queue_name,omitempty"`
}

func (w *Worker) Status() WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WorkerStatus{Running: true, Live: w.prof.Live, Started: w.started, LastPoll: w.lastPoll, NextPoll: w.nextPoll,
		LastErr: w.lastErr, Postings: w.views, Skipped: w.skipped, Batches: w.bviews}
}

// ---------- управление работниками всех магазинов ----------

type Manager struct {
	dir     string
	store   *Store
	mocks   *MockRegistry
	opMu    sync.Mutex // запуск/остановка — по одному
	mu      sync.Mutex
	workers map[string]*Worker
	logs    map[string]*ShopLog
}

func NewManager(dir string, store *Store, mocks *MockRegistry) *Manager {
	return &Manager{dir: dir, store: store, mocks: mocks, workers: map[string]*Worker{}, logs: map[string]*ShopLog{}}
}

func (m *Manager) Log(id string) *ShopLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.logs[id]
	if l == nil {
		l = NewShopLog(m.dir, id)
		m.logs[id] = l
	}
	return l
}

func (m *Manager) journalPath(p *Profile) string {
	mode := "test"
	if p.Live {
		mode = "live"
	}
	return filepath.Join(m.dir, "journal", p.ID+"_"+mode+".json")
}

func (m *Manager) Start(id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.start(id)
}

// start/stop вызываются под opMu: два одновременных запуска не создадут двух работников на один магазин.
func (m *Manager) start(id string) error {
	p, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("магазин не найден")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	m.stop(id, false)
	w := NewWorker(p, NewClient(&p, m.mocks), LoadJournal(m.journalPath(&p)), m.Log(id))
	w.store = m.store
	w.onDone = func() { go m.workerDone(id, w) }
	m.mu.Lock()
	m.workers[id] = w
	m.mu.Unlock()
	w.Start()
	m.store.Update(id, func(pp *Profile) { pp.Running = true })
	m.updateAwake()
	return nil
}

func (m *Manager) Stop(id string, persist bool) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.stop(id, persist)
}

func (m *Manager) stop(id string, persist bool) {
	m.mu.Lock()
	w := m.workers[id]
	delete(m.workers, id)
	m.mu.Unlock()
	if w != nil {
		w.Stop()
	}
	if persist {
		m.store.Update(id, func(pp *Profile) { pp.Running = false })
	}
	m.updateAwake()
}

// workerDone останавливает работника, у которого очередь окончена.
func (m *Manager) workerDone(id string, w *Worker) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	same := m.workers[id] == w
	m.mu.Unlock()
	if same {
		m.stop(id, false)
	}
}

// updateAwake не даёт Windows уснуть, пока работает автоматизация хотя бы одного магазина.
func (m *Manager) updateAwake() {
	m.mu.Lock()
	n := len(m.workers)
	m.mu.Unlock()
	keepAwake(n > 0)
}

func (m *Manager) StopAll() {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	m.mu.Lock()
	var ws []*Worker
	for _, w := range m.workers {
		ws = append(ws, w)
	}
	m.mu.Unlock()
	for _, w := range ws {
		w.Stop()
	}
}

func (m *Manager) Status(id string) WorkerStatus {
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	p, _ := m.store.Get(id)
	var st WorkerStatus
	if w == nil {
		st = WorkerStatus{Live: p.Live}
	} else {
		st = w.Status()
		st.Paused, st.Finishing, st.StockOff = p.Paused, p.Finishing, p.StockOff
	}
	st.QueueName = p.QueueName
	return st
}

// ResetInfo — что произойдёт при сбросе очереди (для предупреждения).
type ResetInfo struct {
	Zero      bool        `json:"zero"`        // будет ли обнуляться остаток
	OnSale    []Batch     `json:"on_sale"`     // пары в продаже
	Undeliver []ResetItem `json:"undelivered"` // заказы в работе, которые будут брошены
	Live      bool        `json:"live"`
}

type ResetItem struct {
	PostingNumber string    `json:"posting_number"`
	Stage         string    `json:"stage"`
	WindowEnd     time.Time `json:"window_end"`
	Products      string    `json:"products"`
}

func (m *Manager) resetInfo(id string) (ResetInfo, Profile, error) {
	p, ok := m.store.Get(id)
	if !ok {
		return ResetInfo{}, p, fmt.Errorf("магазин не найден")
	}
	info := ResetInfo{Zero: !p.KeepOnReset, Live: p.Live}
	batches := p.Batches
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	if w != nil { // в тестовом режиме партии в продаже — в симуляции
		if st := w.Status(); len(st.Batches) > 0 {
			batches = nil
			for _, b := range st.Batches {
				batches = append(batches, b.Batch)
			}
		}
	}
	for _, b := range batches {
		if b.State == BatchActive || b.State == BatchNext {
			info.OnSale = append(info.OnSale, b)
		}
	}
	j := LoadJournal(m.journalPath(&p))
	if w != nil {
		j = w.journal // у работающей автоматизации журнал свежее, чем на диске
	}
	for _, e := range j.All() {
		if e.Final == "" && e.Stage >= 0 && !e.Abandoned {
			info.Undeliver = append(info.Undeliver, ResetItem{PostingNumber: e.PostingNumber, Stage: stageNames[e.Stage], WindowEnd: e.WindowEnd, Products: e.Products})
		}
	}
	sort.Slice(info.Undeliver, func(i, j int) bool { return info.Undeliver[i].WindowEnd.Before(info.Undeliver[j].WindowEnd) })
	return info, p, nil
}

func (m *Manager) ResetPreview(id string) (ResetInfo, error) {
	info, _, err := m.resetInfo(id)
	return info, err
}

// StartQueue — кнопка «Начать»: с сохранённой очереди (tplID) или с партий в таблице.
func (m *Manager) StartQueue(id, tplID string) error { return m.StartQueueNamed(id, tplID, "") }

// StartQueueNamed — «Начать» с партиями из таблицы; name — имя сохранённой очереди, которой заполнили таблицу.
func (m *Manager) StartQueueNamed(id, tplID, name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.IsRunning(id) {
		return fmt.Errorf("очередь уже идёт")
	}
	var lerr error
	_, err := m.store.Update(id, func(p *Profile) {
		var tpl *QueueTemplate
		if tplID != "" {
			for i := range p.Templates {
				if p.Templates[i].ID == tplID {
					tpl = &p.Templates[i]
				}
			}
			if tpl == nil {
				lerr = fmt.Errorf("сохранённая очередь не найдена")
				return
			}
		}
		now := time.Now()
		var out []Batch
		for _, b := range p.Batches {
			switch b.State {
			case BatchDone, BatchSkip: // прошлая очередь: её заказы, если остались, всё равно доводятся по журналу
				continue
			case BatchActive, BatchNext:
				if tpl != nil { // пара прошлой очереди ещё в продаже — завершаем, программа обнулит её остаток
					b.State, b.FinishedAt, b.ZeroDone, b.Keep = BatchDone, now, false, false
				}
			case BatchQueued:
				if tpl != nil {
					continue
				}
			}
			out = append(out, b)
		}
		if tpl != nil {
			for _, row := range tpl.Rows {
				out = append(out, Batch{ID: newID(), SKU1: row.SKU1, SKU2: row.SKU2, Qty: row.Qty, State: BatchQueued})
			}
			name = tpl.Name
		}
		open := 0
		for _, b := range out {
			if b.State != BatchDone {
				open++
			}
		}
		if open == 0 {
			lerr = fmt.Errorf("в очереди нет партий — выберите сохранённую очередь или добавьте партии")
			return
		}
		p.Batches, p.QueueName, p.QueueStarted = out, name, now
		p.Paused, p.Finishing, p.StockOff = false, false, false
	})
	if err == nil {
		err = lerr
	}
	if err != nil {
		return err
	}
	if name != "" {
		m.Log(id).Add("info", "Начата очередь «%s».", name)
	} else {
		m.Log(id).Add("info", "Начата очередь из таблицы партий.")
	}
	return m.start(id)
}

// AppendQueue — кнопка «+ Добавить очередь»: пары сохранённой очереди встают в конец текущей.
// Начатые и завершённые партии не меняются, нумерация продолжается. Если очередь идёт —
// новые пары выставляются по обычным правилам (сразу, если в продаже ничего нет, или заранее
// по порогу из настроек). Если очередь уже закончилась и работа остановлена — она продолжается
// с новыми парами. tplID == "" — без новых пар: продолжить с партиями «в очереди» из таблицы
// (name — имя сохранённой очереди, которой заполнили таблицу).
func (m *Manager) AppendQueue(id, tplID, name string) (added int, err error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	running := m.IsRunning(id)
	var lerr error
	_, err = m.store.Update(id, func(p *Profile) {
		if running && p.Finishing {
			lerr = fmt.Errorf("очередь заканчивается — добавить пары нельзя")
			return
		}
		var tpl *QueueTemplate
		if tplID != "" {
			for i := range p.Templates {
				if p.Templates[i].ID == tplID {
					tpl = &p.Templates[i]
				}
			}
			if tpl == nil {
				lerr = fmt.Errorf("сохранённая очередь не найдена")
				return
			}
		} else if running {
			lerr = fmt.Errorf("выберите сохранённую очередь")
			return
		}
		out := append([]Batch(nil), p.Batches...)
		if tpl != nil {
			for _, row := range tpl.Rows {
				out = append(out, Batch{ID: newID(), SKU1: row.SKU1, SKU2: row.SKU2, Qty: row.Qty, State: BatchQueued})
			}
			added, name = len(tpl.Rows), tpl.Name
		}
		open := 0
		for _, b := range out {
			if b.State == BatchQueued || b.State == BatchActive || b.State == BatchNext {
				open++
			}
		}
		if open == 0 {
			lerr = fmt.Errorf("новых партий нет — выберите сохранённую очередь или добавьте партии")
			return
		}
		p.Batches = out
		if name != "" {
			if p.QueueName == "" {
				p.QueueName = name
			} else {
				p.QueueName += " + " + name
			}
		}
		if !running {
			p.QueueStarted = time.Now()
			p.Paused, p.Finishing, p.StockOff = false, false, false
		}
	})
	if err == nil {
		err = lerr
	}
	if err != nil {
		return 0, err
	}
	if added > 0 {
		m.Log(id).Add("info", "Добавлена очередь «%s»: %d пар(ы) в конец текущей.", name, added)
	} else if name != "" {
		m.Log(id).Add("info", "Добавлена очередь «%s».", name)
	}
	if running {
		m.Kick(id)
		return added, nil
	}
	m.Log(id).Add("info", "Очередь продолжена.")
	return added, m.start(id)
}

// ClearQueue — кнопка «Очистить»: убрать все партии прошлой очереди, чтобы начать с нуля.
// Только когда очередь не идёт. Журнал заказов не трогается.
func (m *Manager) ClearQueue(id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.IsRunning(id) {
		return fmt.Errorf("очередь идёт — сначала закончите её")
	}
	_, err := m.store.Update(id, func(p *Profile) {
		p.Batches, p.QueueName = nil, ""
	})
	if err == nil {
		m.Log(id).Add("info", "Список партий очищен.")
	}
	return err
}

// SetPaused — кнопки «Пауза» / «Продолжить». Остаток обнуляет и возвращает работник в ближайшем цикле.
func (m *Manager) SetPaused(id string, paused bool) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.IsRunning(id) {
		return fmt.Errorf("очередь не запущена")
	}
	var lerr error
	_, err := m.store.Update(id, func(p *Profile) {
		if p.Finishing {
			lerr = fmt.Errorf("очередь заканчивается — пауза недоступна")
			return
		}
		p.Paused = paused
	})
	if err == nil {
		err = lerr
	}
	if err != nil {
		return err
	}
	if paused {
		m.Log(id).Add("info", "Очередь поставлена на паузу.")
	} else {
		m.Log(id).Add("info", "Очередь продолжена.")
	}
	m.Kick(id)
	return nil
}

// FinishDeliver — «Закончить» → «Доставить оставшиеся и закончить»: пары в продаже обнуляются (если включено),
// новые партии не запускаются; когда все заказы доставлены, программа останавливается сама.
func (m *Manager) FinishDeliver(id string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !m.IsRunning(id) {
		return fmt.Errorf("очередь не запущена")
	}
	if _, err := m.store.Update(id, func(p *Profile) {
		// Если была пауза, остаток уже обнулён: возвращать его не нужно.
		p.Finishing, p.Paused, p.StockOff = true, false, false
	}); err != nil {
		return err
	}
	m.Log(id).Add("info", "Очередь заканчивается: новые партии не запускаются, оставшиеся заказы будут доставлены, затем программа остановится.")
	m.Kick(id)
	return nil
}

// FinishAbandon — «Закончить» → «Бросить сейчас»: работа останавливается, пары в продаже обнуляются (если включено),
// недоставленные заказы бросаются. Партии в очереди помечаются «не начата».
func (m *Manager) FinishAbandon(ctx context.Context, id string) (info ResetInfo, warning string, err error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	info, p, err := m.resetInfo(id)
	if err != nil {
		return info, "", err
	}
	m.stop(id, true)
	log := m.Log(id)
	var errs []string
	zeroed := map[string]bool{}
	if info.Zero && p.WarehouseID != 0 {
		c := NewClient(&p, m.mocks)
		c.readOnly = !p.Live
		w := NewWorker(p, c, LoadJournal(m.journalPath(&p)), log)
		for i := range info.OnSale {
			if err := w.setPairStock(ctx, &info.OnSale[i], 0); err != nil {
				errs = append(errs, fmt.Sprintf("SKU %d + %d: %v", info.OnSale[i].SKU1, info.OnSale[i].SKU2, err))
			} else {
				zeroed[info.OnSale[i].ID] = true
			}
		}
	}
	j := LoadJournal(m.journalPath(&p))
	now := time.Now()
	for _, e := range j.All() {
		if e.Final == "" && e.Stage >= 0 && !e.Abandoned {
			e.Abandoned, e.Final, e.FinalAt, e.DueAt = true, "брошен при завершении очереди", now, time.Time{}
		}
	}
	j.MarkDirty()
	j.Save()
	m.store.Update(id, func(pp *Profile) {
		pp.Running, pp.Paused, pp.Finishing, pp.StockOff = false, false, false, false
		if !pp.Live {
			return // в тестовом режиме состояние партий не сохраняется
		}
		for i := range pp.Batches {
			b := &pp.Batches[i]
			switch b.State {
			case BatchActive, BatchNext:
				b.State, b.FinishedAt = BatchDone, now
				b.ZeroDone, b.Keep = zeroed[b.ID] || !info.Zero, !info.Zero
			case BatchQueued:
				b.State = BatchSkip
			}
		}
	})
	log.Add("warn", "Очередь закончена досрочно: программа остановлена. Брошено недоставленных заказов: %d.%s",
		len(info.Undeliver), map[bool]string{true: " Остаток пар в продаже обнулён.", false: ""}[info.Zero && len(info.OnSale) > 0 && len(errs) == 0])
	for _, u := range info.Undeliver {
		log.Add("warn", "Заказ %s брошен (этап «%s», окно до %s) — статусы больше не меняются, переведите его вручную.", u.PostingNumber, u.Stage, u.WindowEnd.Local().Format("02.01 15:04"))
	}
	if len(errs) > 0 {
		warning = "Очередь закончена, но остаток обнулить не удалось: " + strings.Join(errs, "; ") + ". Обнулите его вручную в кабинете Ozon."
		log.Add("err", "%s", warning)
	}
	return info, warning, nil
}

// SwitchNext — кнопка «Следующая партия».
func (m *Manager) SwitchNext(id string) error {
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	if w == nil {
		return fmt.Errorf("сначала начните очередь — переключение партий выполняет она")
	}
	w.RequestSwitch()
	return nil
}

func (m *Manager) Kick(id string) {
	m.mu.Lock()
	w := m.workers[id]
	m.mu.Unlock()
	if w != nil {
		w.Kick()
	}
}

func (m *Manager) IsRunning(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[id] != nil
}

// ResetTest очищает журнал тестового прогона.
func (m *Manager) ResetTest(id string) error {
	p, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("магазин не найден")
	}
	tp := p
	tp.Live = false
	m.opMu.Lock()
	defer m.opMu.Unlock()
	running := m.IsRunning(id) && !p.Live
	if running {
		m.stop(id, false)
	}
	err := LoadJournal(m.journalPath(&tp)).Reset()
	m.Log(id).Add("info", "Журнал тестового прогона очищен.")
	if running {
		return m.start(id)
	}
	return err
}

// ApplyStock заново выставляет остаток активной партии (в тестовом режиме только описывает действие).
func (m *Manager) ApplyStock(ctx context.Context, id string, qty int) ([]string, error) {
	p, ok := m.store.Get(id)
	if !ok {
		return nil, fmt.Errorf("магазин не найден")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if qty < 0 {
		return nil, fmt.Errorf("количество не может быть отрицательным")
	}
	b := p.ActiveBatch()
	if b == nil {
		return nil, fmt.Errorf("нет партии в продаже — начните очередь, программа выставит остаток первой партии сама")
	}
	c := NewClient(&p, m.mocks)
	c.readOnly = !p.Live
	w := NewWorker(p, c, LoadJournal(m.journalPath(&p)), m.Log(id))
	var msgs []string
	seq := m.Log(id).LastSeq()
	err := w.setPairStock(ctx, b, qty)
	for _, ln := range m.Log(id).Since(seq) {
		msgs = append(msgs, ln.Text)
	}
	return msgs, err
}
