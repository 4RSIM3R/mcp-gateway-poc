// Command gerbang is an MCP gateway in front of existing HTTP APIs.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"ilzam.dev/gerbang/internal/catalog"
	"ilzam.dev/gerbang/internal/config"
	"ilzam.dev/gerbang/internal/gateway"
	"ilzam.dev/gerbang/internal/policy"
)

func main() {
	file := flag.String("config", "config/gerbang.yaml", "path to gerbang.yaml")
	check := flag.Bool("check", false, "load the config, list the tools, and exit")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	if err := run(*file, *check); err != nil {
		slog.Error("gerbang stopped", "err", err)
		os.Exit(1)
	}
}

func run(file string, check bool) error {
	cfg, err := config.Load(file)
	if err != nil {
		return err
	}
	cat, err := catalog.Build(cfg)
	if err != nil {
		return err
	}
	pol, err := policy.New(cfg)
	if err != nil {
		return err
	}
	endpoints := []string{"/omni/mcp"}
	for name := range cat.Services {
		endpoints = append(endpoints, "/svc/"+name+"/mcp")
	}
	for name := range cat.Bundles {
		endpoints = append(endpoints, "/bundle/"+name+"/mcp")
	}
	slices.Sort(endpoints)

	if check {
		for _, t := range cat.Tools {
			fmt.Printf("%-40s %-6s %s\n", t.ID, t.Method, t.Path)
		}
		fmt.Printf("\nconfig OK: %d tools on %v\n", len(cat.Tools), endpoints)
		return nil
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: gateway.New(cat, pol, slog.Default()), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("gerbang listening", "addr", cfg.Listen, "tools", len(cat.Tools), "endpoints", endpoints)

	select {
	case err := <-errc: // could not start, e.g. port in use
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down, waiting for in-flight calls")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}
