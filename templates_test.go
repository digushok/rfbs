package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func apiCall(t *testing.T, mux *http.ServeMux, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	req.Host = "127.0.0.1:8766"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTemplatesSaveLoad(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir)
	mgr := NewManager(dir, store, NewMockRegistry())
	mux := http.NewServeMux()
	NewServer(dir, store, mgr).Routes(mux)
	id, _ := store.Snapshot()
	// Первая партия уже в продаже, две в очереди.
	store.Update(id, func(p *Profile) { p.Batches[0].State = BatchActive })

	rows := []map[string]any{{"sku1": "111", "sku2": "222", "qty": "25"}, {"sku1": 333, "sku2": 444, "qty": 10}, {}}
	code, out := apiCall(t, mux, "/api/templates/save", map[string]any{"id": id, "template": map[string]any{"name": "Чехлы весна", "rows": rows}})
	if code != 200 {
		t.Fatalf("сохранение: %d %v", code, out)
	}
	tid := out["id"].(string)
	// Имя должно быть уникальным.
	if code, _ := apiCall(t, mux, "/api/templates/save", map[string]any{"id": id, "template": map[string]any{"name": "чехлы весна", "rows": rows}}); code == 200 {
		t.Fatalf("повтор имени должен отклоняться")
	}
	// Неверная строка.
	if code, _ := apiCall(t, mux, "/api/templates/save", map[string]any{"id": id, "template": map[string]any{"name": "X", "rows": []map[string]any{{"sku1": 1, "sku2": 1, "qty": 3}}}}); code == 200 {
		t.Fatalf("одинаковые SKU должны отклоняться")
	}
	// Загрузка заменяет только «в очереди».
	if code, out := apiCall(t, mux, "/api/templates/load", map[string]any{"id": id, "template_id": tid}); code != 200 {
		t.Fatalf("загрузка: %v", out)
	}
	p, _ := store.Get(id)
	if len(p.Batches) != 3 || p.Batches[0].State != BatchActive || p.Batches[0].SKU1 != mockSKU1 ||
		p.Batches[1].SKU1 != 111 || p.Batches[1].Qty != 25 || p.Batches[2].SKU1 != 333 || p.Batches[2].State != BatchQueued {
		t.Fatalf("неверная очередь после загрузки: %+v", p.Batches)
	}
	// Правка шаблона не меняет загруженную очередь.
	apiCall(t, mux, "/api/templates/save", map[string]any{"id": id, "template": map[string]any{"id": tid, "name": "Чехлы весна", "rows": rows[:1]}})
	p, _ = store.Get(id)
	if len(p.Templates) != 1 || len(p.Templates[0].Rows) != 1 || len(p.Batches) != 3 {
		t.Fatalf("правка шаблона: %+v", p.Templates)
	}
	apiCall(t, mux, "/api/templates/delete", map[string]any{"id": id, "template_id": tid})
	p, _ = store.Get(id)
	if len(p.Templates) != 0 {
		t.Fatalf("шаблон не удалён")
	}
}
