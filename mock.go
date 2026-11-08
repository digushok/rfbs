package main

// Демо-Озон внутри программы: генерирует заказы и проверяет переходы статусов
// так же строго, как настоящий. Ничего не уходит в интернет.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"
)

const (
	mockWarehouseID   int64 = 1020000123456000
	mockWarehouseName       = "Демо-склад rFBS (Москва)"
	mockSKU1          int64 = 5980291763
	mockSKU2          int64 = 5980291764
	mockSKUOther      int64 = 1234567890
)

type mockProduct struct {
	SKU       int64
	ProductID int64
	OfferID   string
	Name      string
}

type MockOzon struct {
	mu        sync.Mutex
	now       func() time.Time
	rnd       *rand.Rand
	products  []mockProduct
	stock     map[int64]int // по SKU на демо-складе
	postings  []*Posting
	nextOrder time.Time
	seq       int
	// Окно доставки демо-заказа относительно момента заказа.
	WindowStart, WindowEnd time.Duration
	OrderEvery             time.Duration
}

func NewMockOzon(now func() time.Time) *MockOzon {
	m := &MockOzon{now: now, rnd: rand.New(rand.NewSource(now().UnixNano())), stock: map[int64]int{},
		WindowStart: 4 * time.Minute, WindowEnd: 10 * time.Minute, OrderEvery: 2 * time.Minute}
	m.products = []mockProduct{
		{mockSKU1, 811001, "CASE-BLACK-01", "Чехол для телефона, чёрный"},
		{mockSKU2, 811002, "GLASS-01", "Защитное стекло"},
		{mockSKUOther, 811003, "CABLE-USB-C", "Кабель USB-C, 1 м"},
	}
	m.stock[mockSKU1], m.stock[mockSKU2], m.stock[mockSKUOther] = 5, 5, 50
	m.nextOrder = now().Add(20 * time.Second)
	return m
}

func (m *MockOzon) prod(sku int64) mockProduct {
	for _, p := range m.products {
		if p.SKU == sku {
			return p
		}
	}
	return mockProduct{}
}

// AddPosting создаёт заказ. target — заказ двух целевых SKU по 1 шт.
func (m *MockOzon) AddPosting(kind string) *Posting {
	now := m.now()
	m.seq++
	p := &Posting{
		PostingNumber: fmt.Sprintf("%08d-%04d-1", 70000000+m.seq*37, m.seq),
		OrderNumber:   fmt.Sprintf("%08d-%04d", 70000000+m.seq*37, m.seq),
		Status:        "awaiting_packaging", Substatus: "posting_created",
	}
	p.InProcessAt.Time = now
	p.ShipmentDate.Time = now.Add(2 * time.Hour)
	p.DeliveryMethod.ID = 77
	p.DeliveryMethod.Name = "Доставка курьером продавца"
	p.DeliveryMethod.WarehouseID = mockWarehouseID
	p.DeliveryMethod.Warehouse = mockWarehouseName
	add := func(sku int64, q int) {
		pr := m.prod(sku)
		p.Products = append(p.Products, PostingProduct{SKU: sku, Name: pr.Name, OfferID: pr.OfferID, Quantity: q})
	}
	switch kind {
	case "target":
		add(mockSKU1, 1)
		add(mockSKU2, 1)
	case "single":
		add(mockSKU1, 1)
	case "double":
		add(mockSKU1, 2)
		add(mockSKU2, 1)
	case "other_wh":
		add(mockSKU1, 1)
		add(mockSKU2, 1)
		p.DeliveryMethod.WarehouseID = mockWarehouseID + 1
	default:
		add(mockSKU1, 1)
		add(mockSKUOther, 1)
	}
	for _, pr := range p.Products {
		m.stock[pr.SKU] -= pr.Quantity
	}
	p.AnalyticsData = &struct {
		DeliveryDateBegin OTime  `json:"delivery_date_begin"`
		DeliveryDateEnd   OTime  `json:"delivery_date_end"`
		WarehouseID       int64  `json:"warehouse_id"`
		City              string `json:"city"`
	}{WarehouseID: p.DeliveryMethod.WarehouseID, City: "Москва"}
	p.AnalyticsData.DeliveryDateBegin.Time = now.Add(m.WindowStart).Truncate(time.Minute)
	p.AnalyticsData.DeliveryDateEnd.Time = now.Add(m.WindowEnd).Truncate(time.Minute)
	m.postings = append(m.postings, p)
	return p
}

