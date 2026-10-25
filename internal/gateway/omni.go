package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ilzam.dev/gerbang/internal/catalog"
)

const maxResults = 20

// omni serves four meta-tools, so agents discover tools instead of loading them all.
func (g *gateway) omni() *mcp.Server {
	s := g.newServer("omni")
	read := &mcp.ToolAnnotations{ReadOnlyHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "discover_servers", Annotations: read,
		Description: "Start here. Lists the services you can use, optionally filtered by keywords."}, g.discoverServers)
	mcp.AddTool(s, &mcp.Tool{Name: "discover_tools", Annotations: read,
		Description: "Lists tools in one server, or searches all servers, optionally filtered by keywords."}, g.discoverTools)
	mcp.AddTool(s, &mcp.Tool{Name: "get_tool_schema", Annotations: read,
		Description: "Returns a tool's full description and input schema. Read it before invoke_tool."}, g.getToolSchema)
	s.AddTool(&mcp.Tool{Name: "invoke_tool", InputSchema: invokeSchema, Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
		Description: "Calls a tool by name, with arguments matching its input schema."}, g.invokeTool)
	return s
}

type discoverServersIn struct {
	Query string `json:"query,omitempty" jsonschema:"keywords such as currency or weather; empty lists everything"`
}

type serverInfo struct {
	Name  string `json:"name"`
	About string `json:"about"`
	Tools int    `json:"tools"` // how many the caller may use
}

type serversOut struct {
	Servers []serverInfo `json:"servers"`
}

func (g *gateway) discoverServers(_ context.Context, req *mcp.CallToolRequest, in discoverServersIn) (*mcp.CallToolResult, serversOut, error) {
	who := caller(req)
	out := serversOut{Servers: []serverInfo{}} // not nil: null fails the output schema
	for _, name := range slices.Sorted(maps.Keys(g.cat.Services)) {
		text, n := name+" "+g.cat.About[name], 0
		for _, t := range g.cat.Services[name] {
			if g.policy.Allowed(who, t.ID) {
				text += " " + t.MCP.Name + " " + t.Summary
				n++
			}
		}
		if n > 0 && score(in.Query, text) > 0 {
			out.Servers = append(out.Servers, serverInfo{Name: name, About: g.cat.About[name], Tools: n})
		}
	}
	return nil, out, nil
}

type discoverToolsIn struct {
	Server string `json:"server,omitempty" jsonschema:"a name from discover_servers; empty searches every server"`
	Query  string `json:"query,omitempty" jsonschema:"keywords such as rates or forecast"`
}

type toolInfo struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
}

type toolsOut struct {
	Tools []toolInfo `json:"tools"`
}

func (g *gateway) discoverTools(_ context.Context, req *mcp.CallToolRequest, in discoverToolsIn) (*mcp.CallToolResult, toolsOut, error) {
	tools := g.cat.Tools
	if in.Server != "" {
		if tools = g.cat.Services[in.Server]; tools == nil {
			return nil, toolsOut{}, fmt.Errorf("unknown server %q; see discover_servers", in.Server)
		}
	}
	who := caller(req)
	type hit struct {
		t     *catalog.Tool
		score int
	}
	var hits []hit
	for _, t := range tools {
		if s := score(in.Query, t.ID+" "+t.Summary); s > 0 && g.policy.Allowed(who, t.ID) {
			hits = append(hits, hit{t, s})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return b.score - a.score })
	out := toolsOut{Tools: []toolInfo{}}
	for _, h := range hits[:min(len(hits), maxResults)] {
		out.Tools = append(out.Tools, toolInfo{Name: h.t.MCP.Name, Summary: h.t.Summary})
	}
	return nil, out, nil
}

type toolNameIn struct {
	Name string `json:"name" jsonschema:"a tool name from discover_tools"`
}

type schemaOut struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ReadOnly    bool   `json:"read_only"`
	InputSchema any    `json:"input_schema"`
}

func (g *gateway) getToolSchema(_ context.Context, req *mcp.CallToolRequest, in toolNameIn) (*mcp.CallToolResult, schemaOut, error) {
	t, err := g.visible(req, in.Name)
	if err != nil {
		return nil, schemaOut{}, err
	}
	return nil, schemaOut{t.MCP.Name, t.MCP.Description, t.MCP.Annotations.ReadOnlyHint, t.MCP.InputSchema}, nil
}

var invokeSchema = &jsonschema.Schema{
	Type:     "object",
	Required: []string{"name"},
	Properties: map[string]*jsonschema.Schema{
		"name":      {Type: "string", Description: "a tool name from discover_tools"},
		"arguments": {Type: "object", Description: "arguments matching the tool's input_schema"},
	},
}

// invokeTool is a raw handler so arguments stay raw JSON, keeping int64s exact.
func (g *gateway) invokeTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var in struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &in); err != nil {
		return toolError("invalid invoke_tool arguments: %v", err), nil
	}
	t, err := g.visible(req, in.Name) // the policy check for the inner tool
	if err != nil {
		return toolError("%v", err), nil
	}
	return g.run(ctx, t, in.Arguments, caller(req), "omni"), nil
}

// visible finds a tool the caller may call; to the caller, others don't exist.
func (g *gateway) visible(req mcp.Request, name string) (*catalog.Tool, error) {
	who, t := caller(req), g.tools[name]
	if t != nil && g.policy.Allowed(who, t.ID) {
		return t, nil
	}
	if t != nil {
		g.log.Warn("tool call denied", "principal", who.Name, "actor", who.Actor, "tool", t.ID, "via", "omni")
	}
	return nil, fmt.Errorf("unknown tool %q; see discover_tools", name)
}

// score counts the query words found in text; an empty query matches everything.
func score(query, text string) int {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return 1
	}
	text = strings.ToLower(text)
	n := 0
	for _, w := range words {
		if strings.Contains(text, w) {
			n++
		}
	}
	return n
}
