package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
)

func client(url string, mod func(*config.Upstream)) *Client {
	u := config.Upstream{BaseURL: url, Timeout: 2 * time.Second, MaxConcurrency: 4, MaxResponseBytes: 1 << 20}
	if mod != nil {
		mod(&u)
	}
	return New(u)
}

func TestRequestShape(t *testing.T) {
	type seen struct {
		r    *http.Request
		body string
	}
	ch := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- seen{r, string(b)}
	}))
	defer srv.Close()

	c := client(srv.URL, func(u *config.Upstream) {
		u.Headers = map[string]string{"User-Agent": "gerbang-test"}
		u.Token = "secret"
	})
	tool := &catalog.Tool{Method: "POST", Path: "/{start_date}..{end_date}", Body: true, Params: []catalog.Param{
		{Name: "start_date", In: "path"},
		{Name: "end_date", In: "path"},
		{Name: "symbols", In: "query"},
		{Name: "status", In: "query", Explode: true},
		{Name: "X-Trace", In: "header"},
	}}
	args := map[string]any{
		"start_date": "2026-10-01", "end_date": "2026-10-07",
		"symbols": []any{"IDR", "EUR"}, "status": []any{"a b", "c"}, "X-Trace": "t1",
		"body": map[string]any{"id": json.Number("9007199254740993")}, // 2^53+1, a float64 would round it
	}
	if _, err := c.Do(context.Background(), tool, args, &config.Principal{Name: "ops-bot", Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	got := <-ch

	if got.r.URL.Path != "/2026-10-01..2026-10-07" || got.r.URL.RawQuery != "symbols=IDR,EUR&status=a+b&status=c" {
		t.Errorf("url = %s", got.r.URL)
	}
	for k, want := range map[string]string{
		"User-Agent":          "gerbang-test",
		"Authorization":       "Bearer secret",
		"X-Trace":             "t1",
		"Content-Type":        "application/json",
		"X-Gerbang-Principal": "", // forward_identity is off
	} {
		if v := got.r.Header.Get(k); v != want {
			t.Errorf("header %s = %q, want %q", k, v, want)
		}
	}
	if got.body != `{"id":9007199254740993}` {
		t.Errorf("body = %s", got.body)
	}
}

func TestPathTraversalRejected(t *testing.T) {
	tool := &catalog.Tool{Method: "GET", Path: "/repos/{owner}/x", Params: []catalog.Param{{Name: "owner", In: "path"}}}
	if _, err := client("http://unused.invalid", nil).Do(context.Background(), tool, map[string]any{"owner": ".."}, nil); err == nil {
		t.Error(`owner ".." was sent`)
	}
}

func TestRetriesOnlyReads(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	c := client(srv.URL, func(u *config.Upstream) { u.Retries = 1 })

	res, err := c.Do(context.Background(), &catalog.Tool{Method: "GET", Path: "/"}, nil, nil)
	if err != nil || res.Status != 200 || hits.Load() != 2 {
		t.Errorf("GET: status %v, err %v, hits %d; want 200 after one retry", res, err, hits.Load())
	}
	hits.Store(0)
	res, err = c.Do(context.Background(), &catalog.Tool{Method: "POST", Path: "/"}, nil, nil)
	if err != nil || res.Status != 503 || hits.Load() != 1 {
		t.Errorf("POST: status %v, err %v, hits %d; want 503 and no retry", res, err, hits.Load())
	}
}

func TestTruncateAndRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", 20))
	}))
	defer srv.Close()
	c := client(srv.URL, func(u *config.Upstream) {
		u.MaxResponseBytes = 10
		u.RateLimit = config.RateLimit{Requests: 1, Per: time.Hour}
	})
	get := &catalog.Tool{Method: "GET", Path: "/"}

	res, err := c.Do(context.Background(), get, nil, nil)
	if err != nil || !res.Truncated || len(res.Body) != 10 {
		t.Errorf("got %+v, %v; want 10 bytes, truncated", res, err)
	}
	if _, err := c.Do(context.Background(), get, nil, nil); !errors.Is(err, ErrRateLimited) {
		t.Errorf("second call err = %v, want ErrRateLimited", err)
	}
}
