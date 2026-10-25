// Package catalog turns exposed OpenAPI operations into MCP tools.
package catalog

import (
	"cmp"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ilzam.dev/gerbang/internal/config"
)

type Catalog struct {
	Tools    []*Tool
	Services map[string][]*Tool // service name → tools
	Bundles  map[string][]*Tool // bundle name → tools
	About    map[string]string  // service name → one line from the spec's info
}

type Tool struct {
	ID      string // "service.name", used by policies
	Service *config.Service
	MCP     *mcp.Tool
	Summary string               // one line, for discovery
	Schema  *jsonschema.Resolved // validates arguments
	Method  string
	Path    string // template, e.g. /pet/{petId}
	Params  []Param
	Body    bool // arguments["body"] is the JSON body
}

type Param struct {
	Name    string
	In      string // path, query or header
	Explode bool   // query arrays: a=1&a=2 rather than a=1,2
}

var invalidChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

const maxDepth = 8 // deeper or recursive schemas accept anything

func Build(cfg *config.Config) (*Catalog, error) {
	c := &Catalog{Services: map[string][]*Tool{}, Bundles: map[string][]*Tool{}, About: map[string]string{}}
	seen := map[string]bool{}
	for i := range cfg.Services {
		svc := &cfg.Services[i]
		doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join(svc.Dir, svc.Spec))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", svc.Name, err)
		}
		if doc.Info != nil {
			c.About[svc.Name] = joinText(doc.Info.Title, firstSentence(doc.Info.Description))
		}
		for _, e := range svc.Expose {
			t, err := build(svc, doc, e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", svc.Name, err)
			}
			if seen[t.ID] {
				return nil, fmt.Errorf("%s: duplicate tool %s", svc.Name, t.ID)
			}
			seen[t.ID] = true
			c.Tools = append(c.Tools, t)
			c.Services[svc.Name] = append(c.Services[svc.Name], t)
		}
	}
	for name, patterns := range cfg.Bundles {
		for _, t := range c.Tools {
			if Match(patterns, t.ID) {
				c.Bundles[name] = append(c.Bundles[name], t)
			}
		}
		if len(c.Bundles[name]) == 0 {
			return nil, fmt.Errorf("bundle %s matches no tools", name)
		}
	}
	return c, nil
}

// Match reports whether id matches any pattern, such as "petstore.get*".
func Match(patterns []string, id string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, id); ok {
			return true
		}
	}
	return false
}

func build(svc *config.Service, doc *openapi3.T, e config.Expose) (*Tool, error) {
	label := cmp.Or(e.Operation, e.Method+" "+e.Path)
	method, tmpl, item, op := find(doc, e)
	if op == nil {
		return nil, fmt.Errorf("%s: not found in the spec", label)
	}
	name := invalidChars.ReplaceAllString(cmp.Or(e.Name, op.OperationID), "_")
	if name == "" {
		return nil, fmt.Errorf("%s: no operationId, so set name", label)
	}
	t := &Tool{ID: svc.Name + "." + name, Service: svc, Method: method, Path: tmpl}

	in := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}}, // reject unknown arguments
	}
	add := func(arg string, s *jsonschema.Schema, required bool) error {
		if _, dup := in.Properties[arg]; dup {
			return fmt.Errorf("%s: two arguments named %q", label, arg)
		}
		in.Properties[arg] = s
		in.PropertyOrder = append(in.PropertyOrder, arg)
		if required {
			in.Required = append(in.Required, arg)
		}
		return nil
	}

	params := mergeParams(item, op)
	for _, h := range e.HideParams { // a typo here would expose the parameter
		if !slices.ContainsFunc(params, func(p *openapi3.ParameterRef) bool { return p.Value.Name == h }) {
			return nil, fmt.Errorf("%s: hide_params: no parameter %q", label, h)
		}
	}
	for _, ref := range params {
		p := ref.Value
		if slices.Contains(e.HideParams, p.Name) {
			if p.Required {
				return nil, fmt.Errorf("%s: cannot hide required parameter %q", label, p.Name)
			}
			continue
		}
		s := &jsonschema.Schema{Type: "string"}
		if p.Schema != nil {
			s = convert(p.Schema.Value, 0)
		}
		s.Description = joinText(p.Description, s.Description)
		if err := add(p.Name, s, p.Required || p.In == "path"); err != nil {
			return nil, err
		}
		sm, _ := p.SerializationMethod()
		t.Params = append(t.Params, Param{Name: p.Name, In: p.In, Explode: sm != nil && sm.Explode})
	}

	if rb := op.RequestBody; rb != nil && rb.Value != nil {
		mt := rb.Value.Content.Get("application/json")
		switch {
		case mt != nil && mt.Schema != nil:
			s := convert(mt.Schema.Value, 0)
			s.Description = joinText(rb.Value.Description, s.Description)
			if err := add("body", s, rb.Value.Required); err != nil {
				return nil, err
			}
			t.Body = true
		case rb.Value.Required:
			return nil, fmt.Errorf("%s: only JSON request bodies are supported", label)
		}
	}

	rs, err := in.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("%s: input schema: %w", label, err)
	}
	t.Schema = rs
	desc := strings.TrimSpace(cmp.Or(e.Description, op.Description, op.Summary))
	t.Summary = joinText(op.Summary, firstSentence(desc))
	t.MCP = &mcp.Tool{
		Name:        svc.Name + "_" + name,
		Title:       strings.TrimSpace(op.Summary),
		Description: desc,
		InputSchema: in,
		Annotations: annotations(method),
	}
	return t, nil
}

