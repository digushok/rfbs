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
	wh := p.DeliveryMethod.WarehouseID
	if wh == 0 && p.AnalyticsData != nil {
		wh = p.AnalyticsData.WarehouseID
	}
	if wh != prof.WarehouseID {
		return fmt.Sprintf("другой склад (%d)", wh)
	}
	if len(p.Products) != 2 {
		return fmt.Sprintf("в заказе %d поз., нужно ровно 2", len(p.Products))
	}
	got := map[int64]int{}
	for _, pr := range p.Products {
		got[pr.SKU] += pr.Quantity
		if pr.Quantity != 1 {
			return fmt.Sprintf("SKU %d в количестве %d шт., нужно 1", pr.SKU, pr.Quantity)
		}
	}
	if len(got) != 2 || got[prof.SKU1] != 1 || got[prof.SKU2] != 1 {
		var s []string
		for k := range got {
			s = append(s, fmt.Sprint(k))
		}
		sort.Strings(s)
		return "другой состав: SKU " + strings.Join(s, ", ")
	}
	return ""
}

type PostingView struct {
	PostingNumber string       `json:"posting_number"`
	OrderNumber   string       `json:"order_number"`
	Created       time.Time    `json:"created"`
	Status        string       `json:"status"`
	Substatus     string       `json:"substatus"`
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
}

type SkipView struct {
	PostingNumber string    `json:"posting_number"`
	Status        string    `json:"status"`
	Reason        string    `json:"reason"`
	Created       time.Time `json:"created"`
	Products      string    `json:"products"`
}

type Worker struct {
	prof    Profile
	client  *Client
	journal *Journal
	log     *ShopLog
	cancel  context.CancelFunc
	done    chan struct{}
	kick    chan struct{}
	now     func() time.Time

	mu        sync.Mutex
	started   time.Time
	lastPoll  time.Time
	nextPoll  time.Time
	lastErr   string
	views     []PostingView
	skipped   []SkipView
	loggedFor map[string]string // что уже записано в лог по отправлению, чтобы не повторяться
}

func NewWorker(prof Profile, client *Client, journal *Journal, log *ShopLog) *Worker {
	client.readOnly = !prof.Live
	return &Worker{prof: prof, client: client, journal: journal, log: log, now: time.Now,
		kick: make(chan struct{}, 1), loggedFor: map[string]string{}}
}

func (w *Worker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	w.started = w.now()
	mode := "ТЕСТОВЫЙ режим: в Ozon ничего не меняется, действия только симулируются"
	if w.prof.Live {
		mode = "БОЕВОЙ режим: статусы в Ozon меняются по-настоящему"
	}
	w.log.Add("info", "Автоматизация запущена. %s. Склад %d, SKU %d + %d, пауза %d сек, запас %d мин.",
		mode, w.prof.WarehouseID, w.prof.SKU1, w.prof.SKU2, w.prof.PauseSec, w.prof.MarginMin)
	go func() {
		defer close(w.done)
		for {
			next := w.Tick(ctx)
			wait := time.Duration(w.prof.PollSec) * time.Second
			if !next.IsZero() {
				if d := next.Sub(w.now()) + 2*time.Second; d < wait {
					wait = d
				}
			}
			if wait < 3*time.Second {
				wait = 3 * time.Second
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
		w.log.Add("info", "Автоматизация остановлена.")
	}
}

func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *Worker) logOnce(pn, key, level, format string, args ...any) {
	w.mu.Lock()
	if w.loggedFor[pn+"|"+key] != "" {
		w.mu.Unlock()
		return
	}
	w.loggedFor[pn+"|"+key] = "1"
	w.mu.Unlock()
	w.log.Add(level, format, args...)
}

func productsText(p *Posting) string {
	var s []string
	for _, pr := range p.Products {
		s = append(s, fmt.Sprintf("%d×%d", pr.SKU, pr.Quantity))
	}
	return strings.Join(s, " + ")
}

// Tick — один цикл: прочитать заказы и выполнить всё, что пора. Возвращает время ближайшего дела.
func (w *Worker) Tick(ctx context.Context) time.Time {
	now := w.now()
	since := now.AddDate(0, 0, -w.prof.LookbackDays)
	postings, err := w.client.ListPostings(ctx, w.prof.WarehouseID, since, now.Add(24*time.Hour))
	w.mu.Lock()
	w.lastPoll = now
	w.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}
		}
		w.mu.Lock()
		changed := w.lastErr != err.Error()
		w.lastErr = err.Error()
		w.mu.Unlock()
		if changed {
			w.log.Add("err", "Не удалось получить заказы: %v", err)
		}
		return time.Time{}
	}
	w.mu.Lock()
	if w.lastErr != "" {
		w.log.Add("ok", "Связь с Ozon восстановлена.")
	}
	w.lastErr = ""
	w.mu.Unlock()

	var views []PostingView
	var skipped []SkipView
	var nextDue time.Time
	for i := range postings {
		p := &postings[i]
		if ctx.Err() != nil {
			return time.Time{}
		}
		if reason := checkTarget(p, &w.prof); reason != "" {
			_, ok, final := realStage(p)
			if ok && final == "" {
				skipped = append(skipped, SkipView{PostingNumber: p.PostingNumber, Status: p.Status, Reason: reason, Created: p.InProcessAt.Time, Products: productsText(p)})
				w.logOnce(p.PostingNumber, "skip", "info", "Заказ %s пропущен — не целевой: %s.", p.PostingNumber, reason)
			}
			continue
		}
		v, due := w.process(ctx, p, now)
		views = append(views, v)
		if !due.IsZero() && (nextDue.IsZero() || due.Before(nextDue)) {
			nextDue = due
		}
	}
	w.journal.Save()
	sort.Slice(views, func(i, j int) bool {
		fi, fj := views[i].Final != "", views[j].Final != ""
		if fi != fj {
			return !fi
		}
		return views[i].Created.After(views[j].Created)
	})
	if len(views) > 200 {
		views = views[:200]
	}
	sort.Slice(skipped, func(i, j int) bool { return skipped[i].Created.After(skipped[j].Created) })
	w.mu.Lock()
	w.views, w.skipped = views, skipped
	w.mu.Unlock()
	return nextDue
}

