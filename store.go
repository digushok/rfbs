package main

// Хранение настроек магазинов, журналов заказов и логов в постоянной папке.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Profile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ClientID string `json:"client_id"`
	APIKey   string `json:"api_key"`
	Demo     bool   `json:"demo"`

	WarehouseID   int64  `json:"warehouse_id"`
	WarehouseName string `json:"warehouse_name"`
	// Партии — пары SKU по очереди. Старые поля SKU1/SKU2/StockQty — только для перехода с версий до 1.2.
	Batches       []Batch         `json:"batches"`
	KeepOldStock  bool            `json:"keep_old_stock"`  // false (по умолчанию) — обнулять остаток старой пары при переключении
	PrestockLeft  *int            `json:"prestock_left"`   // за сколько заказов до конца партии выставлять следующую (nil — 3)
	KeepOnReset   bool            `json:"keep_on_reset"`   // false (по умолчанию) — при сбросе очереди обнулять остаток пар в продаже
	Templates     []QueueTemplate `json:"templates"`       // сохранённые очереди
	StockCheckMin int             `json:"stock_check_min"` // как часто проверять остаток завершённых пар, мин. (0 — 10)
	CancelForeign bool            `json:"cancel_foreign"`  // отменять нецелевые заказы с SKU из очереди
	SKU1          int64           `json:"sku1,omitempty"`
	SKU2          int64           `json:"sku2,omitempty"`
	StockQty      int             `json:"stock_qty,omitempty"`

	Live         bool      `json:"live"`                 // false — тестовый режим
	PauseSec     int       `json:"pause_sec"`            // пауза между шагами
	MarginMin    int       `json:"margin_min"`           // запас до конца окна доставки
	PollSec      int       `json:"poll_sec"`             // как часто опрашивать Ozon
	LookbackDays int       `json:"lookback_days"`        // за сколько дней смотреть заказы
	Running      bool      `json:"running"`              // очередь идёт (восстанавливается после перезапуска)
	Paused       bool      `json:"paused"`               // очередь на паузе: статусы не меняются, остаток пар в продаже обнулён
	Finishing    bool      `json:"finishing"`            // «Закончить»: новые партии не запускаются, оставшиеся заказы доводятся до конца
	StockOff     bool      `json:"stock_off"`            // остаток пар в продаже обнулён на время паузы (при «Продолжить» вернуть)
	QueueName    string    `json:"queue_name,omitempty"` // имя сохранённой очереди, с которой начали
	QueueStarted time.Time `json:"queue_started"`        // когда нажали «Начать»
}

// QueueTemplate — сохранённая очередь: имя и пары SKU с количеством.
type QueueTemplate struct {
	ID   string   `json:"id"`
	Name string   `json:"name"`
	Rows []TplRow `json:"rows"`
}

type TplRow struct {
	SKU1 int64 `json:"sku1"`
	SKU2 int64 `json:"sku2"`
	Qty  int   `json:"qty"`
}

// Состояния партии.
const (
	BatchQueued = "queued"  // в очереди
	BatchActive = "active"  // продаётся сейчас (основная)
	BatchNext   = "next"    // следующая, выставлена заранее, пока основная распродаётся
	BatchDone   = "done"    // распродана или переключена: заказы доводятся до конца, новые не ждём
	BatchSkip   = "skipped" // очередь закончили раньше — партия так и не начиналась
)

