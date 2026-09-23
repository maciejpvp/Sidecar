package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sidecar.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Load(path)
}

func mustLoad(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := load(t, body)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

const minimal = `
services:
  - name: orders-svc
    instances: ["10.0.0.7:15000"]
`

func TestMinimalFileGetsDefaults(t *testing.T) {
	cfg := mustLoad(t, minimal)

	if got, want := cfg.Listeners.Outbound, "127.0.0.1:15001"; got != want {
		t.Errorf("listeners.outbound = %q, want %q", got, want)
	}
	if got, want := cfg.Listeners.Inbound, "0.0.0.0:15000"; got != want {
		t.Errorf("listeners.inbound = %q, want %q", got, want)
	}
	if got, want := cfg.Log.Level, slog.LevelInfo; got != want {
		t.Errorf("log.level = %v, want %v", got, want)
	}
	if len(cfg.Services) != 1 {
		t.Fatalf("got %d services, want 1", len(cfg.Services))
	}

	svc := cfg.Services[0]
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
	cfg := mustLoad(t, `
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
    instances: ["10.0.1.3:15000"]

  - name: orders-svc
    timeout: 2s
    instances: ["10.0.0.7:15000"]
`)

	payments, orders := cfg.Services[0], cfg.Services[1]

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

	if got, want := cfg.Defaults.Timeout, 4*time.Second; got != want {
		t.Errorf("defaults.timeout = %v, want %v", got, want)
	}
}

// `perTryTimeout: 0s` means "disabled" and must survive a non-zero default.
func TestExplicitZeroOverridesDefault(t *testing.T) {
	cfg := mustLoad(t, `
defaults:
  perTryTimeout: 1s
services:
  - name: orders-svc
    perTryTimeout: 0s
    instances: ["10.0.0.7:15000"]
`)

	if got := cfg.Services[0].PerTryTimeout; got != 0 {
		t.Errorf("perTryTimeout = %v, want 0 (explicitly disabled)", got)
	}
}

func TestDurationsAndLevels(t *testing.T) {
	cfg := mustLoad(t, `
inbound:
  defaultTimeout: 1500ms
  maxTimeout: 1m
log:
  level: debug
services:
  - name: orders-svc
    instances: ["10.0.0.7:15000"]
    retry:
      minAttemptTime: 0
`)

	if got, want := cfg.Inbound.DefaultTimeout, 1500*time.Millisecond; got != want {
		t.Errorf("inbound.defaultTimeout = %v, want %v", got, want)
	}
	if got, want := cfg.Inbound.MaxTimeout, time.Minute; got != want {
		t.Errorf("inbound.maxTimeout = %v, want %v", got, want)
	}
	if got, want := cfg.Log.Level, slog.LevelDebug; got != want {
		t.Errorf("log.level = %v, want %v", got, want)
	}
	// Unquoted 0 is a number in YAML, and still has to read as a duration.
	if got := cfg.Services[0].Retry.MinAttemptTime; got != 0 {
		t.Errorf("retry.minAttemptTime = %v, want 0", got)
	}
}

func TestRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{{
		name:    "typo in a field name",
		body:    "services:\n  - name: orders-svc\n    instances: [\"10.0.0.7:15000\"]\n    timeut: 2s\n",
		wantErr: "field timeut not found",
	}, {
		name:    "typo in a block name",
		body:    "defaluts:\n  timeout: 2s\n",
		wantErr: "field defaluts not found",
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
		body:    minimal + "---\n" + minimal,
		wantErr: "single YAML document",
	}, {
		name:    "broken second document",
		body:    minimal + "---\nservices: [oops\n",
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
		name:    "service with no name",
		body:    "services:\n  - instances: [\"10.0.0.7:15000\"]\n",
		wantErr: "services[0].name: required",
	}, {
		name:    "service name that is not a DNS label",
		body:    "services:\n  - name: Orders_SVC\n    instances: [\"10.0.0.7:15000\"]\n",
		wantErr: "must be a DNS label",
	}, {
		name:    "reserved service name",
		body:    "services:\n  - name: _sidecar\n    instances: [\"10.0.0.7:15000\"]\n",
		wantErr: "reserved",
	}, {
		name:    "duplicate service name",
		body:    minimal + "  - name: orders-svc\n    instances: [\"10.0.0.8:15000\"]\n",
		wantErr: "already used by services[0]",
	}, {
		name:    "instance listed twice",
		body:    "services:\n  - name: orders-svc\n    instances: [\"10.0.0.7:15000\", \"10.0.0.7:15000\"]\n",
		wantErr: "listed twice",
	}, {
		name:    "service with no instances",
		body:    "services:\n  - name: orders-svc\n",
		wantErr: "instances: required",
	}, {
		name:    "outbound listener not on loopback",
		body:    "listeners:\n  outbound: \"0.0.0.0:15001\"\n",
		wantErr: "open proxy into the mesh",
	}, {
		name:    "outbound listener as a name",
		body:    "listeners:\n  outbound: \"orders-svc:15001\"\n",
		wantErr: "could resolve anywhere",
	}, {
		name:    "listener without a port",
		body:    "listeners:\n  inbound: \"0.0.0.0\"\n",
		wantErr: "want host:port",
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
	}, {
		name:    "inbound default above max",
		body:    "inbound:\n  defaultTimeout: 1m\n  maxTimeout: 10s\n",
		wantErr: "must not exceed inbound.maxTimeout",
	}, {
		name:    "instance is this sidecar's inbound address",
		body:    "listeners:\n  inbound: \"0.0.0.0:15000\"\nservices:\n  - name: orders-svc\n    instances: [\"127.0.0.1:15000\"]\n",
		wantErr: "own inbound address",
	}, {
		name:    "instance is this sidecar's inbound address verbatim",
		body:    "listeners:\n  inbound: \"127.0.0.1:15000\"\nservices:\n  - name: orders-svc\n    instances: [\"127.0.0.1:15000\"]\n",
		wantErr: "own inbound address",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, tc.body)
			if err == nil {
				t.Fatalf("Load succeeded, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v,\nwant it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestInstanceAddressForm(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr string // substring; empty means the address is accepted
	}{
		{name: "ipv4", addr: "10.0.0.7:15000"},
		{name: "loopback", addr: "127.0.0.1:8080"},
		{name: "dns single label", addr: "orders-svc:15000"},
		{name: "dns dotted", addr: "orders-svc.internal:15000"},
		{name: "dns fully qualified", addr: "orders-svc.internal.:15000"},
		{name: "dns mixed case", addr: "ORDERS-SVC.Internal:15000"},
		{name: "dns punycode", addr: "xn--caf-dma.example:15000"},
		{name: "ipv6 loopback", addr: "[::1]:15010"},
		{name: "ipv6", addr: "[2001:db8::7]:15000"},
		{name: "ipv6 zone", addr: "[fe80::1%eth0]:15000"},

		{name: "ipv6 unbracketed", addr: "2001:db8::7:15000", wantErr: "in brackets"},
		{name: "ipv6 loopback unbracketed", addr: "::1:15000", wantErr: "in brackets"},

		{name: "scheme", addr: "http://10.0.0.7:15000", wantErr: "without a scheme"},
		{name: "scheme and path", addr: "https://www.example.com/", wantErr: "without a scheme"},
		{name: "trailing path", addr: "10.0.0.7:15000/v1", wantErr: "without a scheme"},

		{name: "no port", addr: "10.0.0.7", wantErr: "want host:port"},
		{name: "dns name with no port", addr: "orders-svc.internal", wantErr: "want host:port"},
		{name: "no host", addr: ":15000", wantErr: "host is empty"},
		{name: "port zero", addr: "10.0.0.7:0", wantErr: "not 1..65535"},
		{name: "port too high", addr: "10.0.0.7:70000", wantErr: "not 1..65535"},
		{name: "named port", addr: "10.0.0.7:http", wantErr: "not 1..65535"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := load(t, fmt.Sprintf("services:\n  - name: svc\n    instances: [%q]\n", tc.addr))

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("instance %q rejected: %v", tc.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("instance %q accepted, want an error mentioning %q", tc.addr, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "services[0]") || !strings.Contains(err.Error(), tc.addr) {
				t.Errorf("error = %v, want it to name services[0] and %q", err, tc.addr)
			}
		})
	}
}

// The same address under two services is two upstreams, not a duplicate.
func TestSameInstanceInTwoServices(t *testing.T) {
	mustLoad(t, `
services:
  - name: a
    instances: ["10.0.0.7:15000"]
  - name: b
    instances: ["10.0.0.7:15000"]
`)
}

func TestTrailingDocumentMarker(t *testing.T) {
	for name, tail := range map[string]string{
		"bare marker":        "---\n",
		"marker and comment": "---\n# end\n",
		"end marker":         "---\n...\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := mustLoad(t, minimal+tail)
			if len(cfg.Services) != 1 {
				t.Errorf("got %d services, want 1", len(cfg.Services))
			}
		})
	}
}

