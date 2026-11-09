package main

// HTTP API для веб-интерфейса (только 127.0.0.1).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	dir   string
	store *Store
	mgr   *Manager
}

func NewServer(dir string, store *Store, mgr *Manager) *Server {
	return &Server{dir: dir, store: store, mgr: mgr}
}

type apiFunc func(r *http.Request, body map[string]any) (any, error)

func (s *Server) handle(mux *http.ServeMux, path string, fn apiFunc) {
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-App", "ozon-rfbs")
		// Защита от запросов с чужих сайтов (в том числе через подмену DNS): только своя страница.
		if h := r.Host; !strings.HasPrefix(h, "127.0.0.1:") && !strings.HasPrefix(h, "localhost:") {
			w.WriteHeader(403)
			return
		}
		// Защита от запросов с чужих сайтов: принимаем только со своей страницы.
		if o := r.Header.Get("Origin"); o != "" && !strings.HasPrefix(o, "http://127.0.0.1:") && !strings.HasPrefix(o, "http://localhost:") {
			w.WriteHeader(403)
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			w.WriteHeader(415)
			return
		}
		body := map[string]any{}
		if r.Method == http.MethodPost {
			json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
		}
		out, err := fn(r, body)
		if err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(out)
	})
}

func str(b map[string]any, k string) string {
	switch v := b[k].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return ""
}

