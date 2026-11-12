package main

// Клиент Ozon Seller API. Только те методы, которые нужны программе.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ozonBase = "https://api-seller.ozon.ru"

// Изменяющие запросы. В тестовом режиме клиент физически не отправляет их в Ozon.
var mutatingPaths = map[string]bool{
	"/v2/products/stocks":        true,
	"/v4/posting/fbs/ship":       true,
	"/v2/fbs/posting/delivering": true,
	"/v2/fbs/posting/last-mile":  true,
	"/v2/fbs/posting/delivered":  true,
	"/v2/posting/fbs/cancel":     true,
}

var ErrReadOnly = errors.New("тестовый режим: изменяющий запрос заблокирован")

type OzonError struct {
	Status  int
	Path    string
	Code    string
	Message string
}

func (e *OzonError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Status == 404 && strings.Contains(strings.ToLower(msg), "page not found") {
		return fmt.Sprintf("Ozon %s: метод не найден (HTTP 404) — вероятно, Ozon его отключил, нужна новая версия программы", e.Path)
	}
	return fmt.Sprintf("Ozon %s: HTTP %d: %s", e.Path, e.Status, msg)
}

type Client struct {
	base     string
	clientID string
	apiKey   string
	http     *http.Client
	readOnly bool

	offsetMu sync.Mutex
	offset   time.Duration // часы Ozon минус часы компьютера
	hasOff   bool
}

// ClockOffset — насколько часы Ozon впереди часов компьютера (по заголовку Date).
func (c *Client) ClockOffset() (time.Duration, bool) {
	c.offsetMu.Lock()
	defer c.offsetMu.Unlock()
	return c.offset, c.hasOff
}

func (c *Client) noteDate(h string, sent, got time.Time) {
	if h == "" {
		return
	}
	t, err := http.ParseTime(h)
	if err != nil {
		return
	}
	// Date с точностью до секунды: берём середину запроса и +0,5 с на округление.
	local := sent.Add(got.Sub(sent) / 2)
	off := t.Add(500 * time.Millisecond).Sub(local)
	c.offsetMu.Lock()
	if !c.hasOff {
		c.offset = off
	} else {
		c.offset = (c.offset*3 + off) / 4
	}
	c.hasOff = true
	c.offsetMu.Unlock()
}

func NewClient(p *Profile, mocks *MockRegistry) *Client {
	c := &Client{base: ozonBase, clientID: p.ClientID, apiKey: p.APIKey, http: &http.Client{Timeout: 40 * time.Second}}
	if p.Demo {
		c.base = "http://demo.ozon.local"
		c.clientID, c.apiKey = "demo", "demo"
		c.http = &http.Client{Transport: mocks.Get(p.ID), Timeout: 10 * time.Second}
	}
	return c
}

func (c *Client) call(ctx context.Context, path string, req any, out any) error {
	if c.readOnly && mutatingPaths[path] {
		return ErrReadOnly
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * 2 * time.Second):
			}
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		r.Header.Set("Client-Id", c.clientID)
		r.Header.Set("Api-Key", c.apiKey)
		r.Header.Set("Content-Type", "application/json")
		sent := time.Now()
		resp, err := c.http.Do(r)
		if err == nil {
			c.noteDate(resp.Header.Get("Date"), sent, time.Now())
		}
		if err != nil {
			lastErr = fmt.Errorf("сеть: %w", err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = parseOzonError(resp.StatusCode, path, data)
			continue
		}
		if resp.StatusCode != 200 {
			return parseOzonError(resp.StatusCode, path, data)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("Ozon %s: непонятный ответ: %v", path, err)
		}
		return nil
	}
	return lastErr
}

func parseOzonError(status int, path string, data []byte) error {
	e := &OzonError{Status: status, Path: path}
	var m map[string]any
	if json.Unmarshal(data, &m) == nil {
		e.Code = fmt.Sprint(firstNonNil(m["code"], ""))
		e.Message = fmt.Sprint(firstNonNil(m["message"], ""))
		if e.Code == "0" {
			e.Code = ""
		}
	} else {
		s := strings.TrimSpace(string(data))
		if len(s) > 300 {
			s = s[:300]
		}
		e.Message = s
	}
	return e
}

func firstNonNil(v any, def any) any {
	if v == nil {
		return def
	}
	return v
}

// ---------- время в ответах Ozon ----------

// OTime терпимо разбирает пустые строки, null и разные форматы дат.
type OTime struct{ time.Time }

