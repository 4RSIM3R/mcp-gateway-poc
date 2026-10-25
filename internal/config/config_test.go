package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	cfg, err := Load("../../config/gerbang.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.Services {
		names = append(names, s.Name)
	}
	if want := []string{"fx", "github", "petstore", "weather"}; !slices.Equal(names, want) {
		t.Fatalf("services = %v, want %v", names, want)
	}

	fx, gh := cfg.Services[0].Upstream, cfg.Services[1].Upstream
	if fx.Timeout != 3*time.Second || fx.MaxConcurrency != 50 || fx.Retries != 1 {
		t.Errorf("fx = %+v, want its own timeout plus the gerbang.yaml defaults", fx)
	}
	if gh.Headers["User-Agent"] != "gerbang-poc/0.1" || gh.Headers["Accept"] != "application/vnd.github+json" {
		t.Errorf("github headers = %v, want the defaults merged with its own", gh.Headers)
	}
	if fx.Headers["Accept"] != "" {
		t.Error("github's headers leaked into fx")
	}
	if gh.Token != "test-token" || gh.RateLimit != (RateLimit{50, time.Hour}) {
		t.Errorf("github token %q, rate limit %+v", gh.Token, gh.RateLimit)
	}
}

func TestUnknownFieldFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "services", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "gerbang.yaml"), []byte("listen: 127.0.0.1:8080\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "services", "demo", "service.yaml"),
		[]byte("upstream:\n  base_url: http://x\n  tiemout: 3s\n"), 0o644)

	_, err := Load(filepath.Join(dir, "gerbang.yaml"))
	if err == nil || !strings.Contains(err.Error(), "field tiemout not found") {
		t.Fatalf("err = %v, want an unknown field error", err)
	}
}