func num(b map[string]any, k string) (int64, bool) {
	v, ok := b[k]
	if !ok || v == nil {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return 0, true
		}
		n, err := strconv.ParseInt(t, 10, 64)
		return n, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// parseRows — строки очереди из запроса: SKU 1, SKU 2, количество.
func parseRows(v any) ([]TplRow, error) {
	list, _ := v.([]any)
	var rows []TplRow
	for i, x := range list {
		m, _ := x.(map[string]any)
		if m == nil {
			continue
		}
		a, _ := num(m, "sku1")
		c, _ := num(m, "sku2")
		q, _ := num(m, "qty")
		if a == 0 && c == 0 && q == 0 {
			continue // пустая строка
		}
		b := Batch{SKU1: a, SKU2: c, Qty: int(q)}
		if err := b.Validate(); err != nil {
			return nil, fmt.Errorf("строка %d: %v", i+1, err)
		}
		if q <= 0 {
			return nil, fmt.Errorf("строка %d: укажите количество больше 0", i+1)
		}
		rows = append(rows, TplRow{SKU1: a, SKU2: c, Qty: int(q)})
	}
	return rows, nil
}

func (s *Server) id(r *http.Request, b map[string]any) string {
	if v := str(b, "id"); v != "" {
		return v
	}
	if v := r.URL.Query().Get("id"); v != "" {
		return v
	}
	a, _ := s.store.Snapshot()
	return a
}

type profileOut struct {
	Profile
	APIKey    string `json:"api_key"`
	APIKeySet bool   `json:"api_key_set"`
}

func ctx30() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

func (s *Server) Routes(mux *http.ServeMux) {
	s.handle(mux, "/api/ping", func(r *http.Request, b map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	})

	s.handle(mux, "/api/state", func(r *http.Request, b map[string]any) (any, error) {
		active, list := s.store.Snapshot()
		var shops []map[string]any
		var cur *profileOut
		for _, p := range list {
			shops = append(shops, map[string]any{"id": p.ID, "name": p.Name, "demo": p.Demo, "live": p.Live, "running": s.mgr.IsRunning(p.ID)})
			if p.ID == active {
				cur = &profileOut{Profile: p, APIKeySet: p.APIKey != ""}
			}
		}
		return map[string]any{"version": appVersion, "repo": repoURL, "data_dir": s.dir, "active": active, "shops": shops, "profile": cur}, nil
	})

	s.handle(mux, "/api/profile/save", func(r *http.Request, b map[string]any) (any, error) {
		id := s.id(r, b)
		old, ok := s.store.Get(id)
		if !ok {
			return nil, fmt.Errorf("магазин не найден")
		}
		p, err := s.store.Update(id, func(p *Profile) {
			if _, ok := b["name"]; ok {
				p.Name = str(b, "name")
			}
			if _, ok := b["client_id"]; ok {
				p.ClientID = str(b, "client_id")
			}
			if k := str(b, "api_key"); k != "" {
				p.APIKey = k
			}
			if v, ok := num(b, "warehouse_id"); ok {
				p.WarehouseID = v
				p.WarehouseName = str(b, "warehouse_name")
			}
			if v, ok := b["keep_old_stock"].(bool); ok {
				p.KeepOldStock = v
			}
			if v, ok := b["keep_on_reset"].(bool); ok {
				p.KeepOnReset = v
			}
			if v, ok := num(b, "prestock_left"); ok && v >= 0 {
				n := int(v)
				p.PrestockLeft = &n
			}
			if v, ok := num(b, "pause_sec"); ok {
				p.PauseSec = int(v)
			}
			if v, ok := num(b, "margin_min"); ok {
				p.MarginMin = int(v)
			}
			if v, ok := num(b, "poll_sec"); ok {
				p.PollSec = int(v)
			}
			if v, ok := num(b, "lookback_days"); ok {
				p.LookbackDays = int(v)
			}
			if v, ok := b["live"].(bool); ok {
				p.Live = v
			}
		})
		if err != nil {
			return nil, err
		}
		if old.Live != p.Live {
			if p.Live {
				s.mgr.Log(id).Add("warn", "Включён БОЕВОЙ режим.")
			} else {
				s.mgr.Log(id).Add("info", "Включён тестовый режим.")
			}
		}
		// Перезапуск автоматизации с новыми настройками.
		if s.mgr.IsRunning(id) {
			if err := s.mgr.Start(id); err != nil {
				s.mgr.Stop(id, true)
				return nil, fmt.Errorf("настройки сохранены, но автоматизация остановлена: %v", err)
			}
		}
		return map[string]any{"ok": true}, nil
	})

	s.handle(mux, "/api/profile/new", func(r *http.Request, b map[string]any) (any, error) {
		var p *Profile
		if v, _ := b["demo"].(bool); v {
			p = newDemoProfile()
		} else {
			p = newProfile("Новый магазин")
		}
		if n := str(b, "name"); n != "" {
			p.Name = n
		}
		return map[string]any{"id": p.ID}, s.store.Add(p)
	})

	s.handle(mux, "/api/profile/select", func(r *http.Request, b map[string]any) (any, error) {
		return map[string]any{"ok": true}, s.store.SetActive(str(b, "id"))
	})

	s.handle(mux, "/api/profile/delete", func(r *http.Request, b map[string]any) (any, error) {
		id := str(b, "id")
		s.mgr.Stop(id, false)
		return map[string]any{"ok": true}, s.store.Delete(id)
	})

	s.handle(mux, "/api/check", func(r *http.Request, b map[string]any) (any, error) {
		p, ok := s.store.Get(s.id(r, b))
		if !ok {
			return nil, fmt.Errorf("магазин не найден")
		}
		if !p.Demo && (p.ClientID == "" || p.APIKey == "") {
			return nil, fmt.Errorf("укажите Client-Id и API-ключ")
		}
		ctx, cancel := ctx30()
		defer cancel()
		c := NewClient(&p, s.mgr.mocks)
		names, methods, err := c.Roles(ctx)
		if err != nil {
			return nil, err
		}
		need := []string{"/v4/posting/fbs/list", "/v3/posting/fbs/get", "/v4/posting/fbs/ship", "/v2/fbs/posting/delivering", "/v2/fbs/posting/last-mile",
			"/v2/fbs/posting/delivered", "/v2/products/stocks", "/v3/product/info/list"}
		var missing []string
		if len(methods) > 0 {
			for _, m := range need {
				if !methods[m] {
					missing = append(missing, m)
				}
			}
		}
		return map[string]any{"ok": true, "roles": names, "missing": missing}, nil
	})

	s.handle(mux, "/api/warehouses", func(r *http.Request, b map[string]any) (any, error) {
		p, ok := s.store.Get(s.id(r, b))
		if !ok {
			return nil, fmt.Errorf("магазин не найден")
		}
		ctx, cancel := ctx30()
		defer cancel()
		ws, err := NewClient(&p, s.mgr.mocks).Warehouses(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"warehouses": ws}, nil
	})

	s.handle(mux, "/api/products", func(r *http.Request, b map[string]any) (any, error) {
		p, ok := s.store.Get(s.id(r, b))
		if !ok {
			return nil, fmt.Errorf("магазин не найден")
		}
		var skus []int64
		seen := map[int64]bool{}
		for _, bt := range p.Batches {
			for _, v := range []int64{bt.SKU1, bt.SKU2} {
				if v > 0 && !seen[v] {
					seen[v] = true
					skus = append(skus, v)
				}
			}
		}
		if len(skus) == 0 {
			return map[string]any{"products": []any{}}, nil
		}
		ctx, cancel := ctx30()
		defer cancel()
		c := NewClient(&p, s.mgr.mocks)
		infos, err := c.ProductsBySKU(ctx, skus)
		if err != nil {
			return nil, err
		}
		stockErr := ""
		if p.WarehouseID != 0 {
			rows, err := c.StocksFBS(ctx, skus)
			if err != nil {
				stockErr = err.Error()
			}
			for _, row := range rows {
				if row.WarehouseID != p.WarehouseID || infos[row.SKU] == nil {
					continue
				}
				pr, rs := row.Present, row.Reserved
				infos[row.SKU].Present, infos[row.SKU].Reserved = &pr, &rs
			}
		}
		var out []any
		for _, sku := range skus {
			if pi := infos[sku]; pi != nil {
				out = append(out, pi)
			} else {
				out = append(out, map[string]any{"sku": sku, "missing": true})
			}
		}
		return map[string]any{"products": out, "stock_error": stockErr}, nil
	})

	s.handle(mux, "/api/batches/save", func(r *http.Request, b map[string]any) (any, error) {
		id := s.id(r, b)
		list, _ := b["batches"].([]any)
		var queued []Batch
		for i, x := range list {
			m, _ := x.(map[string]any)
			if m == nil {
				continue
			}
			bt := Batch{ID: str(m, "id"), State: BatchQueued}
			bt.SKU1, _ = num(m, "sku1")
			bt.SKU2, _ = num(m, "sku2")
			q, _ := num(m, "qty")
			bt.Qty = int(q)
			if err := bt.Validate(); err != nil {
				return nil, fmt.Errorf("строка %d: %v", i+1, err)
			}
			if bt.Qty <= 0 {
				return nil, fmt.Errorf("строка %d: укажите количество больше 0", i+1)
			}
			queued = append(queued, bt)
		}
		_, err := s.store.Update(id, func(p *Profile) {
			// Начатые партии не трогаем. keep_ids — партии, которые окно показывает как начатые
			// (в тестовом режиме они начаты только в симуляции, а в настройках ещё в очереди).
			keep := map[string]bool{}
			if ids, ok := b["keep_ids"].([]any); ok {
				for _, x := range ids {
					if v, ok := x.(string); ok {
						keep[v] = true
					}
				}
			}
			var out []Batch
			started := map[string]bool{}
			for _, bt := range p.Batches {
				if bt.Started() || keep[bt.ID] {
					out = append(out, bt)
					started[bt.ID] = true
				}
			}
			for _, q := range queued { // партию могли запустить, пока вы редактировали очередь
				if !started[q.ID] {
					out = append(out, q)
				}
			}
			p.Batches = out
		})
		if err != nil {
			return nil, err
		}
		s.mgr.Log(id).Add("info", "Очередь партий сохранена: в очереди %d.", len(queued))
		s.mgr.Kick(id)
		return map[string]any{"ok": true}, nil
	})

	s.handle(mux, "/api/batches/remove", func(r *http.Request, b map[string]any) (any, error) {
		id, bid := s.id(r, b), str(b, "batch_id")
		var err error
		_, uerr := s.store.Update(id, func(p *Profile) {
			var out []Batch
			for _, bt := range p.Batches {
				if bt.ID == bid {
					if bt.State == BatchActive || bt.State == BatchNext {
						err = fmt.Errorf("партию, которая сейчас в продаже, удалить нельзя — сначала переключитесь на следующую")
					} else {
						continue
					}
				}
				out = append(out, bt)
			}
			p.Batches = out
		})
		if uerr != nil {
			return nil, uerr
		}
		return map[string]any{"ok": true}, err
	})

	s.handle(mux, "/api/batches/reset-preview", func(r *http.Request, b map[string]any) (any, error) {
		return s.mgr.ResetPreview(s.id(r, b))
	})

	s.handle(mux, "/api/batches/reset", func(r *http.Request, b map[string]any) (any, error) {
		ctx, cancel := ctx30()
		defer cancel()
		info, warning, err := s.mgr.ResetQueue(ctx, s.id(r, b))
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "warning": warning, "abandoned": len(info.Undeliver)}, nil
	})

	s.handle(mux, "/api/templates/save", func(r *http.Request, b map[string]any) (any, error) {
		id := s.id(r, b)
		t, _ := b["template"].(map[string]any)
		if t == nil {
			return nil, fmt.Errorf("нет данных очереди")
		}
		tpl := QueueTemplate{ID: str(t, "id"), Name: str(t, "name")}
		if tpl.Name == "" {
			return nil, fmt.Errorf("укажите имя очереди")
		}
		rows, err := parseRows(t["rows"])
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("в очереди нет ни одной пары")
		}
		tpl.Rows = rows
		var dup bool
		_, uerr := s.store.Update(id, func(p *Profile) {
			for _, x := range p.Templates {
				if x.ID != tpl.ID && strings.EqualFold(strings.TrimSpace(x.Name), tpl.Name) {
					dup = true
					return
				}
			}
			for i := range p.Templates {
				if tpl.ID != "" && p.Templates[i].ID == tpl.ID {
					p.Templates[i] = tpl
					return
				}
			}
			tpl.ID = newID()
			p.Templates = append(p.Templates, tpl)
		})
		if uerr != nil {
			return nil, uerr
		}
		if dup {
			return nil, fmt.Errorf("очередь с именем «%s» уже есть — выберите другое имя", tpl.Name)
		}
		return map[string]any{"ok": true, "id": tpl.ID}, nil
	})

	s.handle(mux, "/api/templates/delete", func(r *http.Request, b map[string]any) (any, error) {
		tid := str(b, "template_id")
		_, err := s.store.Update(s.id(r, b), func(p *Profile) {
			var out []QueueTemplate
			for _, x := range p.Templates {
				if x.ID != tid {
					out = append(out, x)
				}
			}
			p.Templates = out
		})
		return map[string]any{"ok": true}, err
	})

	s.handle(mux, "/api/templates/load", func(r *http.Request, b map[string]any) (any, error) {
		id, tid := s.id(r, b), str(b, "template_id")
		keep := map[string]bool{}
		if ids, ok := b["keep_ids"].([]any); ok {
			for _, x := range ids {
				if v, ok := x.(string); ok {
					keep[v] = true
				}
			}
		}
		var name string
		var n int
		var lerr error
		_, err := s.store.Update(id, func(p *Profile) {
			var tpl *QueueTemplate
			for i := range p.Templates {
				if p.Templates[i].ID == tid {
					tpl = &p.Templates[i]
				}
			}
			if tpl == nil {
				lerr = fmt.Errorf("очередь не найдена")
				return
			}
			var out []Batch
			for _, bt := range p.Batches { // начатые партии не трогаем, заменяем только «в очереди»
				if bt.Started() || keep[bt.ID] {
					out = append(out, bt)
				}
			}
			for _, row := range tpl.Rows {
				out = append(out, Batch{ID: newID(), SKU1: row.SKU1, SKU2: row.SKU2, Qty: row.Qty, State: BatchQueued})
			}
			p.Batches = out
			name, n = tpl.Name, len(tpl.Rows)
		})
		if err == nil {
			err = lerr
		}
		if err != nil {
			return nil, err
		}
		s.mgr.Log(id).Add("info", "Загружена очередь «%s»: %d пар(ы) в очереди.", name, n)
		s.mgr.Kick(id)
		return map[string]any{"ok": true}, nil
	})

	s.handle(mux, "/api/batches/next", func(r *http.Request, b map[string]any) (any, error) {
		return map[string]any{"ok": true}, s.mgr.SwitchNext(s.id(r, b))
	})

	s.handle(mux, "/api/stock", func(r *http.Request, b map[string]any) (any, error) {
		qty, ok := num(b, "qty")
		if !ok {
			return nil, fmt.Errorf("укажите количество")
		}
		ctx, cancel := ctx30()
		defer cancel()
		msgs, err := s.mgr.ApplyStock(ctx, s.id(r, b), int(qty))
		if err != nil {
			return nil, err
		}
		return map[string]any{"messages": msgs}, nil
	})

	s.handle(mux, "/api/run", func(r *http.Request, b map[string]any) (any, error) {
		return s.mgr.Status(s.id(r, b)), nil
	})
	s.handle(mux, "/api/run/start", func(r *http.Request, b map[string]any) (any, error) {
		return map[string]any{"ok": true}, s.mgr.Start(s.id(r, b))
	})
	s.handle(mux, "/api/run/stop", func(r *http.Request, b map[string]any) (any, error) {
		s.mgr.Stop(s.id(r, b), true)
		return map[string]any{"ok": true}, nil
	})
	s.handle(mux, "/api/run/now", func(r *http.Request, b map[string]any) (any, error) {
		s.mgr.Kick(s.id(r, b))
		return map[string]any{"ok": true}, nil
	})
	s.handle(mux, "/api/test/reset", func(r *http.Request, b map[string]any) (any, error) {
		return map[string]any{"ok": true}, s.mgr.ResetTest(s.id(r, b))
	})

	s.handle(mux, "/api/demo/order", func(r *http.Request, b map[string]any) (any, error) {
		id := s.id(r, b)
		p, ok := s.store.Get(id)
		if !ok || !p.Demo {
			return nil, fmt.Errorf("доступно только в демо-магазине")
		}
		m := s.mgr.mocks.Get(id)
		m.mu.Lock()
		pp := m.AddPosting(str(b, "kind"))
		m.mu.Unlock()
		s.mgr.Log(id).Add("info", "Демо: создан заказ %s (%s).", pp.PostingNumber, productsText(pp))
		s.mgr.Kick(id)
		return map[string]any{"posting_number": pp.PostingNumber}, nil
	})

	s.handle(mux, "/api/log", func(r *http.Request, b map[string]any) (any, error) {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		lines := s.mgr.Log(s.id(r, b)).Since(after)
		if lines == nil {
			lines = []LogLine{}
		}
		return map[string]any{"lines": lines}, nil
	})

	s.handle(mux, "/api/update", func(r *http.Request, b map[string]any) (any, error) {
		return checkUpdate()
	})

	s.handle(mux, "/api/open-dir", func(r *http.Request, b map[string]any) (any, error) {
		openFolder(s.dir)
		return map[string]any{"ok": true}, nil
	})

	s.handle(mux, "/api/quit", func(r *http.Request, b map[string]any) (any, error) {
		go func() {
			time.Sleep(300 * time.Millisecond)
			s.mgr.StopAll()
			os.Exit(0)
		}()
		return map[string]any{"ok": true}, nil
	})
}