func (t *OTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	for _, f := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if v, err := time.Parse(f, s); err == nil {
			if v.Year() < 2000 {
				return nil
			}
			t.Time = v
			return nil
		}
	}
	return nil
}

func (t OTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Time.UTC().Format(time.RFC3339))
}

// ---------- отправления ----------

type PostingProduct struct {
	SKU      int64  `json:"sku"`
	Name     string `json:"name"`
	OfferID  string `json:"offer_id"`
	Quantity int    `json:"quantity"`
}

type Posting struct {
	PostingNumber  string `json:"posting_number"`
	OrderNumber    string `json:"order_number"`
	Status         string `json:"status"`
	Substatus      string `json:"substatus"`
	InProcessAt    OTime  `json:"in_process_at"`
	ShipmentDate   OTime  `json:"shipment_date"`
	DeliveryMethod struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		WarehouseID int64  `json:"warehouse_id"`
		Warehouse   string `json:"warehouse"`
	} `json:"delivery_method"`
	Products      []PostingProduct `json:"products"`
	AnalyticsData *AnalyticsData   `json:"analytics_data"`
}

type AnalyticsData struct {
	DeliveryDateBegin       OTime  `json:"delivery_date_begin"`
	DeliveryDateEnd         OTime  `json:"delivery_date_end"`
	ClientDeliveryDateBegin OTime  `json:"client_delivery_date_begin"`
	ClientDeliveryDateEnd   OTime  `json:"client_delivery_date_end"`
	WarehouseID             int64  `json:"warehouse_id"`
	City                    string `json:"city"`
}

func (p *Posting) Window() (begin, end time.Time) {
	if p.AnalyticsData == nil {
		return
	}
	a := p.AnalyticsData
	if a.DeliveryDateEnd.IsZero() && !a.ClientDeliveryDateEnd.IsZero() {
		return a.ClientDeliveryDateBegin.Time, a.ClientDeliveryDateEnd.Time
	}
	return a.DeliveryDateBegin.Time, a.DeliveryDateEnd.Time
}

// ListPostings — отправления склада в указанном статусе, созданные за период (/v4/posting/fbs/list).
func (c *Client) ListPostings(ctx context.Context, warehouseID int64, status string, since, to time.Time) ([]Posting, error) {
	var all []Posting
	cursor := ""
	for {
		req := map[string]any{
			"filter": map[string]any{
				"since":         since.UTC().Format(time.RFC3339),
				"to":            to.UTC().Format(time.RFC3339),
				"statuses":      []string{status},
				"warehouse_ids": []string{strconv.FormatInt(warehouseID, 10)},
			},
			"limit":    100,
			"sort_dir": "ASC",
			"with":     map[string]any{"analytics_data": true},
		}
		if cursor != "" {
			req["cursor"] = cursor
		}
		type page struct {
			Postings []Posting `json:"postings"`
			HasNext  bool      `json:"has_next"`
			Cursor   string    `json:"cursor"`
		}
		var raw struct {
			page
			Result *page `json:"result"` // на случай ответа в обёртке result
		}
		if err := c.call(ctx, "/v4/posting/fbs/list", req, &raw); err != nil {
			return nil, err
		}
		resp := raw.page
		if len(resp.Postings) == 0 && raw.Result != nil {
			resp = *raw.Result
		}
		all = append(all, resp.Postings...)
		if !resp.HasNext || len(resp.Postings) == 0 || resp.Cursor == "" || resp.Cursor == cursor {
			return all, nil
		}
		cursor = resp.Cursor
		if len(all) >= 20000 {
			return all, fmt.Errorf("в статусе %s больше %d отправлений — список неполный", status, len(all))
		}
	}
}

// GetPosting — одно отправление по номеру.
func (c *Client) GetPosting(ctx context.Context, pn string) (*Posting, error) {
	req := map[string]any{"posting_number": pn, "with": map[string]any{"analytics_data": true}}
	var resp struct {
		Result Posting `json:"result"`
	}
	if err := c.call(ctx, "/v3/posting/fbs/get", req, &resp); err != nil {
		return nil, err
	}
	if resp.Result.PostingNumber == "" {
		return nil, fmt.Errorf("отправление %s не найдено", pn)
	}
	return &resp.Result, nil
}

// Ship — «Собрать»: одна коробка со всеми товарами отправления. Статус → awaiting_deliver.
func (c *Client) Ship(ctx context.Context, p *Posting) error {
	var items []map[string]any
	for _, pr := range p.Products {
		items = append(items, map[string]any{"product_id": pr.SKU, "quantity": pr.Quantity})
	}
	req := map[string]any{
		"packages":       []any{map[string]any{"products": items}},
		"posting_number": p.PostingNumber,
		"with":           map[string]any{"additional_data": false},
	}
	var resp struct {
		Result []string `json:"result"`
	}
	return c.call(ctx, "/v4/posting/fbs/ship", req, &resp)
}