// dueFor — когда можно выполнить шаг next.
func (w *Worker) dueFor(next int, e *JEntry, end time.Time, now time.Time) (time.Time, string) {
	pause := time.Duration(w.prof.PauseSec) * time.Second
	last := e.LastActionAt()
	earliest := now
	if !last.IsZero() {
		earliest = last.Add(pause)
	}
	if next < StageDelivered {
		return earliest, ""
	}
	if end.IsZero() {
		return time.Time{}, "Ozon не вернул окно доставки — перевод в «Условно доставлен» не запланирован"
	}
	target := end.Add(-time.Duration(w.prof.MarginMin) * time.Minute)
	if target.Before(earliest) {
		target = earliest
	}
	return target, ""
}

func (w *Worker) process(ctx context.Context, p *Posting, now time.Time) (PostingView, time.Time) {
	begin, end := p.Window()
	v := PostingView{PostingNumber: p.PostingNumber, OrderNumber: p.OrderNumber, Created: p.InProcessAt.Time,
		Status: p.Status, Substatus: p.Substatus, WindowBegin: begin, WindowEnd: end}
	e := w.journal.Get(p.PostingNumber)
	e.SeenAt = now
	rs, ok, final := realStage(p)
	stage := e.Stage
	if ok && rs > stage {
		stage = rs
	}
	if e.Stage < 0 && ok && final == "" {
		w.logOnce(p.PostingNumber, "found", "info", "Найден целевой заказ %s (%s), статус Ozon: %s, окно доставки: %s.",
			p.PostingNumber, productsText(p), p.Status, fmtWindow(begin, end))
	}
	if stage > e.Stage && ok {
		e.Stage = stage
	}
	v.Stage = stage
	for i := 0; i <= StageDelivered; i++ {
		v.Done[i] = i <= stage && stage >= 0
		v.Plan[i] = e.DoneAt[i]
	}
	v.Error = e.LastError
	if !w.prof.Live && final == "" && e.Stage > rs && rs >= 0 {
		v.Warning = "Этапы после «" + stageNames[rs] + "» выполнены только в тесте"
	}

	if final != "" || !ok || stage >= StageDelivered {
		if final == "" && !ok {
			final = "статус " + p.Status
		}
		if final == "" {
			final = "условно доставлен"
		}
		v.Final = final
		return v, time.Time{}
	}

	next := stage + 1
	due, warn := w.dueFor(next, e, end, now)
	if warn != "" {
		v.Warning = warn
		w.logOnce(p.PostingNumber, "nowindow", "warn", "Заказ %s: %s.", p.PostingNumber, warn)
	}
	if !due.IsZero() && !due.After(now) {
		if next == StageDelivered && !end.IsZero() && now.After(end) {
			w.logOnce(p.PostingNumber, "late", "warn", "Заказ %s: окно доставки уже закончилось (%s), перевожу в «Условно доставлен» сразу.",
				p.PostingNumber, end.Local().Format("02.01 15:04"))
		}
		err := w.act(ctx, p, e, next, now)
		if err == nil {
			stage = next
			v.Stage, v.Done[next], v.Plan[next], v.Error = stage, true, now, ""
			if stage >= StageDelivered {
				v.Final = "условно доставлен"
				return v, time.Time{}
			}
			next = stage + 1
			due, _ = w.dueFor(next, e, end, now)
		} else {
			v.Error = e.LastError
			due = now.Add(time.Duration(w.prof.PollSec) * time.Second)
		}
	}
	// план для оставшихся шагов
	t := due
	pause := time.Duration(w.prof.PauseSec) * time.Second
	for i := next; i <= StageDelivered && !t.IsZero(); i++ {
		if i == StageDelivered && !end.IsZero() {
			if dt := end.Add(-time.Duration(w.prof.MarginMin) * time.Minute); dt.After(t) {
				t = dt
			}
		}
		v.Plan[i] = t
		t = t.Add(pause)
	}
	v.NextAction = actionNames[next]
	v.NextAt = due
	return v, due
}

