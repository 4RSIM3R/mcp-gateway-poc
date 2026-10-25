// Package policy decides who is calling and which tools they may call.
package policy

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
)

type Policy struct {
	byKey map[string]*config.Principal
	rules []config.Policy
}

const principalKey = "gerbang.principal" // in auth.TokenInfo.Extra

func New(cfg *config.Config) (*Policy, error) {
	p := &Policy{byKey: map[string]*config.Principal{}, rules: cfg.Policies}
	for i := range cfg.Principals {
		pr := &cfg.Principals[i]
		if pr.Key == "" {
			return nil, fmt.Errorf("principal %q has no key", pr.Name)
		}
		if other := p.byKey[pr.Key]; other != nil {
			return nil, fmt.Errorf("principals %q and %q share a key", other.Name, pr.Name)
		}
		p.byKey[pr.Key] = pr
	}
	for _, r := range cfg.Policies {
		for _, pat := range r.Allow {
			if _, err := path.Match(pat, ""); err != nil {
				return nil, fmt.Errorf("policy %q: bad pattern %q", r.Name, pat)
			}
		}
	}
	return p, nil
}

// Verify is an auth.TokenVerifier for the API keys in gerbang.yaml.
func (p *Policy) Verify(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	pr := p.byKey[token]
	if pr == nil {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: pr.Name, Extra: map[string]any{principalKey: pr}}, nil
}

// Principal returns the caller that Verify attached, or nil.
func Principal(ti *auth.TokenInfo) *config.Principal {
	if ti == nil {
		return nil
	}
	pr, _ := ti.Extra[principalKey].(*config.Principal)
	return pr
}

// Allowed reports whether any policy lets pr call the tool.
func (p *Policy) Allowed(pr *config.Principal, toolID string) bool {
	if pr == nil {
		return false
	}
	for _, r := range p.rules {
		if r.When.Actor != "" && r.When.Actor != pr.Actor {
			continue
		}
		hasRole := slices.ContainsFunc(r.When.Roles, func(role string) bool { return slices.Contains(pr.Roles, role) })
		if hasRole && catalog.Match(r.Allow, toolID) {
			return true
		}
	}
	return false
}
