package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Sidecar is one sidecar's startup configuration with nothing left unset. It
// is read once: everything that changes at runtime — routes, instances,
// per-service policy — comes from the control plane instead.
type Sidecar struct {
	Listeners    Listeners
	App          App
	ControlPlane ControlPlane
	Service      Identity
	Inbound      Inbound
	Limits       Limits
	Shutdown     Shutdown
	Log          Log
}

type Listeners struct {
	Inbound  string
	Outbound string // loopback only, or the sidecar is an open proxy into the mesh
	Admin    string // /healthz, /readyz, /snapshot — what the kubelet probes
}

type App struct {
	Address string
	// HealthPath is probed on Address before this instance registers, and
	// while it stays registered. Empty means "healthy as soon as the sidecar
	// is up", which is only right for apps with nothing to warm up.
	HealthPath string
}

type ControlPlane struct {
	Address string // host:port
}

// Identity is what this sidecar registers as.
type Identity struct {
	Name string
	// Advertise is where other sidecars reach this instance: host:port, a
	// routable address, never 0.0.0.0. In Kubernetes, $(POD_IP):15000.
	Advertise string
}

type Inbound struct {
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

type Limits struct {
	MaxHeaderBytes int
}

type Shutdown struct {
	DrainTimeout time.Duration
}

// Env names the environment variables that override sidecar.yaml. They are
// the per-pod values, so a Kubernetes manifest can set them from the Downward
// API and no pod needs a file of its own.
var Env = struct {
	Service, Advertise, ControlPlane, AppAddress, AppHealthPath, LogLevel string
}{
	Service:       "SIDECAR_SERVICE",
	Advertise:     "SIDECAR_ADVERTISE",
	ControlPlane:  "SIDECAR_CONTROL_PLANE",
	AppAddress:    "SIDECAR_APP_ADDRESS",
	AppHealthPath: "SIDECAR_APP_HEALTH_PATH",
	LogLevel:      "SIDECAR_LOG_LEVEL",
}

type sidecarFile struct {
	Listeners    fileListeners    `yaml:"listeners"`
	App          fileApp          `yaml:"app"`
	ControlPlane fileControlPlane `yaml:"controlPlane"`
	Service      fileIdentity     `yaml:"service"`
	Inbound      fileInbound      `yaml:"inbound"`
	Limits       fileLimits       `yaml:"limits"`
	Shutdown     fileShutdown     `yaml:"shutdown"`
	Log          fileLog          `yaml:"log"`
}

type fileListeners struct {
	Inbound  *string `yaml:"inbound"`
	Outbound *string `yaml:"outbound"`
	Admin    *string `yaml:"admin"`
}

type fileApp struct {
	Address    *string `yaml:"address"`
	HealthPath *string `yaml:"healthPath"`
}

type fileControlPlane struct {
	Address *string `yaml:"address"`
}

type fileIdentity struct {
	Name      *string `yaml:"name"`
	Advertise *string `yaml:"advertise"`
}

type fileInbound struct {
	DefaultTimeout *duration `yaml:"defaultTimeout"`
	MaxTimeout     *duration `yaml:"maxTimeout"`
}

type fileLimits struct {
	MaxHeaderBytes *int `yaml:"maxHeaderBytes"`
}

// LoadSidecar builds the startup config: defaults, then the file at path if
// path is not empty, then the environment. getenv is os.Getenv outside tests.
func LoadSidecar(path string, getenv func(string) string) (*Sidecar, error) {
	var raw sidecarFile
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := decode(f, &raw); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	var c Sidecar
	c.defaults()
	c.apply(&raw)
	if err := c.applyEnv(getenv); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		if path != "" {
			return nil, fmt.Errorf("%s (and environment): %w", path, err)
		}
		return nil, err
	}
	return &c, nil
}

func (c *Sidecar) defaults() {
	*c = Sidecar{
		Listeners: Listeners{Inbound: "0.0.0.0:15000", Outbound: "127.0.0.1:15001", Admin: "0.0.0.0:15020"},
		App:       App{Address: "127.0.0.1:8080", HealthPath: "/healthz"},
		Inbound:   Inbound{DefaultTimeout: 3 * time.Second, MaxTimeout: 30 * time.Second},
		Limits:    Limits{MaxHeaderBytes: 64 << 10},
		Shutdown:  Shutdown{DrainTimeout: 10 * time.Second},
		Log:       Log{Level: slog.LevelInfo},
	}
}

