// Command users is a fake existing service, described by openapi.yml.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type User struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Phone     *string   `json:"phone"`
	Tier      string    `json:"tier"`
	CreatedAt time.Time `json:"created_at"`
}

type Address struct {
	Label      string `json:"label"`
	Line1      string `json:"line1"`
	City       string `json:"city"`
	PostalCode string `json:"postal_code"`
	Country    string `json:"country"`
}

// Read-only, so no locking.
var users = []User{
	{"usr_1", "Rina Wijaya", "rina.wijaya@example.com", new("+62-812-0000-0001"), "gold", time.Date(2024, 3, 2, 8, 0, 0, 0, time.UTC)},
	{"usr_2", "Budi Santoso", "budi.santoso@example.com", new("+62-812-0000-0002"), "standard", time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)},
	{"usr_3", "Maya Lestari", "maya.lestari@example.com", nil, "standard", time.Date(2026, 6, 30, 8, 0, 0, 0, time.UTC)},
}

var addresses = map[string][]Address{
	"usr_1": {
		{"home", "Jl. Melati No. 12", "Bandung", "40115", "ID"},
		{"office", "Jl. Asia Afrika No. 8", "Bandung", "40111", "ID"},
	},
	"usr_2": {{"home", "Jl. Kenanga No. 3", "Yogyakarta", "55281", "ID"}},
}

func findUser(id string) (User, bool) {
	for _, u := range users {
		if u.ID == id {
			return u, true
		}
	}
	return User{}, false
}

func searchUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	if len(q) < 2 {
		writeError(w, http.StatusBadRequest, "q must be at least 2 characters")
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 50 {
			writeError(w, http.StatusBadRequest, "limit must be an integer between 1 and 50")
			return
		}
		limit = n
	}

	matches := []User{}
	for _, u := range users {
		if strings.Contains(strings.ToLower(u.Name), q) || strings.Contains(strings.ToLower(u.Email), q) {
			matches = append(matches, u)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": matches[:min(limit, len(matches))]})
}

func getUser(w http.ResponseWriter, r *http.Request) {
	u, ok := findUser(r.PathValue("userId"))
	if !ok {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func listAddresses(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("userId")
	if _, ok := findUser(id); !ok {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": append([]Address{}, addresses[id]...)})
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
	addr := flag.String("addr", "127.0.0.1:9002", "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /users", searchUsers)
	mux.HandleFunc("GET /users/{userId}", getUser)
	mux.HandleFunc("GET /users/{userId}/addresses", listAddresses)

	srv := &http.Server{Addr: *addr, Handler: logRequests(mux), ReadHeaderTimeout: 5 * time.Second}
	slog.Info("users service listening", "addr", *addr)
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
