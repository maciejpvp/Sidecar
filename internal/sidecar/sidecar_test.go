package sidecar

import (
	"log/slog"
	"testing"

	"sidecar/internal/config"
)

func testConfig(t *testing.T) *config.Sidecar {
	t.Helper()
	env := map[string]string{
		config.Env.Service:      "svc",
		config.Env.Advertise:    "10.0.0.7:15000",
		config.Env.ControlPlane: "127.0.0.1:1",
	}
	cfg, err := config.LoadSidecar("", func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}
	return cfg
}

func TestOutboundServerTakesLimitsFromConfig(t *testing.T) {
	cfg := testConfig(t)
	cfg.Limits.MaxHeaderBytes = 4096

	srv := New(cfg, slog.New(slog.DiscardHandler)).OutboundServer()
	if srv.MaxHeaderBytes != 4096 {
		t.Errorf("MaxHeaderBytes = %d, want 4096 from limits.maxHeaderBytes", srv.MaxHeaderBytes)
	}
	if srv.ReadHeaderTimeout <= 0 {
		t.Error("ReadHeaderTimeout unset: a client could hold a connection open sending headers forever")
	}
}

func TestStartsWithNoRoutes(t *testing.T) {
	sc := New(testConfig(t), slog.New(slog.DiscardHandler))
	if sc.Routes.Ready() {
		t.Error("a sidecar that has not heard from the control plane has routes")
	}
}