func (c *Sidecar) apply(f *sidecarFile) {
	set(&c.Listeners.Inbound, f.Listeners.Inbound)
	set(&c.Listeners.Outbound, f.Listeners.Outbound)
	set(&c.Listeners.Admin, f.Listeners.Admin)
	set(&c.App.Address, f.App.Address)
	set(&c.App.HealthPath, f.App.HealthPath)
	set(&c.ControlPlane.Address, f.ControlPlane.Address)
	set(&c.Service.Name, f.Service.Name)
	set(&c.Service.Advertise, f.Service.Advertise)
	setDuration(&c.Inbound.DefaultTimeout, f.Inbound.DefaultTimeout)
	setDuration(&c.Inbound.MaxTimeout, f.Inbound.MaxTimeout)
	set(&c.Limits.MaxHeaderBytes, f.Limits.MaxHeaderBytes)
	setDuration(&c.Shutdown.DrainTimeout, f.Shutdown.DrainTimeout)
	if f.Log.Level != nil {
		c.Log.Level = slog.Level(*f.Log.Level)
	}
}

// applyEnv lets a set, non-empty variable win over the file. An empty value is
// "not set", because a manifest that templates an unset value produces "".
func (c *Sidecar) applyEnv(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	for name, dst := range map[string]*string{
		Env.Service:       &c.Service.Name,
		Env.Advertise:     &c.Service.Advertise,
		Env.ControlPlane:  &c.ControlPlane.Address,
		Env.AppAddress:    &c.App.Address,
		Env.AppHealthPath: &c.App.HealthPath,
	} {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}
	if v := getenv(Env.LogLevel); v != "" {
		var l level
		if err := l.parse(v); err != nil {
			return fmt.Errorf("%s: %w", Env.LogLevel, err)
		}
		c.Log.Level = slog.Level(l)
	}
	return nil
}

func (c *Sidecar) validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if err := listenAddr(c.Listeners.Inbound); err != nil {
		add("listeners.inbound: %w", err)
	}
	if err := listenAddr(c.Listeners.Outbound); err != nil {
		add("listeners.outbound: %w", err)
	} else if err := loopbackOnly(c.Listeners.Outbound); err != nil {
		add("listeners.outbound: %w", err)
	}
	if err := listenAddr(c.Listeners.Admin); err != nil {
		add("listeners.admin: %w", err)
	}
	if err := listenAddr(c.App.Address); err != nil {
		add("app.address: %w", err)
	}
	if p := c.App.HealthPath; p != "" && !strings.HasPrefix(p, "/") {
		add("app.healthPath: must start with /, got %q (empty disables the check)", p)
	}

	switch {
	case c.ControlPlane.Address == "":
		add("controlPlane.address: required (or set %s)", Env.ControlPlane)
	default:
		if err := InstanceAddr(c.ControlPlane.Address); err != nil {
			add("controlPlane.address: %q: %w", c.ControlPlane.Address, err)
		}
	}

	switch {
	case c.Service.Name == "":
		add("service.name: required (or set %s)", Env.Service)
	default:
		if err := ServiceName(c.Service.Name); err != nil {
			add("service.name: %w", err)
		}
	}
	switch a := c.Service.Advertise; {
	case a == "":
		add("service.advertise: required (or set %s), e.g. $(POD_IP):15000", Env.Advertise)
	default:
		if err := advertiseAddr(a); err != nil {
			add("service.advertise: %q: %w", a, err)
		} else if a == c.Listeners.Outbound || sameLoopbackPort(a, c.Listeners.Outbound) {
			// Every caller of this service would be sent into our own outbound
			// listener, which would pick this instance again: a loop.
			add("service.advertise: %q is this sidecar's own outbound listener, which would route every call back into itself", a)
		}
	}

	if c.Inbound.DefaultTimeout <= 0 {
		add("inbound.defaultTimeout: must be positive, got %v", c.Inbound.DefaultTimeout)
	}
	if c.Inbound.MaxTimeout <= 0 {
		add("inbound.maxTimeout: must be positive, got %v", c.Inbound.MaxTimeout)
	}
	if c.Inbound.DefaultTimeout > c.Inbound.MaxTimeout {
		add("inbound.defaultTimeout (%v) must not exceed inbound.maxTimeout (%v), or every request that omits a deadline is clamped below the default",
			c.Inbound.DefaultTimeout, c.Inbound.MaxTimeout)
	}
	if c.Limits.MaxHeaderBytes <= 0 {
		add("limits.maxHeaderBytes: must be positive, got %d", c.Limits.MaxHeaderBytes)
	}
	if c.Shutdown.DrainTimeout <= 0 {
		add("shutdown.drainTimeout: must be positive, got %v", c.Shutdown.DrainTimeout)
	}

	return errors.Join(errs...)
}
