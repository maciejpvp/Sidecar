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

// identity is the part every sidecar must be given; the rest defaults.
const identity = `
controlPlane:
  address: "controlplane:15100"
service:
  name: orders-svc
  advertise: "10.0.0.7:15000"
`

func loadSidecar(t *testing.T, body string, env map[string]string) (*Sidecar, error) {
	t.Helper()
	path := ""
	if body != "" {
		path = filepath.Join(t.TempDir(), "sidecar.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	return LoadSidecar(path, func(k string) string { return env[k] })
}

func mustLoadSidecar(t *testing.T, body string, env map[string]string) *Sidecar {
	t.Helper()
	c, err := loadSidecar(t, body, env)
	if err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}
	return c
}

func TestSidecarDefaults(t *testing.T) {
	c := mustLoadSidecar(t, identity, nil)

	for _, tc := range []struct{ field, got, want string }{
		{"listeners.inbound", c.Listeners.Inbound, "0.0.0.0:15000"},
		{"listeners.outbound", c.Listeners.Outbound, "127.0.0.1:15001"},
		{"listeners.admin", c.Listeners.Admin, "0.0.0.0:15020"},
		{"app.address", c.App.Address, "127.0.0.1:8080"},
		{"app.healthPath", c.App.HealthPath, "/healthz"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if got, want := c.Log.Level, slog.LevelInfo; got != want {
		t.Errorf("log.level = %v, want %v", got, want)
	}
	if got, want := c.Inbound.DefaultTimeout, 3*time.Second; got != want {
		t.Errorf("inbound.defaultTimeout = %v, want %v", got, want)
	}
}

// In Kubernetes there is no file at all: the Downward API fills the environment.
func TestSidecarFromEnvironmentAlone(t *testing.T) {
	c := mustLoadSidecar(t, "", map[string]string{
		Env.Service:       "orders-svc",
		Env.Advertise:     "10.1.2.3:15000",
		Env.ControlPlane:  "sidecar-controlplane.mesh:15100",
		Env.AppAddress:    "127.0.0.1:9090",
		Env.AppHealthPath: "/ready",
		Env.LogLevel:      "debug",
	})

	if c.Service.Name != "orders-svc" || c.Service.Advertise != "10.1.2.3:15000" {
		t.Errorf("identity = %+v, want orders-svc at 10.1.2.3:15000", c.Service)
	}
	if c.ControlPlane.Address != "sidecar-controlplane.mesh:15100" {
		t.Errorf("controlPlane.address = %q", c.ControlPlane.Address)
	}
	if c.App.Address != "127.0.0.1:9090" || c.App.HealthPath != "/ready" {
		t.Errorf("app = %+v, want 127.0.0.1:9090 /ready", c.App)
	}
	if c.Log.Level != slog.LevelDebug {
		t.Errorf("log.level = %v, want DEBUG", c.Log.Level)
	}
}

func TestEnvironmentWinsOverFile(t *testing.T) {
	c := mustLoadSidecar(t, identity, map[string]string{Env.Advertise: "10.9.9.9:15000", Env.Service: ""})

	if got := c.Service.Advertise; got != "10.9.9.9:15000" {
		t.Errorf("advertise = %q, want the environment's 10.9.9.9:15000", got)
	}
	// Set but empty is "not set": a templated manifest produces "" for unset values.
	if got := c.Service.Name; got != "orders-svc" {
		t.Errorf("name = %q, want the file's orders-svc", got)
	}
}

func TestSidecarRejects(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		env     map[string]string
		wantErr string
	}{{
		name:    "nothing at all",
		wantErr: "service.name: required (or set SIDECAR_SERVICE)",
	}, {
		name:    "no control plane",
		env:     map[string]string{Env.Service: "a", Env.Advertise: "10.0.0.7:15000"},
		wantErr: "controlPlane.address: required",
	}, {
		name:    "no advertise address",
		env:     map[string]string{Env.Service: "a", Env.ControlPlane: "cp:15100"},
		wantErr: "service.advertise: required",
	}, {
		name:    "advertise on every interface",
		body:    identity,
		env:     map[string]string{Env.Advertise: "0.0.0.0:15000"},
		wantErr: "cannot be dialled",
	}, {
		name:    "advertise with a scheme",
		body:    identity,
		env:     map[string]string{Env.Advertise: "http://10.0.0.7:15000"},
		wantErr: "without a scheme",
	}, {
		name:    "advertise is our own outbound listener",
		body:    identity,
		env:     map[string]string{Env.Advertise: "localhost:15001"},
		wantErr: "own outbound listener",
	}, {
		name:    "service name that is not a DNS label",
		body:    identity,
		env:     map[string]string{Env.Service: "Orders_SVC"},
		wantErr: "must be a DNS label",
	}, {
		name:    "bad log level in the environment",
		body:    identity,
		env:     map[string]string{Env.LogLevel: "chatty"},
		wantErr: "SIDECAR_LOG_LEVEL",
	}, {
		name:    "typo in a field name",
		body:    identity + "listners:\n  inbound: \":15000\"\n",
		wantErr: "field listners not found",
	}, {
		// Services and instances now come from the control plane.
		name:    "a leftover static service list",
		body:    identity + "services:\n  - name: orders-svc\n",
		wantErr: "field services not found",
	}, {
		name:    "outbound listener not on loopback",
		body:    identity + "listeners:\n  outbound: \"0.0.0.0:15001\"\n",
		wantErr: "open proxy into the mesh",
	}, {
		name:    "outbound listener as a name",
		body:    identity + "listeners:\n  outbound: \"orders-svc:15001\"\n",
		wantErr: "could resolve anywhere",
	}, {
		name:    "listener without a port",
		body:    identity + "listeners:\n  inbound: \"0.0.0.0\"\n",
		wantErr: "want host:port",
	}, {
		name:    "health path without a slash",
		body:    identity + "app:\n  healthPath: healthz\n",
		wantErr: "must start with /",
	}, {
		name:    "inbound default above max",
		body:    identity + "inbound:\n  defaultTimeout: 1m\n  maxTimeout: 10s\n",
		wantErr: "must not exceed inbound.maxTimeout",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadSidecar(t, tc.body, tc.env)
			if err == nil {
				t.Fatalf("LoadSidecar succeeded, want an error mentioning %q", tc.wantErr)
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
			err := InstanceAddr(tc.addr)
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
		})
	}
}

// The loop guard, across every spelling of "this sidecar's own outbound port".
func TestAdvertiseLoopGuard(t *testing.T) {
	tests := []struct {
		outbound  string
		advertise string
		loop      bool
	}{
		{"127.0.0.1:15001", "127.0.0.1:15001", true},
		{"127.0.0.1:15001", "localhost:15001", true},
		{"127.0.0.1:15001", "LOCALHOST:15001", true},
		{"127.0.0.1:15001", "[::1]:15001", true},
		{"[::1]:15001", "127.0.0.1:15001", true},

		{"127.0.0.1:15001", "127.0.0.1:15000", false}, // the inbound port: the normal case locally
		{"127.0.0.1:15001", "10.0.0.7:15001", false},  // same port, another host
		{"127.0.0.1:15001", "orders-svc:15001", false},
	}

	for _, tc := range tests {
		t.Run(tc.outbound+" vs "+tc.advertise, func(t *testing.T) {
			body := fmt.Sprintf("listeners:\n  outbound: %q\ncontrolPlane:\n  address: \"cp:15100\"\nservice:\n  name: a\n  advertise: %q\n", tc.outbound, tc.advertise)
			_, err := loadSidecar(t, body, nil)

			caught := err != nil && strings.Contains(err.Error(), "own outbound listener")
			if caught != tc.loop {
				t.Errorf("loop guard caught = %v, want %v (err: %v)", caught, tc.loop, err)
			}
			if !tc.loop && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestSnapshotValidation(t *testing.T) {
	ok := NewService("orders-svc", "10.0.0.7:15000", "10.0.0.8:15000")
	empty := NewService("declared-but-down")
	if err := Snapshot([]Service{ok, empty}); err != nil {
		t.Fatalf("Snapshot rejected a valid snapshot: %v", err)
	}

	broken := NewService("orders-svc", "http://10.0.0.7:15000")
	twice := NewService("twice", "10.0.0.7:15000", "10.0.0.7:15000")
	badPolicy := NewService("bad-policy", "10.0.0.7:15000")
	badPolicy.Retry.MaxAttempts = 0

	err := Snapshot([]Service{broken, twice, badPolicy, NewService("twice")})
	if err == nil {
		t.Fatal("Snapshot accepted a broken snapshot")
	}
	for _, want := range []string{"without a scheme", "listed twice", "maxAttempts", "appears twice"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

// The committed sidecar.yaml is the file a reader copies, so it has to load.
func TestRepoSidecarLoads(t *testing.T) {
	if _, err := LoadSidecar(filepath.Join("..", "..", "sidecar.yaml"), nil); err != nil {
		t.Fatalf("load ../../sidecar.yaml: %v", err)
	}
}
