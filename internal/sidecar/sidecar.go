// Package sidecar wires config, routing and proxy together. main and the e2e
// harness both use it, so the wiring the tests exercise is the one that ships.
package sidecar

import (
	"log/slog"
	"net/http"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

// Handler builds the outbound proxy and keeps its routes, log level and
// connection pool in step with every reload. level may be nil.
func Handler(cfg *config.Source, level *slog.LevelVar, log *slog.Logger) *proxy.Handler {
	c := cfg.Current()
	if level != nil {
		level.Set(c.Log.Level)
	}

	routes := routing.NewStore(routing.NewTable(c.Services))
	p := proxy.New(routes, log)

	cfg.OnChange(func(_, next *config.Config) {
		if level != nil {
			level.Set(next.Log.Level)
		}
		// Swap first, so no request opens a connection to a removed instance
		// after the pool is cleared.
		routes.Swap(routing.NewTable(next.Services))
		p.CloseIdleConnections()
	})
	return p
}

// Server is the outbound http.Server; its settings are restart-only.
func Server(cfg *config.Source, h http.Handler) *http.Server {
	// No ReadTimeout/WriteTimeout: they would cap the whole exchange, fighting
	// the per-service deadline and cutting off streaming responses.
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    cfg.Current().Limits.MaxHeaderBytes,
	}
}
