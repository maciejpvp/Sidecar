package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"
)

const DefaultMeshPath = "mesh.yaml"

// Mesh is the control plane's file with nothing left unset: the policy every
// sidecar applies, per service. It lists no instances — those register
// themselves — so a service that is not named here still routes, with Defaults.
type Mesh struct {
	Registry Registry
	Limits   MeshLimits
	Reload   Reload
	Log      Log
	// Validated on its own, so a file with no services still fails on a broken default.
	Defaults Policy
	Services []ServicePolicy // in file order
}

// ServicePolicy is a service the mesh file names, with its resolved policy.
type ServicePolicy struct {
	Name string
	Policy
}

type Registry struct {
	// LeaseTTL is how long a registration lives without a heartbeat. Sidecars
	// are told to heartbeat every LeaseTTL/3, so one lost heartbeat is not an
	// expiry, and a control plane that restarts waits one LeaseTTL before
	// serving snapshots, which is long enough for every live instance to have
	// registered again.
	LeaseTTL time.Duration
}

// Heartbeat is how often a sidecar should renew its lease.
func (r Registry) Heartbeat() time.Duration { return r.LeaseTTL / 3 }

type MeshLimits struct {
	MaxBodyBytes int64
}

type Reload struct {
	Interval time.Duration // 0 disables hot reload
}

type Log struct {
	Level slog.Level
}

// PolicyFor is the policy of the named service: its own if the file names it,
// otherwise the defaults. Unnamed services are the normal case, not an error —
// a new deployment routes the moment its first pod registers.
func (m *Mesh) PolicyFor(name string) Policy {
	for _, s := range m.Services {
		if s.Name == name {
			return s.Policy
		}
	}
	return m.Defaults
}

type meshFile struct {
	Registry fileRegistry  `yaml:"registry"`
	Limits   fileMeshLimit `yaml:"limits"`
	Defaults filePolicy    `yaml:"defaults"`
	Services []fileService `yaml:"services"`
	Reload   fileReload    `yaml:"reload"`
	Log      fileLog       `yaml:"log"`
}

type fileRegistry struct {
	LeaseTTL *duration `yaml:"leaseTTL"`
}

type fileMeshLimit struct {
	MaxBodyBytes *int64 `yaml:"maxBodyBytes"`
}

type fileService struct {
	Name       *string `yaml:"name"`
	filePolicy `yaml:",inline"`
}

// LoadMesh reads, resolves and validates the mesh file at path.
func LoadMesh(path string) (*Mesh, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var m Mesh
	if err := m.read(f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// DefaultMesh is the mesh a control plane runs with when it has no file.
func DefaultMesh() *Mesh {
	var m Mesh
	m.defaults()
	return &m
}

func (m *Mesh) read(r io.Reader) error {
	var raw meshFile
	if err := decode(r, &raw); err != nil {
		return err
	}
	m.defaults()
	m.apply(&raw)
	return m.validate()
}

func (m *Mesh) defaults() {
	*m = Mesh{
		Registry: Registry{LeaseTTL: 15 * time.Second},
		Limits:   MeshLimits{MaxBodyBytes: 10 << 20},
		Reload:   Reload{Interval: 2 * time.Second},
		Log:      Log{Level: slog.LevelInfo},
		Defaults: DefaultPolicy(),
	}
}

func (m *Mesh) apply(f *meshFile) {
	setDuration(&m.Registry.LeaseTTL, f.Registry.LeaseTTL)
	set(&m.Limits.MaxBodyBytes, f.Limits.MaxBodyBytes)
	setDuration(&m.Reload.Interval, f.Reload.Interval)
	if f.Log.Level != nil {
		m.Log.Level = slog.Level(*f.Log.Level)
	}

	m.Defaults.apply(f.Defaults)

	m.Services = make([]ServicePolicy, 0, len(f.Services))
	for _, fs := range f.Services {
		svc := ServicePolicy{Policy: m.Defaults}
		set(&svc.Name, fs.Name)
		svc.Policy.apply(fs.filePolicy)
		m.Services = append(m.Services, svc)
	}
}

func (m *Mesh) validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	// Below 3s the heartbeat interval drops under one second, which is chatty
	// for no gain; above 5m a dead pod keeps receiving traffic for minutes.
	if ttl := m.Registry.LeaseTTL; ttl < 3*time.Second || ttl > 5*time.Minute {
		add("registry.leaseTTL: must be 3s..5m, got %v", ttl)
	}
	if m.Limits.MaxBodyBytes <= 0 {
		add("limits.maxBodyBytes: must be positive, got %d", m.Limits.MaxBodyBytes)
	}
	if m.Reload.Interval < 0 {
		add("reload.interval: must not be negative, got %v (0 disables hot reload)", m.Reload.Interval)
	}

	// Checked even when every service overrides it: a broken default is still wrong.
	errs = append(errs, m.Defaults.validate("defaults", m.Limits.MaxBodyBytes)...)

	seen := make(map[string]int, len(m.Services))
	for i, svc := range m.Services {
		at := fmt.Sprintf("services[%d]", i)
		if svc.Name != "" {
			at = fmt.Sprintf("services[%d] (%s)", i, svc.Name)
		}
		if err := ServiceName(svc.Name); err != nil {
			add("%s.name: %w", at, err)
		}
		if first, dup := seen[svc.Name]; dup && svc.Name != "" {
			add("%s.name: %q is already used by services[%d]", at, svc.Name, first)
		} else if svc.Name != "" {
			seen[svc.Name] = i
		}
		errs = append(errs, svc.Policy.validate(at, m.Limits.MaxBodyBytes)...)
	}

	return errors.Join(errs...)
}

// keepStatic restores the restart-only fields and returns those the file tried
// to change, so Current never claims a setting the process is not running with.
func (m *Mesh) keepStatic(old *Mesh) []string {
	var changed []string
	keep(&changed, "reload.interval", &m.Reload.Interval, old.Reload.Interval)
	return changed
}

func keep[T comparable](changed *[]string, name string, dst *T, old T) {
	if *dst != old {
		*changed = append(*changed, name)
		*dst = old
	}
}