type Batch struct {
	ID         string    `json:"id"`
	SKU1       int64     `json:"sku1"`
	SKU2       int64     `json:"sku2"`
	Qty        int       `json:"qty"`  // сколько комплектов выставить
	Sold       int       `json:"sold"` // сколько целевых заказов пришло (без отменённых)
	State      string    `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	ZeroDone   bool      `json:"zero_done,omitempty"` // остаток обнулён после завершения
	Keep       bool      `json:"keep,omitempty"`      // остаток после завершения не обнулять (так настроено)
}

func (b *Batch) Matches(a, c int64) bool {
	return (b.SKU1 == a && b.SKU2 == c) || (b.SKU1 == c && b.SKU2 == a)
}

// Started — партия уже выставлялась (её заказы считаются целевыми).
func (b *Batch) Started() bool {
	return b.State == BatchActive || b.State == BatchNext || b.State == BatchDone
}

const defaultPrestock = 3

// Prestock — за сколько заказов до конца основной партии выставлять следующую (0 — только после распродажи).
func (p *Profile) Prestock() int {
	if p.PrestockLeft == nil {
		return defaultPrestock
	}
	return *p.PrestockLeft
}

// clone — независимая копия (списки партий и очередей не делятся с оригиналом).
func (p *Profile) clone() Profile {
	c := *p
	c.Batches = append([]Batch(nil), p.Batches...)
	c.Templates = make([]QueueTemplate, len(p.Templates))
	for i, t := range p.Templates {
		t.Rows = append([]TplRow(nil), t.Rows...)
		c.Templates[i] = t
	}
	if p.PrestockLeft != nil {
		v := *p.PrestockLeft
		c.PrestockLeft = &v
	}
	return c
}

func (p *Profile) ActiveBatch() *Batch {
	for i := range p.Batches {
		if p.Batches[i].State == BatchActive {
			return &p.Batches[i]
		}
	}
	return nil
}

func (p *Profile) normalize() {
	// Переход с одной пары на партии.
	if len(p.Batches) == 0 && p.SKU1 > 0 && p.SKU2 > 0 {
		p.Batches = []Batch{{ID: newID(), SKU1: p.SKU1, SKU2: p.SKU2, Qty: p.StockQty, State: BatchActive}}
	}
	p.SKU1, p.SKU2, p.StockQty = 0, 0, 0
	for i := range p.Batches {
		if p.Batches[i].ID == "" {
			p.Batches[i].ID = newID()
		}
		if p.Batches[i].State == "" {
			p.Batches[i].State = BatchQueued
		}
	}
	if p.PauseSec <= 0 {
		p.PauseSec = 60
	}
	if p.MarginMin < 0 {
		p.MarginMin = 0
	}
	if p.PollSec < 15 {
		p.PollSec = 30
	}
	if p.StockCheckMin <= 0 {
		p.StockCheckMin = 10
	}
	if p.LookbackDays <= 0 {
		p.LookbackDays = 14
	}
	if p.Name == "" {
		p.Name = "Магазин"
	}
}

// Validate проверяет, что для автоматизации всё заполнено.
func (p *Profile) Validate() error {
	if !p.Demo && (p.ClientID == "" || p.APIKey == "") {
		return fmt.Errorf("не указаны Client-Id и API-ключ (вкладка «Настройки»)")
	}
	if p.WarehouseID == 0 {
		return fmt.Errorf("не выбран склад")
	}
	if len(p.Batches) == 0 {
		return fmt.Errorf("не добавлено ни одной партии (пары SKU)")
	}
	for i, b := range p.Batches {
		if err := b.Validate(); err != nil {
			return fmt.Errorf("партия %d: %v", i+1, err)
		}
	}
	return nil
}

func (b *Batch) Validate() error {
	if b.SKU1 <= 0 || b.SKU2 <= 0 {
		return fmt.Errorf("не указаны оба SKU")
	}
	if b.SKU1 == b.SKU2 {
		return fmt.Errorf("SKU должны быть разными")
	}
	if b.Qty < 0 {
		return fmt.Errorf("количество не может быть отрицательным")
	}
	return nil
}

type Store struct {
	dir      string
	mu       sync.Mutex
	Active   string     `json:"active"`
	Profiles []*Profile `json:"profiles"`
}

func dataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		exe, _ := os.Executable()
		base = filepath.Dir(exe)
	}
	d := filepath.Join(base, "OzonRFBS")
	os.MkdirAll(filepath.Join(d, "logs"), 0o700)
	os.MkdirAll(filepath.Join(d, "journal"), 0o700)
	return d
}

func LoadStore(dir string) *Store {
	s := &Store{dir: dir}
	if b, err := os.ReadFile(filepath.Join(dir, "profiles.json")); err == nil {
		json.Unmarshal(b, s)
	}
	for _, p := range s.Profiles {
		p.normalize()
	}
	if len(s.Profiles) == 0 {
		p := newDemoProfile()
		s.Profiles = append(s.Profiles, p)
		s.Active = p.ID
		s.saveLocked()
	}
	if s.find(s.Active) == nil {
		s.Active = s.Profiles[0].ID
	}
	return s
}

func newProfile(name string) *Profile {
	p := &Profile{ID: newID(), Name: name, MarginMin: 3, Live: true} // обычная работа; тестовый режим — в настройках
	p.normalize()
	return p
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newDemoProfile() *Profile {
	p := &Profile{ID: newID(), Name: "Демо-магазин", Demo: true,
		WarehouseID: mockWarehouseID, WarehouseName: mockWarehouseName,
		MarginMin: 3, Batches: []Batch{
			{SKU1: mockSKU1, SKU2: mockSKU2, Qty: 5},
			{SKU1: mockSKU3, SKU2: mockSKU4, Qty: 3},
			{SKU1: mockSKU5, SKU2: mockSKU6, Qty: 4},
		}}
	p.normalize()
	return p
}

func (s *Store) find(id string) *Profile {
	for _, p := range s.Profiles {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// Get возвращает копию профиля.
func (s *Store) Get(id string) (Profile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	if p == nil {
		return Profile{}, false
	}
	return p.clone(), true
}

func (s *Store) Update(id string, fn func(p *Profile)) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.find(id)
	if p == nil {
		return Profile{}, fmt.Errorf("магазин не найден")
	}
	fn(p)
	p.normalize()
	return p.clone(), s.saveLocked()
}

func (s *Store) Add(p *Profile) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.normalize()
	s.Profiles = append(s.Profiles, p)
	s.Active = p.ID
	return s.saveLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Profile
	for _, p := range s.Profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	s.Profiles = out
	if len(s.Profiles) == 0 {
		p := newDemoProfile()
		s.Profiles = append(s.Profiles, p)
	}
	if s.find(s.Active) == nil {
		s.Active = s.Profiles[0].ID
	}
	return s.saveLocked()
}

func (s *Store) SetActive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.find(id) == nil {
		return fmt.Errorf("магазин не найден")
	}
	s.Active = id
	return s.saveLocked()
}

func (s *Store) Snapshot() (active string, list []Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.Profiles {
		list = append(list, p.clone())
	}
	return s.Active, list
}

func (s *Store) saveLocked() error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return writeFileAtomic(filepath.Join(s.dir, "profiles.json"), b)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- журнал обработанных заказов ----------

// Этапы обработки отправления.
const (
	StageNew        = 0 // ожидает сборки
	StageShipped    = 1 // собран (ожидает отгрузки)
	StageDelivering = 2 // доставляется
	StageLastMile   = 3 // курьер в пути
	StageDelivered  = 4 // условно доставлен
)

type JEntry struct {
	PostingNumber string       `json:"posting_number"`
	Stage         int          `json:"stage"`
	DoneAt        [5]time.Time `json:"done_at"` // когда программа выполнила этап
	Fails         int          `json:"fails"`
	LastError     string       `json:"last_error,omitempty"`
	NextTry       time.Time    `json:"next_try"` // после ошибки — не раньше этого времени
	DueAt         time.Time    `json:"due_at"`   // когда запланирован следующий шаг
	Warning       string       `json:"warning,omitempty"`
	Final         string       `json:"final,omitempty"` // заказ завершён: доставлен, отменён...
	FinalAt       time.Time    `json:"final_at"`
	SeenAt        time.Time    `json:"seen_at"`             // когда последний раз получали данные из Ozon
	Batch         string       `json:"batch,omitempty"`     // партия, к которой привязан заказ
	Uncounted     bool         `json:"uncounted,omitempty"` // отменён и вычтен из «продано»
	NoCount       bool         `json:"no_count,omitempty"`  // не учитывается в «продано» (пара из партии в очереди)
	Manual        [5]bool      `json:"manual"`              // этап выполнен не программой (нажат вручную)
	ManualAt      time.Time    `json:"manual_at"`           // когда программа заметила ручной шаг (от него — пауза)
	Picked        bool         `json:"picked,omitempty"`    // заказ подхвачен: часть этапов сделана вручную
	Abandoned     bool         `json:"abandoned,omitempty"` // брошен при сбросе очереди: программа его больше не трогает

	// Сведения для отображения (последние данные из Ozon).
	OrderNumber string    `json:"order_number,omitempty"`
	Products    string    `json:"products,omitempty"`
	Status      string    `json:"status,omitempty"`
	Substatus   string    `json:"substatus,omitempty"`
	Invalid     int       `json:"invalid,omitempty"` // сколько раз подряд Ozon ответил «invalid transition» на «Условно доставлен»
	Created     time.Time `json:"created"`
	WindowBegin time.Time `json:"window_begin"`
	WindowEnd   time.Time `json:"window_end"`
}

func (e *JEntry) LastActionAt() time.Time {
	var t time.Time
	for _, d := range e.DoneAt {
		if d.After(t) {
			t = d
		}
	}
	return t
}

type Journal struct {
	path    string
	mu      sync.Mutex
	dirty   bool
	Entries map[string]*JEntry `json:"entries"`
	Foreign map[string]*FEntry `json:"foreign,omitempty"` // нецелевые заказы, по которым программа что-то сделала
}

// FEntry — что программа сделала с нецелевым заказом (пополнение или отмена) — чтобы не повторять.
type FEntry struct {
	Action     string        `json:"action"` // replenished, cancelled
	At         time.Time     `json:"at"`
	Restore    map[int64]int `json:"restore,omitempty"` // после отмены: SKU (не из пар в продаже) → какой остаток вернуть
	SKUs       []int64       `json:"skus,omitempty"`    // SKU заказа, остаток которых проверяется после отмены
	VerifyAt   time.Time     `json:"verify_at,omitempty"`
	VerifyLeft int           `json:"verify_left,omitempty"`
}

func LoadJournal(path string) *Journal {
	j := &Journal{path: path, Entries: map[string]*JEntry{}, Foreign: map[string]*FEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, j)
		if j.Entries == nil {
			j.Entries = map[string]*JEntry{}
		}
		if j.Foreign == nil {
			j.Foreign = map[string]*FEntry{}
		}
	}
	return j
}

// Get возвращает запись (создаёт новую). Изменения записей сохраняются при Save после MarkDirty.
func (j *Journal) Get(pn string) *JEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	e := j.Entries[pn]
	if e == nil {
		e = &JEntry{PostingNumber: pn, Stage: -1}
		j.Entries[pn] = e
		j.dirty = true
	}
	return e
}

func (j *Journal) Peek(pn string) *JEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Entries[pn]
}

func (j *Journal) All() []*JEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]*JEntry, 0, len(j.Entries))
	for _, e := range j.Entries {
		out = append(out, e)
	}
	return out
}

// ForeignGet / ForeignSet — записи о нецелевых заказах (только из цикла работника).
func (j *Journal) ForeignGet(pn string) *FEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Foreign[pn]
}

func (j *Journal) ForeignSet(pn string, f *FEntry) {
	j.mu.Lock()
	j.Foreign[pn] = f
	j.dirty = true
	j.mu.Unlock()
}

func (j *Journal) ForeignAll() map[string]*FEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make(map[string]*FEntry, len(j.Foreign))
	for k, v := range j.Foreign {
		out[k] = v
	}
	return out
}

func (j *Journal) MarkDirty() {
	j.mu.Lock()
	j.dirty = true
	j.mu.Unlock()
}

// Save записывает журнал на диск, если он менялся. Старые завершённые заказы удаляются.
func (j *Journal) Save() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.dirty {
		return nil
	}
	now := time.Now()
	for k, e := range j.Entries {
		if (e.Final != "" && now.Sub(e.FinalAt) > 7*24*time.Hour) || (!e.SeenAt.IsZero() && now.Sub(e.SeenAt) > 45*24*time.Hour) {
			delete(j.Entries, k)
		}
	}
	for k, f := range j.Foreign {
		if now.Sub(f.At) > 7*24*time.Hour {
			delete(j.Foreign, k)
		}
	}
	b, _ := json.Marshal(j)
	if err := writeFileAtomic(j.path, b); err != nil {
		return err
	}
	j.dirty = false
	return nil
}

func (j *Journal) Reset() error {
	j.mu.Lock()
	j.Entries = map[string]*JEntry{}
	j.Foreign = map[string]*FEntry{}
	j.dirty = true
	j.mu.Unlock()
	return j.Save()
}

// ---------- лог ----------

type LogLine struct {
	Seq   int64     `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // info, ok, warn, err, test
	Text  string    `json:"text"`
}

type ShopLog struct {
	mu    sync.Mutex
	seq   int64
	lines []LogLine
	file  string
}

func NewShopLog(dir, shopID string) *ShopLog {
	return &ShopLog{file: filepath.Join(dir, "logs", "shop_"+shopID+".log")}
}

func (l *ShopLog) Add(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seq++
	ln := LogLine{Seq: l.seq, Time: time.Now(), Level: level, Text: fmt.Sprintf(format, args...)}
	l.lines = append(l.lines, ln)
	if len(l.lines) > 3000 {
		l.lines = l.lines[len(l.lines)-2000:]
	}
	if l.seq%200 == 0 {
		if st, err := os.Stat(l.file); err == nil && st.Size() > 10<<20 {
			os.Remove(l.file + ".old")
			os.Rename(l.file, l.file+".old")
		}
	}
	if f, err := os.OpenFile(l.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s [%s] %s\n", ln.Time.Format("2006-01-02 15:04:05"), level, ln.Text)
		f.Close()
	}
}

func (l *ShopLog) LastSeq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (l *ShopLog) Since(seq int64) []LogLine {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []LogLine
	for _, ln := range l.lines {
		if ln.Seq > seq {
			out = append(out, ln)
		}
	}
	return out
}
