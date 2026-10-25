package gateway

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestOmni(t *testing.T) {
	url := setup(t)
	bot := connect(t, url+"/omni/mcp", "bot-key")
	lead := connect(t, url+"/omni/mcp", "lead-key")

	want := []string{"discover_servers", "discover_tools", "get_tool_schema", "invoke_tool"}
	if got := toolNames(t, bot); !slices.Equal(got, want) {
		t.Fatalf("omni tools = %v, want %v", got, want)
	}

	var servers serversOut
	text, _ := callTool(t, bot, "discover_servers", `{}`)
	json.Unmarshal([]byte(text), &servers)
	if len(servers.Servers) != 1 || servers.Servers[0].Name != "pets" || servers.Servers[0].Tools != 1 {
		t.Errorf("bot discover_servers = %s, want pets with 1 tool", text)
	}

	var tools toolsOut
	text, _ = callTool(t, bot, "discover_tools", `{"query":"delete"}`)
	json.Unmarshal([]byte(text), &tools)
	if len(tools.Tools) != 0 {
		t.Errorf("bot found %s, want no delete tool", text)
	}
	text, _ = callTool(t, lead, "discover_tools", `{"server":"pets","query":"delete"}`)
	json.Unmarshal([]byte(text), &tools)
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "pets_deletePet" {
		t.Errorf("lead discover_tools = %s, want pets_deletePet", text)
	}

	// To bot, deletePet does not exist: not in the schema, not through invoke.
	for _, c := range []struct{ tool, args string }{
		{"get_tool_schema", `{"name":"pets_deletePet"}`},
		{"invoke_tool", `{"name":"pets_deletePet","arguments":{"petId":1}}`},
	} {
		if text, isErr := callTool(t, bot, c.tool, c.args); !isErr || !strings.Contains(text, "unknown tool") {
			t.Errorf("bot %s = %q (error %v), want unknown tool", c.tool, text, isErr)
		}
	}

	if text, isErr := callTool(t, bot, "get_tool_schema", `{"name":"pets_getPet"}`); isErr || !strings.Contains(text, `"petId"`) {
		t.Errorf("get_tool_schema = %q (error %v)", text, isErr)
	}
	if text, isErr := callTool(t, bot, "invoke_tool", `{"name":"pets_getPet","arguments":{"petId":9007199254740993}}`); isErr || text != `{"got":"GET /pets/9007199254740993"}` {
		t.Errorf("invoke_tool getPet = %q (error %v)", text, isErr)
	}
	if text, isErr := callTool(t, bot, "invoke_tool", `{"name":"pets_getPet","arguments":{"petID":1}}`); !isErr || !strings.Contains(text, "invalid arguments") {
		t.Errorf("invoke_tool with a typo = %q (error %v), want invalid arguments", text, isErr)
	}
}
