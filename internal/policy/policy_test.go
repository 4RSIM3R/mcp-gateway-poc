package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"ilzam.dev/gerbang/internal/config"
)

func TestRepoPolicies(t *testing.T) {
	cfg, err := config.Load("../../config/gerbang.yaml")
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ti, err := p.Verify(context.Background(), "dev-key-ops-bot", nil)
	if err != nil || Principal(ti).Name != "ops-bot" {
		t.Fatalf("Verify(ops-bot key) = %v, %v", ti, err)
	}
	if _, err := p.Verify(context.Background(), "wrong", nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Verify(wrong key) error = %v, want ErrInvalidToken", err)
	}

	bot := Principal(ti)
	alice := &cfg.Principals[1]
	managerBot := &config.Principal{Name: "manager-bot", Actor: "agent", Roles: []string{"store-manager"}}
	for _, tc := range []struct {
		who  *config.Principal
		tool string
		want bool
	}{
		{bot, "petstore.getPetById", true},
		{bot, "github.repos_get", true},
		{bot, "petstore.deletePet", false},
		{alice, "petstore.deletePet", true},
		{alice, "fx.getLatestRates", true},
		{managerBot, "petstore.deletePet", false}, // store writes need a human
		{nil, "fx.getLatestRates", false},
	} {
		if got := p.Allowed(tc.who, tc.tool); got != tc.want {
			t.Errorf("Allowed(%v, %s) = %v, want %v", tc.who, tc.tool, got, tc.want)
		}
	}
}

func TestSharedKeyFails(t *testing.T) {
	cfg := &config.Config{Principals: []config.Principal{{Name: "a", Key: "k"}, {Name: "b", Key: "k"}}}
	if _, err := New(cfg); err == nil {
		t.Error("want an error for two principals sharing a key")
	}
}
