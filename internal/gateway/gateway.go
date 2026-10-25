// Package gateway serves the catalog's tools over MCP.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
	"ilzam.dev/gerbang/internal/policy"
	"ilzam.dev/gerbang/internal/upstream"
)

type gateway struct {
	cat     *catalog.Catalog
	policy  *policy.Policy
	log     *slog.Logger
	clients map[string]*upstream.Client // service name → client
	tools   map[string]*catalog.Tool    // MCP tool name → tool
	servers map[string]*mcp.Server      // "svc/<name>" or "bundle/<name>"
}

// New returns the handler for /{svc|bundle}/{name}/mcp, /omni/mcp and /healthz.
func New(c *catalog.Catalog, p *policy.Policy, log *slog.Logger) http.Handler {
	g := &gateway{cat: c, policy: p, log: log, clients: map[string]*upstream.Client{},
		tools: map[string]*catalog.Tool{}, servers: map[string]*mcp.Server{}}
	for _, t := range c.Tools {
		if g.clients[t.Service.Name] == nil {
			g.clients[t.Service.Name] = upstream.New(t.Service.Upstream)
		}
		g.tools[t.MCP.Name] = t
	}
	add := func(key string, tools []*catalog.Tool) {
		s := g.newServer(key)
		for _, t := range tools {
			s.AddTool(t.MCP, g.handler(t))
		}
		g.servers[key] = s
	}
	for name, tools := range c.Services {
		add("svc/"+name, tools)
	}
	for name, tools := range c.Bundles {
		add("bundle/"+name, tools)
	}

	stateless := &mcp.StreamableHTTPOptions{Stateless: true}
	server := func(r *http.Request) *mcp.Server { return g.servers[r.PathValue("kind")+"/"+r.PathValue("name")] }
	sdk := mcp.NewStreamableHTTPHandler(server, stateless)
	endpoint := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if server(r) == nil {
			http.NotFound(w, r)
			return
		}
		sdk.ServeHTTP(w, r)
	})
	omni := g.omni()
	omniHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return omni }, stateless)
	requireKey := auth.RequireBearerToken(p.Verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true}) // API keys don't expire

	mux := http.NewServeMux()
	mux.Handle("/{kind}/{name}/mcp", requireKey(endpoint))
	mux.Handle("/omni/mcp", requireKey(omniHandler))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","tools":%d,"endpoints":%d}`+"\n", len(c.Tools), len(g.servers)+1)
	})
	return mux
}

func (g *gateway) newServer(name string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "gerbang/" + name, Version: "0.1.0"}, nil)
	s.AddReceivingMiddleware(g.authorize)
	return s
}

// authorize hides tools the caller may not call and refuses calls to them.
func (g *gateway) authorize(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		who := caller(req)
		if who == nil {
			return nil, errors.New("unauthenticated")
		}
		switch method {
		case "tools/list":
			res, err := next(ctx, method, req)
			if lr, ok := res.(*mcp.ListToolsResult); ok {
				kept := []*mcp.Tool{}
				for _, tl := range lr.Tools {
					if t := g.tools[tl.Name]; t == nil || g.policy.Allowed(who, t.ID) { // nil: an omni meta-tool
						kept = append(kept, tl)
					}
				}
				lr.Tools = kept
			}
			return res, err
		case "tools/call":
			if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				if t := g.tools[params.Name]; t != nil && !g.policy.Allowed(who, t.ID) {
					g.log.Warn("tool call denied", "principal", who.Name, "actor", who.Actor, "tool", t.ID)
					return toolError("permission denied: %s may not call %s", who.Name, params.Name), nil
				}
			}
		}
		return next(ctx, method, req)
	}
}

func (g *gateway) handler(t *catalog.Tool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return g.run(ctx, t, req.Params.Arguments, caller(req), "direct"), nil // a Go error becomes a protocol error the model can't read
	}
}

// run calls one tool and writes the audit line.
func (g *gateway) run(ctx context.Context, t *catalog.Tool, raw json.RawMessage, who *config.Principal, via string) *mcp.CallToolResult {
	start := time.Now()
	res, outcome := call(ctx, t, g.clients[t.Service.Name], raw, who)
	g.log.Info("tool call", "principal", who.Name, "actor", who.Actor, "tool", t.ID, "via", via,
		"outcome", outcome, "ms", time.Since(start).Milliseconds())
	return res
}

func call(ctx context.Context, t *catalog.Tool, c *upstream.Client, raw json.RawMessage, who *config.Principal) (*mcp.CallToolResult, string) {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	// Decoded twice: the validator sees json.Number as a string; the request needs exact int64s.
	var plain any
	if err := json.Unmarshal(raw, &plain); err != nil {
		return toolError("arguments are not valid JSON: %v", err), "bad arguments"
	}
	if err := t.Schema.Validate(plain); err != nil {
		return toolError("invalid arguments: %v", err), "bad arguments"
	}
	var args map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&args); err != nil {
		return toolError("arguments are not a JSON object: %v", err), "bad arguments"
	}

	res, err := c.Do(ctx, t, args, who)
	if err != nil {
		return toolError("calling %s failed: %v", t.Service.Name, err), "error: " + err.Error()
	}
	text, outcome := string(res.Body), fmt.Sprintf("http %d", res.Status)
	if res.Truncated {
		text += fmt.Sprintf("\n[gerbang: response cut at %d bytes; narrow the request, e.g. with filters or per_page]", len(res.Body))
		outcome += " truncated"
	}
	if res.Status >= 400 {
		return toolError("%s returned %d %s: %s", t.Service.Name, res.Status, http.StatusText(res.Status), text), outcome
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, outcome
}

func caller(req mcp.Request) *config.Principal {
	if ex := req.GetExtra(); ex != nil {
		return policy.Principal(ex.TokenInfo)
	}
	return nil
}

func toolError(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}
