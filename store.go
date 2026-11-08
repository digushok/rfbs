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
	SKU1          int64  `json:"sku1"`
	SKU2          int64  `json:"sku2"`
	StockQty      int    `json:"stock_qty"`

	Live         bool `json:"live"`          // false — тестовый режим
	PauseSec     int  `json:"pause_sec"`     // пауза между шагами
	MarginMin    int  `json:"margin_min"`    // запас до конца окна доставки
	PollSec      int  `json:"poll_sec"`      // как часто опрашивать Ozon
	LookbackDays int  `json:"lookback_days"` // за сколько дней смотреть заказы
	Running      bool `json:"running"`       // автоматизация включена (восстанавливается после перезапуска)
}

func (p *Profile) normalize() {
	if p.PauseSec <= 0 {
		p.PauseSec = 60
	}
	if p.MarginMin < 0 {
		p.MarginMin = 0
	}
	if p.PollSec < 15 {
		p.PollSec = 30
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
	if p.SKU1 <= 0 || p.SKU2 <= 0 {
		return fmt.Errorf("не указаны оба SKU")
	}
	if p.SKU1 == p.SKU2 {
		return fmt.Errorf("SKU должны быть разными")
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
	p := &Profile{ID: newID(), Name: name, MarginMin: 3}
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
		SKU1: mockSKU1, SKU2: mockSKU2, StockQty: 5, MarginMin: 3}
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
	return *p, true
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
	return *p, s.saveLocked()
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
		list = append(list, *p)
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
	SeenAt        time.Time    `json:"seen_at"`
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
	Entries map[string]*JEntry `json:"entries"`
}

func LoadJournal(path string) *Journal {
	j := &Journal{path: path, Entries: map[string]*JEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, j)
		if j.Entries == nil {
			j.Entries = map[string]*JEntry{}
		}
	}
	return j
}

func (j *Journal) Get(pn string) *JEntry {
	j.mu.Lock()
	defer j.mu.Unlock()
	e := j.Entries[pn]
	if e == nil {
		e = &JEntry{PostingNumber: pn, Stage: -1}
		j.Entries[pn] = e
	}
	return e
}

func (j *Journal) Save() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	cut := time.Now().Add(-45 * 24 * time.Hour)
	for k, e := range j.Entries {
		if !e.SeenAt.IsZero() && e.SeenAt.Before(cut) {
			delete(j.Entries, k)
		}
	}
	b, _ := json.MarshalIndent(j, "", " ")
	return writeFileAtomic(j.path, b)
}

func (j *Journal) Reset() error {
	j.mu.Lock()
	j.Entries = map[string]*JEntry{}
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
	if f, err := os.OpenFile(l.file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintf(f, "%s [%s] %s\n", ln.Time.Format("2006-01-02 15:04:05"), level, ln.Text)
		f.Close()
	}
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