func (w *Worker) act(ctx context.Context, p *Posting, e *JEntry, next int, now time.Time) error {
	name := actionNames[next]
	if !w.prof.Live {
		w.log.Add("test", "[ТЕСТ] Заказ %s: «%s» — сделал бы запрос %s. В Ozon ничего не отправлено.", p.PostingNumber, name, actionPaths[next])
		e.Stage, e.DoneAt[next], e.Fails, e.LastError = next, now, 0, ""
		return nil
	}
	var err error
	if next == StageShipped {
		err = w.client.Ship(ctx, p)
	} else {
		err = w.client.SetStatus(ctx, actionPaths[next], p.PostingNumber)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		e.Fails++
		e.LastError = fmt.Sprintf("«%s»: %v", name, err)
		w.log.Add("err", "Заказ %s: не удалось «%s» (попытка %d): %v", p.PostingNumber, name, e.Fails, err)
		// Если «Курьер в пути» стабильно не принимается (например, уже выставлен вручную), идём дальше.
		if next == StageLastMile && e.Fails >= 3 {
			w.log.Add("warn", "Заказ %s: считаю этап «Курьер в пути» выполненным и перехожу к ожиданию конца окна.", p.PostingNumber)
			e.Stage, e.DoneAt[next], e.Fails = next, now, 0
		}
		return err
	}
	e.Stage, e.DoneAt[next], e.Fails, e.LastError = next, now, 0, ""
	w.log.Add("ok", "Заказ %s: «%s» — выполнено.", p.PostingNumber, name)
	return nil
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
	Skipped  []SkipView    `json:"skipped"`
}

func (w *Worker) Status() WorkerStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	return WorkerStatus{Running: true, Live: w.prof.Live, Started: w.started, LastPoll: w.lastPoll, NextPoll: w.nextPoll,
		LastErr: w.lastErr, Postings: w.views, Skipped: w.skipped}
}

// ---------- управление работниками всех магазинов ----------

type Manager struct {
	dir     string
	store   *Store
	mocks   *MockRegistry
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
	p, ok := m.store.Get(id)
	if !ok {
		return fmt.Errorf("магазин не найден")
	}
	if err := p.Validate(); err != nil {
		return err
	}
	m.Stop(id, false)
	w := NewWorker(p, NewClient(&p, m.mocks), LoadJournal(m.journalPath(&p)), m.Log(id))
	m.mu.Lock()
	m.workers[id] = w
	m.mu.Unlock()
	w.Start()
	m.store.Update(id, func(pp *Profile) { pp.Running = true })
	return nil
}

func (m *Manager) Stop(id string, persist bool) {
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
}

func (m *Manager) StopAll() {
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
	if w == nil {
		p, _ := m.store.Get(id)
		return WorkerStatus{Live: p.Live}
	}
	return w.Status()
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
	running := m.IsRunning(id) && !p.Live
	if running {
		m.Stop(id, false)
	}
	err := LoadJournal(m.journalPath(&tp)).Reset()
	m.Log(id).Add("info", "Журнал тестового прогона очищен.")
	if running {
		return m.Start(id)
	}
	return err
}

// ApplyStock выставляет одинаковый остаток обоим SKU (в тестовом режиме только описывает действие).
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
	m.store.Update(id, func(pp *Profile) { pp.StockQty = qty })
	c := NewClient(&p, m.mocks)
	c.readOnly = !p.Live
	log := m.Log(id)
	infos, err := c.ProductsBySKU(ctx, []int64{p.SKU1, p.SKU2})
	if err != nil {
		return nil, err
	}
	var msgs []string
	for _, sku := range []int64{p.SKU1, p.SKU2} {
		pi := infos[sku]
		if pi == nil {
			return msgs, fmt.Errorf("товар с SKU %d не найден в магазине", sku)
		}
		if !p.Live {
			s := fmt.Sprintf("[ТЕСТ] Выставил бы остаток %d шт. для SKU %d (артикул «%s», product_id %d) на складе %d. В Ozon ничего не отправлено.",
				qty, sku, pi.OfferID, pi.ProductID, p.WarehouseID)
			log.Add("test", "%s", s)
			msgs = append(msgs, s)
			continue
		}
		if err := c.SetStock(ctx, p.WarehouseID, pi, qty); err != nil {
			log.Add("err", "Остаток SKU %d: ошибка: %v", sku, err)
			return msgs, fmt.Errorf("SKU %d: %v", sku, err)
		}
		s := fmt.Sprintf("Остаток SKU %d (артикул «%s») на складе %d: %d шт.", sku, pi.OfferID, p.WarehouseID, qty)
		log.Add("ok", "%s", s)
		msgs = append(msgs, s)
	}
	return msgs, nil
}