// CONFIG.md §Validation 7, across every spelling of "this sidecar's own port".
func TestLoopGuard(t *testing.T) {
	tests := []struct {
		inbound  string
		instance string
		loop     bool
	}{
		{"0.0.0.0:15000", "127.0.0.1:15000", true},
		{"0.0.0.0:15000", "localhost:15000", true},
		{"0.0.0.0:15000", "LOCALHOST:15000", true},
		{"0.0.0.0:15000", "localhost.:15000", true},
		{"0.0.0.0:15000", "[::1]:15000", true},
		{":15000", "127.0.0.1:15000", true},
		{"[::]:15000", "localhost:15000", true},
		{"127.0.0.1:15000", "127.0.0.1:15000", true},
		{"127.0.0.1:15000", "localhost:15000", true},
		{"localhost:15000", "127.0.0.1:15000", true},

		{"0.0.0.0:15000", "localhost:15001", false},  // another port on this host
		{"0.0.0.0:15000", "10.0.0.7:15000", false},   // same port, another host
		{"10.0.0.5:15000", "127.0.0.1:15000", false}, // listener not on loopback
		{"0.0.0.0:15000", "orders-svc:15000", false}, // a name we cannot resolve here
	}

	for _, tc := range tests {
		t.Run(tc.inbound+" vs "+tc.instance, func(t *testing.T) {
			_, err := load(t, fmt.Sprintf("listeners:\n  inbound: %q\nservices:\n  - name: svc\n    instances: [%q]\n", tc.inbound, tc.instance))

			caught := err != nil && strings.Contains(err.Error(), "own inbound address")
			if caught != tc.loop {
				t.Errorf("loop guard caught = %v, want %v (err: %v)", caught, tc.loop, err)
			}
			if !tc.loop && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestReportsEveryProblemAtOnce(t *testing.T) {
	_, err := load(t, `
defaults:
  retry:
    maxAttempts: 99
services:
  - name: BAD_NAME
    instances: []
`)
	if err == nil {
		t.Fatal("Load succeeded, want errors")
	}

	for _, want := range []string{"maxAttempts", "DNS label", "instances: required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("Load succeeded on a missing file")
	}
	if !strings.Contains(err.Error(), "nope.yaml") {
		t.Errorf("error = %v, want it to name the file", err)
	}
}

// The committed sidecar.yaml is the file a reader copies, so it has to load.
func TestRepoConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", DefaultPath))
	if err != nil {
		t.Fatalf("load ../../%s: %v", DefaultPath, err)
	}
	if len(cfg.Services) == 0 {
		t.Error("the example config routes no services")
	}
}
