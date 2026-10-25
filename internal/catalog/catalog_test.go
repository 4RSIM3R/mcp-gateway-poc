package catalog

import (
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/jsonschema-go/jsonschema"

	"ilzam.dev/gerbang/internal/config"
)

func TestBuildRepoConfig(t *testing.T) {
	cfg, err := config.Load("../../config/gerbang.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Tools) != 28 {
		t.Errorf("got %d tools, want 28", len(c.Tools))
	}
	byID := map[string]*Tool{}
	for _, tl := range c.Tools {
		byID[tl.ID] = tl
	}

	period := byID["fx.getRatesForPeriod"]
	if period == nil || period.MCP.Name != "fx_getRatesForPeriod" || period.Path != "/{start_date}..{end_date}" {
		t.Fatalf("fx.getRatesForPeriod = %+v", period)
	}
	in := period.MCP.InputSchema.(*jsonschema.Schema)
	if !slices.Equal(in.Required, []string{"start_date", "end_date"}) {
		t.Errorf("required = %v", in.Required)
	}
	for _, p := range period.Params {
		if p.Name == "symbols" && p.Explode {
			t.Error("symbols should not explode (the spec says explode: false)")
		}
	}
	if !period.MCP.Annotations.ReadOnlyHint {
		t.Error("a GET tool should be read-only")
	}
	if err := period.Schema.Validate(map[string]any{"start_date": "2026-10-01", "end_date": "2026-10-07", "symbols": []any{"IDR"}}); err != nil {
		t.Errorf("valid arguments rejected: %v", err)
	}
	if err := period.Schema.Validate(map[string]any{"startDate": "2026-10-01"}); err == nil {
		t.Error("unknown argument accepted")
	}

	if byID["github.repos_get"] == nil {
		t.Error("repos/get should become github.repos_get")
	}
	if _, ok := byID["weather.getForecast"].MCP.InputSchema.(*jsonschema.Schema).Properties["apikey"]; ok {
		t.Error("apikey should be hidden")
	}
	if add := byID["petstore.addPet"]; !add.Body || !slices.Contains(add.MCP.InputSchema.(*jsonschema.Schema).Required, "body") {
		t.Error("addPet should take a required JSON body")
	}
	if n, m := len(c.Bundles["trip-planner"]), len(c.Bundles["pet-shop"]); n != 5 || m != 11 {
		t.Errorf("bundle sizes = %d, %d; want 5, 11", n, m)
	}
}

func TestConvertNullable(t *testing.T) {
	want := []string{"string", "null"}
	v30 := convert(&openapi3.Schema{Type: &openapi3.Types{"string"}, Nullable: true}, 0)
	v31 := convert(&openapi3.Schema{Type: &openapi3.Types{"string", "null"}}, 0)
	if !slices.Equal(v30.Types, want) || !slices.Equal(v31.Types, want) {
		t.Errorf("3.0 nullable = %v, 3.1 = %v; want %v for both", v30.Types, v31.Types, want)
	}
}