func (m *MockOzon) generate() {
	if m.OrderEvery <= 0 {
		return
	}
	for !m.now().Before(m.nextOrder) {
		kinds := []string{"target", "target", "target", "single", "double", "other", "other_wh"}
		k := kinds[m.rnd.Intn(len(kinds))]
		if k == "target" && (m.stock[mockSKU1] <= 0 || m.stock[mockSKU2] <= 0) {
			k = "other"
		}
		m.AddPosting(k)
		m.nextOrder = m.nextOrder.Add(m.OrderEvery + time.Duration(m.rnd.Intn(30))*time.Second)
	}
}

func (m *MockOzon) find(pn string) *Posting {
	for _, p := range m.postings {
		if p.PostingNumber == pn {
			return p
		}
	}
	return nil
}

func (m *MockOzon) Handle(path string, body []byte) (int, any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generate()
	var req map[string]any
	json.Unmarshal(body, &req)
	errResp := func(code int, msg string) (int, any) {
		return code, map[string]any{"code": code, "message": msg}
	}
	switch path {
	case "/v1/roles":
		return 200, map[string]any{"roles": []any{
			map[string]any{"name": "Posting FBS", "methods": []string{"/v3/posting/fbs/list", "/v4/posting/fbs/ship", "/v2/fbs/posting/delivering", "/v2/fbs/posting/last-mile", "/v2/fbs/posting/delivered"}},
			map[string]any{"name": "Stocks", "methods": []string{"/v2/products/stocks", "/v1/product/info/stocks-by-warehouse/fbs", "/v1/warehouse/list"}},
			map[string]any{"name": "Product read-only", "methods": []string{"/v3/product/info/list"}},
		}}
	case "/v1/warehouse/list":
		return 200, map[string]any{"result": []any{
			map[string]any{"warehouse_id": mockWarehouseID, "name": mockWarehouseName, "is_rfbs": true, "status": "created"},
			map[string]any{"warehouse_id": mockWarehouseID + 1, "name": "Демо-склад FBS (Подольск)", "is_rfbs": false, "status": "created"},
		}}
	case "/v3/product/info/list":
		var items []any
		for _, s := range toIntList(req["sku"]) {
			pr := m.prod(s)
			if pr.SKU == 0 {
				continue
			}
			items = append(items, map[string]any{"id": pr.ProductID, "offer_id": pr.OfferID, "name": pr.Name,
				"sources": []any{map[string]any{"sku": pr.SKU, "source": "sds"}}})
		}
		return 200, map[string]any{"items": items}
	case "/v1/product/info/stocks-by-warehouse/fbs":
		var res []any
		for _, s := range toIntList(req["sku"]) {
			pr := m.prod(s)
			if pr.SKU == 0 {
				continue
			}
			reserved := 0
			for _, p := range m.postings {
				if p.Status == "awaiting_packaging" || p.Status == "awaiting_deliver" {
					for _, x := range p.Products {
						if x.SKU == s {
							reserved += x.Quantity
						}
					}
				}
			}
			res = append(res, map[string]any{"sku": s, "product_id": pr.ProductID, "warehouse_id": mockWarehouseID,
				"warehouse_name": mockWarehouseName, "present": m.stock[s] + reserved, "reserved": reserved})
		}
		return 200, map[string]any{"result": res}
	case "/v2/products/stocks":
		var res []any
		stocks, _ := req["stocks"].([]any)
		for _, x := range stocks {
			s, _ := x.(map[string]any)
			pid := toInt(s["product_id"])
			var pr mockProduct
			for _, p := range m.products {
				if p.ProductID == pid || (p.OfferID == s["offer_id"] && pid == 0) {
					pr = p
				}
			}
			if pr.SKU == 0 || toInt(s["warehouse_id"]) != mockWarehouseID {
				res = append(res, map[string]any{"product_id": pid, "updated": false, "errors": []any{map[string]any{"code": "NOT_FOUND", "message": "товар или склад не найден"}}})
				continue
			}
			m.stock[pr.SKU] = int(toInt(s["stock"]))
			res = append(res, map[string]any{"product_id": pid, "offer_id": pr.OfferID, "warehouse_id": mockWarehouseID, "updated": true, "errors": []any{}})
		}
		return 200, map[string]any{"result": res}
	case "/v3/posting/fbs/list":
		f, _ := req["filter"].(map[string]any)
		whs := toIntList(f["warehouse_id"])
		var out []*Posting
		for _, p := range m.postings {
			if len(whs) > 0 && whs[0] != p.DeliveryMethod.WarehouseID {
				continue
			}
			out = append(out, p)
		}
		return 200, map[string]any{"result": map[string]any{"postings": out, "has_next": false}}
	case "/v4/posting/fbs/ship":
		pn, _ := req["posting_number"].(string)
		p := m.find(pn)
		if p == nil {
			return errResp(404, "POSTING_NOT_FOUND")
		}
		if p.Status != "awaiting_packaging" {
			return errResp(400, "POSTING_ALREADY_SHIPPED: отправление в статусе "+p.Status)
		}
		p.Status, p.Substatus = "awaiting_deliver", "posting_awaiting_passport_data"
		return 200, map[string]any{"result": []string{pn}}
	case "/v2/fbs/posting/delivering", "/v2/fbs/posting/last-mile", "/v2/fbs/posting/delivered":
		var res []any
		for _, pn := range toStrList(req["posting_number"]) {
			p := m.find(pn)
			ok, errText := false, ""
			switch {
			case p == nil:
				errText = "POSTING_NOT_FOUND"
			case path == "/v2/fbs/posting/delivering" && p.Status == "awaiting_deliver":
				p.Status, p.Substatus, ok = "delivering", "posting_transferring_to_delivery", true
			case path == "/v2/fbs/posting/last-mile" && p.Status == "delivering":
				p.Status, p.Substatus, ok = "driver_pickup", "posting_driver_pick_up", true
			case path == "/v2/fbs/posting/delivered" && (p.Status == "driver_pickup" || p.Status == "delivering"):
				p.Status, p.Substatus, ok = "delivered", "posting_conditionally_delivered", true
			default:
				errText = "INVALID_POSTING_STATUS: " + p.Status
			}
			r := map[string]any{"posting_number": pn, "result": ok}
			if !ok {
				r["error"] = []string{errText}
			}
			res = append(res, r)
		}
		return 200, map[string]any{"result": res}
	}
	return errResp(404, "демо: метод "+path+" не реализован")
}

func toIntList(v any) []int64 {
	arr, _ := v.([]any)
	var out []int64
	for _, x := range arr {
		out = append(out, toInt(x))
	}
	return out
}

func toStrList(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// RoundTrip позволяет использовать демо-Озон как http-транспорт.
func (m *MockOzon) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		r.Body.Close()
	}
	code, out := m.Handle(r.URL.Path, body)
	b, _ := json.Marshal(out)
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(b)), Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
}

type MockRegistry struct {
	mu sync.Mutex
	m  map[string]*MockOzon
}

func NewMockRegistry() *MockRegistry { return &MockRegistry{m: map[string]*MockOzon{}} }

func (r *MockRegistry) Get(id string) *MockOzon {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[id] == nil {
		r.m[id] = NewMockOzon(time.Now)
	}
	return r.m[id]
}
