package sidecar

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"sidecar/internal/config"
)

func writeConfig(t *testing.T, path, level string) {
	t.Helper()
	body := "limits:\n  maxHeaderBytes: 4096\nlog:\n  level: " + level +
		"\nservices:\n  - name: svc\n    instances: [\"10.0.0.7:15000\"]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// The e2e harness cannot see this: its logger has no adjustable level.
func TestHandlerFollowsLogLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.yaml")
	writeConfig(t, path, "warn")

	cfg, err := config.New(path, config.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}

	var level slog.LevelVar
	Handler(cfg, &level, slog.New(slog.DiscardHandler))
	if got := level.Level(); got != slog.LevelWarn {
		t.Fatalf("level at startup = %v, want WARN", got)
	}

	writeConfig(t, path, "debug")
	if err := cfg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := level.Level(); got != slog.LevelDebug {
		t.Errorf("level after reload = %v, want DEBUG", got)
	}
}

func TestServerTakesLimitsFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.yaml")
	writeConfig(t, path, "info")

	cfg, err := config.New(path, config.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}

	srv := Server(cfg, Handler(cfg, nil, slog.New(slog.DiscardHandler)))
	if srv.MaxHeaderBytes != 4096 {
		t.Errorf("MaxHeaderBytes = %d, want 4096 from limits.maxHeaderBytes", srv.MaxHeaderBytes)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout unset: a client could hold a connection open sending headers forever")
	}
}