// find looks an operation up by operationId, or by method and path.
func find(doc *openapi3.T, e config.Expose) (method, tmpl string, item *openapi3.PathItem, op *openapi3.Operation) {
	for p, it := range doc.Paths.Map() {
		for m, o := range it.Operations() {
			byID := e.Operation != "" && o.OperationID == e.Operation
			byRoute := e.Operation == "" && strings.EqualFold(m, e.Method) && p == e.Path
			if byID || byRoute {
				return m, p, it, o
			}
		}
	}
	return "", "", nil, nil
}

// mergeParams puts path-level parameters first; the operation's win on a clash.
func mergeParams(item *openapi3.PathItem, op *openapi3.Operation) openapi3.Parameters {
	var ps openapi3.Parameters
	for _, p := range item.Parameters {
		if op.Parameters.GetByInAndName(p.Value.In, p.Value.Name) == nil {
			ps = append(ps, p)
		}
	}
	return append(ps, op.Parameters...)
}

func annotations(method string) *mcp.ToolAnnotations {
	a := &mcp.ToolAnnotations{OpenWorldHint: new(true)}
	switch method {
	case "GET", "HEAD":
		a.ReadOnlyHint = true
	case "PUT", "DELETE":
		a.IdempotentHint = true
	}
	return a
}

// convert maps an OpenAPI schema to JSON Schema; the loader already resolved $refs.
func convert(s *openapi3.Schema, depth int) *jsonschema.Schema {
	if s == nil || depth > maxDepth {
		return &jsonschema.Schema{}
	}
	out := &jsonschema.Schema{
		Description: strings.TrimSpace(s.Description),
		Format:      s.Format,
		Enum:        s.Enum,
		Pattern:     s.Pattern,
		Minimum:     s.Min,
		Maximum:     s.Max,
		MaxLength:   toInt(s.MaxLength),
		MaxItems:    toInt(s.MaxItems),
		Required:    s.Required,
		AllOf:       convertAll(s.AllOf, depth),
		AnyOf:       convertAll(s.AnyOf, depth),
		OneOf:       convertAll(s.OneOf, depth),
	}
	types := slices.Clone(s.Type.Slice())
	if s.Nullable && len(types) > 0 && !slices.Contains(types, "null") { // OpenAPI 3.0
		types = append(types, "null")
	}
	if len(types) == 1 {
		out.Type = types[0]
	} else if len(types) > 1 {
		out.Types = types
	}
	if s.MinLength > 0 {
		out.MinLength = new(int(s.MinLength))
	}
	if s.MinItems > 0 {
		out.MinItems = new(int(s.MinItems))
	}
	if s.Default != nil {
		out.Default, _ = json.Marshal(s.Default)
	}
	if s.Items != nil {
		out.Items = convert(s.Items.Value, depth+1)
	}
	for name, p := range s.Properties {
		if out.Properties == nil {
			out.Properties = map[string]*jsonschema.Schema{}
		}
		out.Properties[name] = convert(p.Value, depth+1)
	}
	if ap := s.AdditionalProperties; ap.Schema != nil {
		out.AdditionalProperties = convert(ap.Schema.Value, depth+1)
	} else if ap.Has != nil && !*ap.Has {
		out.AdditionalProperties = &jsonschema.Schema{Not: &jsonschema.Schema{}}
	}
	return out
}

func convertAll(refs openapi3.SchemaRefs, depth int) []*jsonschema.Schema {
	var out []*jsonschema.Schema
	for _, r := range refs {
		out = append(out, convert(r.Value, depth+1))
	}
	return out
}

func toInt(u *uint64) *int {
	if u == nil {
		return nil
	}
	return new(int(*u))
}

// firstSentence returns the first line, cut after its first ". ".
func firstSentence(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	return s
}

// joinText joins a parameter's description with its schema's.
func joinText(a, b string) string {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "" || a == b:
		return a
	}
	return strings.TrimSuffix(a, ".") + ". " + b
}
