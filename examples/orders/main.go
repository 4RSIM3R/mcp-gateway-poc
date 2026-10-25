// Command orders is a fake existing service, described by openapi.yml.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

type LineItem struct {
	SKU            string `json:"sku"`
	Name           string `json:"name"`
	Quantity       int    `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type Order struct {
	ID           string     `json:"id"`
	CustomerID   string     `json:"customer_id"`
	Status       string     `json:"status"`
	Items        []LineItem `json:"items"`
	TotalCents   int64      `json:"total_cents"`
	Currency     string     `json:"currency"`
	CreatedAt    time.Time  `json:"created_at"`
	CancelReason *string    `json:"cancel_reason"`
}

type itemRequest struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type product struct {
	name       string
	priceCents int64
}

var products = map[string]product{
	"SKU-KOPI-250":  {"Arabica coffee beans 250g", 1250},
	"SKU-MUG-01":    {"Ceramic mug", 900},
	"SKU-FILTER-02": {"Pour-over paper filters (100)", 450},
}

var validStatuses = []string{"pending", "paid", "shipped", "delivered", "cancelled"}

type store struct {
	mu     sync.Mutex
	orders map[string]*Order
	nextID int
}

func newStore() *store {
	s := &store{orders: map[string]*Order{}, nextID: 1001}
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	seed := []struct {
		customer, status string
		items            []itemRequest
	}{
		{"usr_1", "delivered", []itemRequest{{"SKU-KOPI-250", 2}}},
		{"usr_2", "shipped", []itemRequest{{"SKU-MUG-01", 1}, {"SKU-FILTER-02", 2}}},
		{"usr_1", "paid", []itemRequest{{"SKU-FILTER-02", 1}}},
		{"usr_3", "pending", []itemRequest{{"SKU-KOPI-250", 1}, {"SKU-MUG-01", 2}}},
		{"usr_2", "cancelled", []itemRequest{{"SKU-MUG-01", 4}}},
	}
	for i, o := range seed {
		ord := s.add(o.customer, o.status, base.Add(time.Duration(i)*6*time.Hour), o.items)
		if o.status == "cancelled" {
			reason := "Customer ordered the wrong quantity"
			ord.CancelReason = &reason
		}
	}
	return s
}

// add creates an order from validated items; callers hold s.mu.
func (s *store) add(customerID, status string, createdAt time.Time, items []itemRequest) *Order {
	o := &Order{
		ID:         fmt.Sprintf("ord_%d", s.nextID),
		CustomerID: customerID,
		Status:     status,
		Currency:   "USD",
		CreatedAt:  createdAt,
	}
	s.nextID++
	for _, it := range items {
		p := products[it.SKU]
		o.Items = append(o.Items, LineItem{SKU: it.SKU, Name: p.name, Quantity: it.Quantity, UnitPriceCents: p.priceCents})
		o.TotalCents += p.priceCents * int64(it.Quantity)
	}
	s.orders[o.ID] = o
	return o
}

func (s *store) listOrders(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status, customerID := q.Get("status"), q.Get("customer_id")
	if status != "" && !slices.Contains(validStatuses, status) {
		writeError(w, http.StatusBadRequest, "unknown status "+strconv.Quote(status))
		return
	}
	limit := 20
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 100")
			return
		}
		limit = n
	}

	s.mu.Lock()
	orders := []Order{}
	for _, o := range s.orders {
		if (status == "" || o.Status == status) && (customerID == "" || o.CustomerID == customerID) {
			orders = append(orders, *o)
		}
	}
	s.mu.Unlock()

	slices.SortFunc(orders, func(a, b Order) int { return b.CreatedAt.Compare(a.CreatedAt) })
	orders = orders[:min(limit, len(orders))]
	writeJSON(w, http.StatusOK, map[string]any{"orders": orders, "count": len(orders)})
}

func (s *store) getOrder(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[r.PathValue("orderId")]
	if !ok {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *store) createOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CustomerID string        `json:"customer_id"`
		Items      []itemRequest `json:"items"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.CustomerID == "" || len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "customer_id and at least one item are required")
		return
	}
	for _, it := range req.Items {
		if _, ok := products[it.SKU]; !ok {
			writeError(w, http.StatusBadRequest, "unknown sku "+strconv.Quote(it.SKU))
			return
		}
		if it.Quantity < 1 {
			writeError(w, http.StatusBadRequest, "quantity must be at least 1")
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusCreated, s.add(req.CustomerID, "pending", time.Now().UTC(), req.Items))
}

func (s *store) cancelOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[r.PathValue("orderId")]
	if !ok {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	if o.Status != "pending" && o.Status != "paid" {
		writeError(w, http.StatusConflict, fmt.Sprintf("order is %s and can no longer be cancelled", o.Status))
		return
	}
	o.Status = "cancelled"
	o.CancelReason = &req.Reason
	writeJSON(w, http.StatusOK, o)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// logRequests logs each call with the caller gerbang acted for.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "method", r.Method, "path", r.URL.RequestURI(),
			"principal", r.Header.Get("X-Gerbang-Principal"), "actor", r.Header.Get("X-Gerbang-Actor"))
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9001", "listen address")
	flag.Parse()

	s := newStore()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders", s.listOrders)
	mux.HandleFunc("POST /orders", s.createOrder)
	mux.HandleFunc("GET /orders/{orderId}", s.getOrder)
	mux.HandleFunc("POST /orders/{orderId}/cancel", s.cancelOrder)

	srv := &http.Server{Addr: *addr, Handler: logRequests(mux), ReadHeaderTimeout: 5 * time.Second}
	slog.Info("orders service listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
