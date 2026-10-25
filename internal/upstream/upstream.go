// Package upstream turns tool arguments into HTTP calls to the real API.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
)

var ErrRateLimited = errors.New("rate limit reached, try again later")

// Client calls one service. Create one per service and share it.
type Client struct {
	cfg     config.Upstream
	http    *http.Client
	slots   chan struct{} // in-flight call limit
	limiter *rate.Limiter // nil means no limit
}

type Result struct {
	Status    int
	Body      []byte
	Truncated bool
}

func New(u config.Upstream) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = u.MaxConcurrency // the default of 2 churns connections under load
	c := &Client{cfg: u, http: &http.Client{Transport: tr}, slots: make(chan struct{}, u.MaxConcurrency)}
	if rl := u.RateLimit; rl.Requests > 0 {
		c.limiter = rate.NewLimiter(rate.Every(rl.Per/time.Duration(rl.Requests)), rl.Requests)
	}
	return c
}

// Do calls the tool's endpoint and reads at most MaxResponseBytes of the reply.
func (c *Client) Do(ctx context.Context, t *catalog.Tool, args map[string]any, who *config.Principal) (*Result, error) {
	if c.limiter != nil && !c.limiter.Allow() { // fail fast rather than queue
		return nil, ErrRateLimited
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout) // covers every attempt
	defer cancel()
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("service busy: %w", ctx.Err())
	}

	target, body, err := c.target(t, args)
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, t.Method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.setHeaders(req, t, args, body != nil, who)
		res, err := c.http.Do(req)
		if attempt < c.cfg.Retries && retryable(ctx, t.Method, res, err) {
			if res != nil {
				res.Body.Close()
			}
			select {
			case <-time.After(100*time.Millisecond<<attempt + rand.N(50*time.Millisecond)):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		data, err := io.ReadAll(io.LimitReader(res.Body, c.cfg.MaxResponseBytes+1))
		if err != nil {
			return nil, err
		}
		r := &Result{Status: res.StatusCode, Body: data}
		if int64(len(data)) > c.cfg.MaxResponseBytes {
			r.Body, r.Truncated = data[:c.cfg.MaxResponseBytes], true
		}
		return r, nil
	}
}

// retryable: only reads, only network errors or 502/503/504, only if time is left.
func retryable(ctx context.Context, method string, res *http.Response, err error) bool {
	if method != http.MethodGet && method != http.MethodHead || ctx.Err() != nil {
		return false
	}
	if err != nil {
		return true
	}
	return res.StatusCode == http.StatusBadGateway || res.StatusCode == http.StatusServiceUnavailable ||
		res.StatusCode == http.StatusGatewayTimeout
}

// target builds the URL and the JSON body from the arguments.
func (c *Client) target(t *catalog.Tool, args map[string]any) (string, []byte, error) {
	p := t.Path
	var query []string
	for _, prm := range t.Params {
		v, ok := args[prm.Name]
		if !ok {
			continue
		}
		vals := values(v)
		switch prm.In {
		case "path":
			s := strings.Join(vals, ",")
			if s == "." || s == ".." { // would walk up the URL path
				return "", nil, fmt.Errorf("%s: %q is not allowed", prm.Name, s)
			}
			p = strings.ReplaceAll(p, "{"+prm.Name+"}", url.PathEscape(s))
		case "query":
			for i := range vals {
				vals[i] = url.QueryEscape(vals[i])
			}
			k := url.QueryEscape(prm.Name)
			if prm.Explode {
				for _, s := range vals {
					query = append(query, k+"="+s)
				}
			} else {
				query = append(query, k+"="+strings.Join(vals, ",")) // literal comma, per the spec
			}
		}
	}
	target := c.cfg.BaseURL + p
	if len(query) > 0 {
		target += "?" + strings.Join(query, "&")
	}

	var body []byte
	if b, ok := args["body"]; ok && t.Body {
		var err error
		if body, err = json.Marshal(b); err != nil {
			return "", nil, err
		}
	}
	return target, body, nil
}

// setHeaders applies static headers, then header params, then the gateway's own.
func (c *Client) setHeaders(req *http.Request, t *catalog.Tool, args map[string]any, hasBody bool, who *config.Principal) {
	for k, v := range c.cfg.Headers {
		req.Header.Set(k, v)
	}
	for _, prm := range t.Params {
		if v, ok := args[prm.Name]; ok && prm.In == "header" {
			req.Header.Set(prm.Name, strings.Join(values(v), ","))
		}
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	if c.cfg.ForwardIdentity && who != nil {
		req.Header.Set("X-Gerbang-Principal", who.Name)
		req.Header.Set("X-Gerbang-Actor", who.Actor)
	}
}

// values renders an argument as strings, one per array element.
func values(v any) []string {
	list, ok := v.([]any)
	if !ok {
		list = []any{v}
	}
	out := make([]string, len(list))
	for i, x := range list {
		switch x := x.(type) {
		case string:
			out[i] = x
		case json.Number:
			out[i] = x.String()
		default: // bools, objects
			b, _ := json.Marshal(x)
			out[i] = string(b)
		}
	}
	return out
}