// CancelReason — причина отмены отправления продавцом.
type CancelReason struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
}

func (c *Client) CancelReasons(ctx context.Context) ([]CancelReason, error) {
	var resp struct {
		Result []CancelReason `json:"result"`
	}
	if err := c.call(ctx, "/v2/posting/fbs/cancel-reason/list", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	return resp.Result, nil
}

// Cancel отменяет отправление (возможно в статусах «Ожидает сборки» и «Ожидает отгрузки»).
func (c *Client) Cancel(ctx context.Context, postingNumber string, reasonID int64, message string) error {
	var resp struct {
		Result bool `json:"result"`
	}
	req := map[string]any{"posting_number": postingNumber, "cancel_reason_id": reasonID, "cancel_reason_message": message}
	if err := c.call(ctx, "/v2/posting/fbs/cancel", req, &resp); err != nil {
		return err
	}
	if !resp.Result {
		return &OzonError{Status: 200, Path: "/v2/posting/fbs/cancel", Message: "Ozon не подтвердил отмену"}
	}
	return nil
}

// SetStatus — «Доставляется» / «Курьер в пути» / «Доставлено» для одного отправления.
func (c *Client) SetStatus(ctx context.Context, path, postingNumber string) error {
	res, err := c.SetStatusBatch(ctx, path, []string{postingNumber})
	if err != nil {
		return err
	}
	return res[postingNumber]
}

// SetStatusBatch — смена статуса сразу нескольким отправлениям.
// Возвращает ошибку по каждому номеру (nil — успешно) или общую ошибку запроса.
func (c *Client) SetStatusBatch(ctx context.Context, path string, pns []string) (map[string]error, error) {
	req := map[string]any{"posting_number": pns}
	var resp struct {
		Result []struct {
			PostingNumber string          `json:"posting_number"`
			Result        bool            `json:"result"`
			Error         json.RawMessage `json:"error"`
		} `json:"result"`
	}
	if err := c.call(ctx, path, req, &resp); err != nil {
		return nil, err
	}
	out := map[string]error{}
	for _, pn := range pns {
		out[pn] = &OzonError{Status: 200, Path: path, Message: "Ozon не вернул результат по отправлению"}
	}
	for _, r := range resp.Result {
		pn := r.PostingNumber
		if pn == "" && len(pns) == 1 {
			pn = pns[0]
		}
		if _, ok := out[pn]; !ok {
			continue
		}
		if r.Result {
			out[pn] = nil
			continue
		}
		msg := strings.Trim(string(r.Error), `[]"`)
		if msg == "" || msg == "null" {
			msg = "Ozon не подтвердил смену статуса"
		}
		out[pn] = &OzonError{Status: 200, Path: path, Message: msg}
	}
	return out, nil
}

// ---------- склады ----------

type Warehouse struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	IsRFBS bool   `json:"is_rfbs"`
	Status string `json:"status"`
}

