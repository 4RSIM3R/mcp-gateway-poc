package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
	"ilzam.dev/gerbang/internal/policy"
)

const gerbangYAML = `
principals:
  - {name: bot, key: bot-key, actor: agent, roles: [reader]}
  - {name: lead, key: lead-key, actor: human, roles: [reader, admin]}
policies:
  - {name: read, when: {roles: [reader]}, allow: ["pets.get*"]}
  - {name: admin, when: {roles: [admin], actor: human}, allow: ["pets.*"]}
bundles:
  everything: ["*"]
`

const petsSpec = `
openapi: 3.0.3
info: {title: Pets, version: "1"}
paths:
  /pets/{petId}:
    parameters:
      - {name: petId, in: path, required: true, schema: {type: integer, format: int64}}
    get:
      operationId: getPet
      responses: {"200": {description: ok}}
    delete:
      operationId: deletePet
      responses: {"200": {description: ok}}
`

// setup runs a fake pets API behind a real gateway and returns the gateway URL.
func setup(t *testing.T) string {
	pets := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/404") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"message":"pet not found"}`)
			return
		}
		fmt.Fprintf(w, `{"got":%q}`, r.Method+" "+r.URL.Path) // echo, so tests see what arrived
	}))
	t.Cleanup(pets.Close)

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "services", "pets"), 0o755)
	os.WriteFile(filepath.Join(dir, "gerbang.yaml"), []byte(gerbangYAML), 0o644)
	os.WriteFile(filepath.Join(dir, "services", "pets", "openapi.yml"), []byte(petsSpec), 0o644)
	os.WriteFile(filepath.Join(dir, "services", "pets", "service.yaml"), []byte(
		"spec: openapi.yml\nupstream: {base_url: "+pets.URL+"}\nexpose:\n  - operation: getPet\n  - operation: deletePet\n"), 0o644)

	cfg, err := config.Load(filepath.Join(dir, "gerbang.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pol, err := policy.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(New(cat, pol, slog.New(slog.DiscardHandler)))
	t.Cleanup(gw.Close)
	return gw.URL
}

type bearer string

func (key bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(key))
	return http.DefaultTransport.RoundTrip(r)
}

func connect(t *testing.T, endpoint, key string) *mcp.ClientSession {
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearer(key)}, DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func callTool(t *testing.T, cs *mcp.ClientSession, tool, args string) (text string, isError bool) {
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

func TestGateway(t *testing.T) {
	url := setup(t)
	bot := connect(t, url+"/svc/pets/mcp", "bot-key")
	lead := connect(t, url+"/svc/pets/mcp", "lead-key")

	if got := toolNames(t, bot); !slices.Equal(got, []string{"pets_getPet"}) {
		t.Errorf("bot sees %v, want only pets_getPet", got)
	}
	if got := toolNames(t, lead); !slices.Equal(got, []string{"pets_deletePet", "pets_getPet"}) {
		t.Errorf("lead sees %v, want both tools", got)
	}

	// 2^53+1: survives only if no float64 sits between client and upstream.
	if text, isErr := callTool(t, bot, "pets_getPet", `{"petId":9007199254740993}`); isErr || text != `{"got":"GET /pets/9007199254740993"}` {
		t.Errorf("getPet = %q (error %v)", text, isErr)
	}
	if text, isErr := callTool(t, bot, "pets_deletePet", `{"petId":1}`); !isErr || !strings.Contains(text, "permission denied") {
		t.Errorf("bot deletePet = %q (error %v), want permission denied", text, isErr)
	}
	if text, isErr := callTool(t, lead, "pets_deletePet", `{"petId":404}`); !isErr || !strings.Contains(text, "404 Not Found") {
		t.Errorf("deletePet 404 = %q (error %v), want the upstream 404", text, isErr)
	}
	if text, isErr := callTool(t, bot, "pets_getPet", `{"petID":1}`); !isErr || !strings.Contains(text, "invalid arguments") {
		t.Errorf("getPet with a typo = %q (error %v), want invalid arguments", text, isErr)
	}

	bundle := connect(t, url+"/bundle/everything/mcp", "bot-key")
	if got := toolNames(t, bundle); !slices.Equal(got, []string{"pets_getPet"}) {
		t.Errorf("bot sees %v in the bundle, want only pets_getPet", got)
	}
}

func TestHTTPErrors(t *testing.T) {
	url := setup(t)
	post := func(path, key string) int {
		req, _ := http.NewRequest("POST", url+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("/svc/pets/mcp", ""); code != http.StatusUnauthorized {
		t.Errorf("no key: %d, want 401", code)
	}
	if code := post("/svc/pets/mcp", "wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong key: %d, want 401", code)
	}
	if code := post("/svc/nope/mcp", "bot-key"); code != http.StatusNotFound {
		t.Errorf("unknown endpoint: %d, want 404", code)
	}
	res, err := http.Get(url + "/healthz")
	if err != nil || res.StatusCode != http.StatusOK {
		t.Errorf("healthz: %v, %v", res, err)
	}
}
