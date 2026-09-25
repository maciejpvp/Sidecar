package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loadMesh(t *testing.T, body string) (*Mesh, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mesh.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write mesh: %v", err)
	}
	return LoadMesh(path)
}

func mustLoadMesh(t *testing.T, body string) *Mesh {
	t.Helper()
	m, err := loadMesh(t, body)
	if err != nil {
		t.Fatalf("LoadMesh: %v", err)
	}
	return m
}

const minimalMesh = `
services:
  - name: orders-svc
`

func TestMinimalMeshGetsDefaults(t *testing.T) {
	m := mustLoadMesh(t, minimalMesh)

	if got, want := m.Registry.LeaseTTL, 15*time.Second; got != want {
		t.Errorf("registry.leaseTTL = %v, want %v", got, want)
	}
	if got, want := m.Registry.Heartbeat(), 5*time.Second; got != want {
		t.Errorf("heartbeat = %v, want %v", got, want)
	}
	if got, want := m.Log.Level, slog.LevelInfo; got != want {
		t.Errorf("log.level = %v, want %v", got, want)
	}
	if len(m.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(m.Services))
	}

	svc := m.Services[0]
	if svc.Name != "orders-svc" {
		t.Errorf("name = %q, want orders-svc", svc.Name)
	}
	if got, want := svc.Timeout, 5*time.Second; got != want {
		t.Errorf("timeout = %v, want %v", got, want)
	}
	if got, want := svc.Retry.MaxAttempts, 3; got != want {
		t.Errorf("retry.maxAttempts = %d, want %d", got, want)
	}
	if got, want := svc.Retry.Backoff.Max, 250*time.Millisecond; got != want {
		t.Errorf("retry.backoff.max = %v, want %v", got, want)
	}
	if got, want := svc.Outlier.ConsecutiveFailures, 5; got != want {
		t.Errorf("outlier.consecutiveFailures = %d, want %d", got, want)
	}
}

// Overriding one knob must not reset its siblings to the built-in value.
func TestServiceInheritsFieldByField(t *testing.T) {
	m := mustLoadMesh(t, `
defaults:
  timeout: 4s
  retry:
    maxAttempts: 5
    backoff:
      base: 10ms
      max: 100ms
  outlier:
    consecutiveFailures: 7

services:
  - name: payments-svc
    retry:
      maxAttempts: 1

  - name: orders-svc
    timeout: 2s
`)

	payments, orders := m.Services[0], m.Services[1]

	if got, want := payments.Retry.MaxAttempts, 1; got != want {
		t.Errorf("payments retry.maxAttempts = %d, want %d", got, want)
	}
	// Not reset by the retry override above it.
	if got, want := payments.Retry.Backoff.Base, 10*time.Millisecond; got != want {
		t.Errorf("payments retry.backoff.base = %v, want %v (from defaults)", got, want)
	}
	if got, want := payments.Timeout, 4*time.Second; got != want {
		t.Errorf("payments timeout = %v, want %v (from defaults)", got, want)
	}
	if got, want := payments.Outlier.ConsecutiveFailures, 7; got != want {
		t.Errorf("payments outlier.consecutiveFailures = %d, want %d (from defaults)", got, want)
	}

	if got, want := orders.Timeout, 2*time.Second; got != want {
		t.Errorf("orders timeout = %v, want %v", got, want)
	}
	if got, want := orders.Retry.MaxAttempts, 5; got != want {
		t.Errorf("orders retry.maxAttempts = %d, want %d (from defaults)", got, want)
	}

	if got, want := m.Defaults.Timeout, 4*time.Second; got != want {
		t.Errorf("defaults.timeout = %v, want %v", got, want)
	}
}

// A service the file does not name routes with the defaults: new deployments
// need no mesh edit.
func TestPolicyForUnnamedService(t *testing.T) {
	m := mustLoadMesh(t, "defaults:\n  timeout: 4s\nservices:\n  - name: orders-svc\n    timeout: 2s\n")

	if got := m.PolicyFor("orders-svc").Timeout; got != 2*time.Second {
		t.Errorf("orders-svc timeout = %v, want 2s", got)
	}
	if got := m.PolicyFor("brand-new-svc").Timeout; got != 4*time.Second {
		t.Errorf("unnamed service timeout = %v, want the 4s default", got)
	}
}

// `perTryTimeout: 0s` means "disabled" and must survive a non-zero default.
func TestExplicitZeroOverridesDefault(t *testing.T) {
	m := mustLoadMesh(t, `
defaults:
  perTryTimeout: 1s
services:
  - name: orders-svc
    perTryTimeout: 0s
`)

	if got := m.Services[0].PerTryTimeout; got != 0 {
		t.Errorf("perTryTimeout = %v, want 0 (explicitly disabled)", got)
	}
}

func TestDurationsAndLevels(t *testing.T) {
	m := mustLoadMesh(t, `
registry:
  leaseTTL: 1m
log:
  level: debug
services:
  - name: orders-svc
    retry:
      minAttemptTime: 0
`)

	if got, want := m.Registry.LeaseTTL, time.Minute; got != want {
		t.Errorf("registry.leaseTTL = %v, want %v", got, want)
	}
	if got, want := m.Log.Level, slog.LevelDebug; got != want {
		t.Errorf("log.level = %v, want %v", got, want)
	}
	// Unquoted 0 is a number in YAML, and still has to read as a duration.
	if got := m.Services[0].Retry.MinAttemptTime; got != 0 {
		t.Errorf("retry.minAttemptTime = %v, want 0", got)
	}
}

func TestMeshRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{{
		name:    "typo in a field name",
		body:    "services:\n  - name: orders-svc\n    timeut: 2s\n",
		wantErr: "field timeut not found",
	}, {
		name:    "typo in a block name",
		body:    "defaluts:\n  timeout: 2s\n",
		wantErr: "field defaluts not found",
	}, {
		// Instances register themselves; listing them is a leftover from the
		// static file, and must say so rather than be silently ignored.
		name:    "instances listed by hand",
		body:    "services:\n  - name: orders-svc\n    instances: [\"10.0.0.7:15000\"]\n",
		wantErr: "field instances not found",
	}, {
		name:    "not yaml at all",
		body:    "services: [oh dear\n",
		wantErr: "yaml",
	}, {
		name:    "empty file",
		body:    "",
		wantErr: "file is empty",
	}, {
		name:    "two documents",
		body:    minimalMesh + "---\n" + minimalMesh,
		wantErr: "single YAML document",
	}, {
		name:    "broken second document",
		body:    minimalMesh + "---\nservices: [oops\n",
		wantErr: "did not find expected",
	}, {
		name:    "duration that is not one",
		body:    "defaults:\n  timeout: soon\n",
		wantErr: `"soon" is not a duration`,
	}, {
		name:    "unknown log level",
		body:    "log:\n  level: chatty\n",
		wantErr: `"chatty" is not one of debug, info, warn, error`,
	}, {
		name:    "lease too short",
		body:    "registry:\n  leaseTTL: 1s\n",
		wantErr: "registry.leaseTTL: must be 3s..5m",
	}, {
		name:    "service with no name",
		body:    "services:\n  - timeout: 2s\n",
		wantErr: "services[0].name: required",
	}, {
		name:    "service name that is not a DNS label",
		body:    "services:\n  - name: Orders_SVC\n",
		wantErr: "must be a DNS label",
	}, {
		name:    "reserved service name",
		body:    "services:\n  - name: _sidecar\n",
		wantErr: "reserved",
	}, {
		name:    "duplicate service name",
		body:    minimalMesh + "  - name: orders-svc\n",
		wantErr: "already used by services[0]",
	}, {
		name:    "maxAttempts out of range",
		body:    "defaults:\n  retry:\n    maxAttempts: 99\n",
		wantErr: "must be 1..10",
	}, {
		name:    "perTryTimeout longer than timeout",
		body:    "defaults:\n  timeout: 1s\n  perTryTimeout: 2s\n",
		wantErr: "must not exceed timeout",
	}, {
		name:    "retry buffer above the hard cap",
		body:    "limits:\n  maxBodyBytes: 1024\ndefaults:\n  retry:\n    maxBodyBytes: 4096\n",
		wantErr: "must not exceed limits.maxBodyBytes",
	}, {
		name:    "backoff max below base",
		body:    "defaults:\n  retry:\n    backoff:\n      base: 1s\n      max: 100ms\n",
		wantErr: "must be at least backoff.base",
	}, {
		name:    "budget ratio above 1",
		body:    "defaults:\n  retry:\n    budget:\n      ratio: 1.5\n",
		wantErr: "must be 0..1",
	}, {
		name:    "budget window not whole seconds",
		body:    "defaults:\n  retry:\n    budget:\n      window: 1500ms\n",
		wantErr: "whole seconds",
	}, {
		name:    "budget window too long",
		body:    "defaults:\n  retry:\n    budget:\n      window: 5m\n",
		wantErr: "must be 1s..60s",
	}, {
		name:    "maxEjection below baseEjection",
		body:    "defaults:\n  outlier:\n    baseEjection: 1m\n    maxEjection: 10s\n",
		wantErr: "must be at least outlier.baseEjection",
	}, {
		name:    "maxEjectionPercent above 100",
		body:    "defaults:\n  outlier:\n    maxEjectionPercent: 150\n",
		wantErr: "must be 0..100",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadMesh(t, tc.body)
			if err == nil {
				t.Fatalf("LoadMesh succeeded, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v,\nwant it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestTrailingDocumentMarker(t *testing.T) {
	for name, tail := range map[string]string{
		"bare marker":        "---\n",
		"marker and comment": "---\n# end\n",
		"end marker":         "---\n...\n",
	} {
		t.Run(name, func(t *testing.T) {
			m := mustLoadMesh(t, minimalMesh+tail)
			if len(m.Services) != 1 {
				t.Errorf("got %d services, want 1", len(m.Services))
			}
		})
	}
}

func TestReportsEveryProblemAtOnce(t *testing.T) {
	_, err := loadMesh(t, `
defaults:
  retry:
    maxAttempts: 99
services:
  - name: BAD_NAME
    outlier:
      maxEjectionPercent: 150
`)
	if err == nil {
		t.Fatal("LoadMesh succeeded, want errors")
	}

	for _, want := range []string{"maxAttempts", "DNS label", "maxEjectionPercent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestMissingFile(t *testing.T) {
	_, err := LoadMesh(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("LoadMesh succeeded on a missing file")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// The committed mesh.yaml is the file a reader copies, so it has to load.
func TestRepoMeshLoads(t *testing.T) {
	m, err := LoadMesh(filepath.Join("..", "..", DefaultMeshPath))
	if err != nil {
		t.Fatalf("load ../../%s: %v", DefaultMeshPath, err)
	}
	if len(m.Services) == 0 {
		t.Error("the example mesh names no services")
	}
}