func (c *Client) Warehouses(ctx context.Context) ([]Warehouse, error) {
	var raw any
	err := c.call(ctx, "/v2/warehouse/list", map[string]any{"limit": 200}, &raw)
	if err != nil {
		var raw2 any
		if err2 := c.call(ctx, "/v1/warehouse/list", map[string]any{}, &raw2); err2 != nil {
			return nil, err
		}
		raw = raw2
	}
	seen := map[int64]bool{}
	var out []Warehouse
	walkObjects(raw, func(m map[string]any) {
		id := toInt(m["warehouse_id"])
		name, _ := m["name"].(string)
		if id == 0 || name == "" || seen[id] {
			return
		}
		seen[id] = true
		w := Warehouse{ID: id, Name: name}
		w.IsRFBS, _ = m["is_rfbs"].(bool)
		w.Status, _ = m["status"].(string)
		out = append(out, w)
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ---------- товары и остатки ----------

type ProductInfo struct {
	SKU       int64  `json:"sku"`
	ProductID int64  `json:"product_id"`
	OfferID   string `json:"offer_id"`
	Name      string `json:"name"`
	Present   *int64 `json:"present,omitempty"`
	Reserved  *int64 `json:"reserved,omitempty"`
}

func (c *Client) ProductsBySKU(ctx context.Context, skus []int64) (map[int64]*ProductInfo, error) {
	var raw map[string]any
	if err := c.call(ctx, "/v3/product/info/list", map[string]any{"sku": skus}, &raw); err != nil {
		return nil, err
	}
	want := map[int64]bool{}
	for _, s := range skus {
		want[s] = true
	}
	out := map[int64]*ProductInfo{}
	items, _ := raw["items"].([]any)
	if items == nil {
		if r, ok := raw["result"].(map[string]any); ok {
			items, _ = r["items"].([]any)
		}
	}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		var found []int64
		for _, k := range []string{"sku", "fbs_sku", "fbo_sku"} {
			if v := toInt(m[k]); v != 0 {
				found = append(found, v)
			}
		}
		if src, ok := m["sources"].([]any); ok {
			for _, s := range src {
				if sm, ok := s.(map[string]any); ok {
					if v := toInt(sm["sku"]); v != 0 {
						found = append(found, v)
					}
				}
			}
		}
		for _, sku := range found {
			if !want[sku] || out[sku] != nil {
				continue
			}
			pi := &ProductInfo{SKU: sku, ProductID: toInt(m["id"])}
			if pi.ProductID == 0 {
				pi.ProductID = toInt(m["product_id"])
			}
			pi.OfferID, _ = m["offer_id"].(string)
			pi.Name, _ = m["name"].(string)
			out[sku] = pi
		}
	}
	return out, nil
}

type StockRow struct {
	SKU         int64
	WarehouseID int64
	Present     int64
	Reserved    int64
}

// StocksFBS — остатки товаров на складах продавца.
func (c *Client) StocksFBS(ctx context.Context, skus []int64) ([]StockRow, error) {
	strs := make([]string, len(skus))
	for i, s := range skus {
		strs[i] = strconv.FormatInt(s, 10)
	}
	var raw any
	err := c.call(ctx, "/v2/product/info/stocks-by-warehouse/fbs", map[string]any{"sku": skus, "limit": 1000}, &raw)
	if err != nil {
		var raw2 any
		if err2 := c.call(ctx, "/v1/product/info/stocks-by-warehouse/fbs", map[string]any{"sku": strs}, &raw2); err2 != nil {
			return nil, err
		}
		raw = raw2
	}
	var out []StockRow
	walkObjects(raw, func(m map[string]any) {
		if _, ok := m["present"]; !ok {
			return
		}
		out = append(out, StockRow{SKU: toInt(m["sku"]), WarehouseID: toInt(m["warehouse_id"]), Present: toInt(m["present"]), Reserved: toInt(m["reserved"])})
	})
	return out, nil
}

// SetStock — выставить остаток товара на складе.
func (c *Client) SetStock(ctx context.Context, warehouseID int64, p *ProductInfo, qty int) error {
	req := map[string]any{"stocks": []any{map[string]any{
		"offer_id":     p.OfferID,
		"product_id":   p.ProductID,
		"stock":        qty,
		"warehouse_id": warehouseID,
	}}}
	var resp struct {
		Result []struct {
			Updated bool `json:"updated"`
			Errors  []struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"result"`
	}
	if err := c.call(ctx, "/v2/products/stocks", req, &resp); err != nil {
		return err
	}
	for _, r := range resp.Result {
		if !r.Updated {
			var msgs []string
			for _, e := range r.Errors {
				msgs = append(msgs, strings.TrimSpace(e.Code+" "+e.Message))
			}
			if len(msgs) == 0 {
				msgs = append(msgs, "Ozon не обновил остаток")
			}
			return &OzonError{Status: 200, Path: "/v2/products/stocks", Message: strings.Join(msgs, "; ")}
		}
	}
	return nil
}

// Roles — методы, разрешённые ключу.
func (c *Client) Roles(ctx context.Context) (names []string, methods map[string]bool, err error) {
	var raw any
	if err = c.call(ctx, "/v1/roles", map[string]any{}, &raw); err != nil {
		return
	}
	methods = map[string]bool{}
	walkObjects(raw, func(m map[string]any) {
		if n, ok := m["name"].(string); ok && n != "" {
			if _, has := m["methods"]; has {
				names = append(names, n)
			}
		}
		if ms, ok := m["methods"].([]any); ok {
			for _, x := range ms {
				if s, ok := x.(string); ok {
					methods[s] = true
				}
			}
		}
	})
	return
}

// ---------- утилиты разбора ----------

func walkObjects(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, x := range t {
			walkObjects(x, fn)
		}
	case []any:
		for _, x := range t {
			walkObjects(x, fn)
		}
	}
}

func toInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}
