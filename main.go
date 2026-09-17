package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sidecar/internal/logging"
	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

// Outbound listener: loopback only, so the sidecar can never be used as an
// open proxy into the mesh (DESIGN §2.1).
const outboundAddr = "127.0.0.1:15001"

func main() {
	var level slog.LevelVar
	level.Set(slog.LevelDebug)
	logger := logging.New(&level)
	slog.SetDefault(logger)

	table := routing.NewTable(map[string][]string{
		"service1": {"https://www.youtube.com/"},
		"billing":  {"http://10.0.0.1:8080", "http://10.0.0.2:8080", "http://10.0.0.3:8080"},
	})

	p := proxy.New(table, logger)

	ln, err := net.Listen("tcp", outboundAddr)
	if err != nil {
		slog.Error("outbound listener failed to bind", "addr", outboundAddr, "error", err)
		os.Exit(1)
	}

	slog.Info("ready", "dir", "outbound", "addr", ln.Addr().String())

	srv := &http.Server{Handler: p}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("outbound listener stopped", "addr", outboundAddr, "error", err)
		os.Exit(1)
	}
}
