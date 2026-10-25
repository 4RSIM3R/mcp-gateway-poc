// Package config loads gerbang.yaml and each service.yaml.
package config

import (
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Listen      string              `yaml:"listen"`
	ServicesDir string              `yaml:"services_dir"`
	Defaults    Upstream            `yaml:"defaults"`
	Principals  []Principal         `yaml:"principals"`
	Policies    []Policy            `yaml:"policies"`
	Bundles     map[string][]string `yaml:"bundles"` // name → tool ID patterns
	Services    []Service           `yaml:"-"`
}

type Principal struct {
	Name  string   `yaml:"name"`
	Key   string   `yaml:"key"`
	Actor string   `yaml:"actor"` // human, agent or service
	Roles []string `yaml:"roles"`
}

type Policy struct {
	Name  string   `yaml:"name"`
	When  When     `yaml:"when"`
	Allow []string `yaml:"allow"` // tool ID patterns
}

type When struct {
	Roles []string `yaml:"roles"` // any of these
	Actor string   `yaml:"actor"` // empty matches any actor
}

type Service struct {
	Name     string   `yaml:"-"` // folder name
	Dir      string   `yaml:"-"`
	Spec     string   `yaml:"spec"` // relative to Dir
	Source   string   `yaml:"source"`
	Upstream Upstream `yaml:"upstream"`
	Expose   []Expose `yaml:"expose"`
}

type Upstream struct {
	BaseURL          string            `yaml:"base_url"`
	Timeout          time.Duration     `yaml:"timeout"`
	MaxConcurrency   int               `yaml:"max_concurrency"`
	Retries          int               `yaml:"retries"` // GET only
	MaxResponseBytes int64             `yaml:"max_response_bytes"`
	Headers          map[string]string `yaml:"headers"`
	TokenEnv         string            `yaml:"token_env"`
	RateLimit        RateLimit         `yaml:"rate_limit"` // zero means no limit
	ForwardIdentity  bool              `yaml:"forward_identity"`
	Token            string            `yaml:"-"` // read from TokenEnv
}

type RateLimit struct {
	Requests int           `yaml:"requests"`
	Per      time.Duration `yaml:"per"`
}

type Expose struct {
	Operation   string   `yaml:"operation"` // operationId; or use Method+Path
	Method      string   `yaml:"method"`
	Path        string   `yaml:"path"`
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	HideParams  []string `yaml:"hide_params"`
}

func Load(file string) (*Config, error) {
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		ServicesDir: "services",
		Defaults:    Upstream{Timeout: 10 * time.Second, MaxConcurrency: 32, MaxResponseBytes: 256 << 10},
	}
	if err := decode(file, &cfg); err != nil { // overrides the values above
		return nil, err
	}

	dir := filepath.Join(filepath.Dir(file), cfg.ServicesDir)
	paths, _ := filepath.Glob(filepath.Join(dir, "*", "service.yaml"))
	if len(paths) == 0 {
		return nil, fmt.Errorf("no service.yaml found under %s", dir)
	}
	for _, p := range paths { // sorted by name
		svc := Service{Name: filepath.Base(filepath.Dir(p)), Dir: filepath.Dir(p), Upstream: cfg.Defaults}
		svc.Upstream.Headers = maps.Clone(cfg.Defaults.Headers) // decode merges into this map
		if err := decode(p, &svc); err != nil {                 // overrides the defaults
			return nil, err
		}
		if svc.Upstream.BaseURL == "" {
			return nil, fmt.Errorf("%s: upstream.base_url is required", p)
		}
		svc.Upstream.BaseURL = strings.TrimSuffix(svc.Upstream.BaseURL, "/")
		if env := svc.Upstream.TokenEnv; env != "" {
			if svc.Upstream.Token = os.Getenv(env); svc.Upstream.Token == "" {
				slog.Warn("token not set, calling without it", "service", svc.Name, "env", env)
			}
		}
		cfg.Services = append(cfg.Services, svc)
	}
	return &cfg, nil
}

// decode is strict: unknown fields are errors.
func decode(file string, v any) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}
