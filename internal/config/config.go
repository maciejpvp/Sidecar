// Package config loads the sidecar's YAML file: parse, apply defaults, validate
// (format: docs/CONFIG.md). Every rule lives here, so whatever Load accepts the
// rest of the program uses without checking again.
package config

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const DefaultPath = "sidecar.yaml"

// Config is a whole configuration with nothing left unset.
type Config struct {
	Listeners Listeners
	App       App
	Inbound   Inbound
	Limits    Limits
	Reload    Reload
	Shutdown  Shutdown
	Log       Log
	// Validated on its own, so a file with no services still fails on a broken default.
	Defaults Policy
	Services []Service // in file order
}

type Listeners struct {
	Inbound  string
	Outbound string // loopback only, or the sidecar is an open proxy into the mesh
}

type App struct {
	Address string
}

type Inbound struct {
	DefaultTimeout time.Duration
	MaxTimeout     time.Duration
}

type Limits struct {
	MaxBodyBytes   int64
	MaxHeaderBytes int
}

type Reload struct {
	Interval time.Duration // 0 disables hot reload
}

type Shutdown struct {
	DrainTimeout time.Duration
}

type Log struct {
	Level slog.Level
}

type Service struct {
	Name      string
	Instances []string // host:port
	Policy
}

// NewService is a service with the default policy. Zero never means "default"
// outside this package, so services built in code must start here, not Service{}.
func NewService(name string, instances ...string) Service {
	s := Service{Name: name, Instances: instances}
	s.Policy.defaults()
	return s
}

type Policy struct {
	Timeout       time.Duration
	PerTryTimeout time.Duration // 0 = no per-attempt limit
	Retry         Retry
	Outlier       Outlier
}

type Retry struct {
	MaxAttempts    int   // total attempts including the first; 1 = no retries
	MaxBodyBytes   int64 // larger bodies are streamed and never retried
	MinAttemptTime time.Duration
	Backoff        Backoff
	Budget         Budget
}

type Backoff struct {
	Base time.Duration
	Max  time.Duration
}

type Budget struct {
	Ratio        float64
	MinPerSecond int
	Window       time.Duration
}

type Outlier struct {
	ConsecutiveFailures int
	BaseEjection        time.Duration
	MaxEjection         time.Duration
	MaxEjectionPercent  int
	DecayAfter          time.Duration
}

// Load reads, resolves and validates the file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := cfg.read(f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) read(r io.Reader) error {
	dec := yaml.NewDecoder(r)
	// A typo must fail loudly, not quietly fall back to a default.
	dec.KnownFields(true)

	var raw file
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("file is empty")
		}
		return err
	}
	// A second document is an error; a trailing `---` with nothing after it is not.
	for {
		var extra yaml.Node
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !blank(&extra) {
			return errors.New("want a single YAML document")
		}
	}

	c.defaults()
	c.apply(&raw)
	return c.validate()
}

// blank reports whether a document is empty, which is what a bare `---` decodes to.
func blank(n *yaml.Node) bool {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}

// defaults fills c with the Default column of CONFIG.md.
func (c *Config) defaults() {
	*c = Config{
		Listeners: Listeners{Inbound: "0.0.0.0:15000", Outbound: "127.0.0.1:15001"},
		App:       App{Address: "127.0.0.1:8080"},
		Inbound:   Inbound{DefaultTimeout: 3 * time.Second, MaxTimeout: 30 * time.Second},
		Limits:    Limits{MaxBodyBytes: 10 << 20, MaxHeaderBytes: 64 << 10},
		Reload:    Reload{Interval: 2 * time.Second},
		Shutdown:  Shutdown{DrainTimeout: 10 * time.Second},
		Log:       Log{Level: slog.LevelInfo},
	}
	c.Defaults.defaults()
}

func (p *Policy) defaults() {
	*p = Policy{
		Timeout: 5 * time.Second,
		Retry: Retry{
			MaxAttempts:    3,
			MaxBodyBytes:   1 << 20,
			MinAttemptTime: 20 * time.Millisecond,
			Backoff:        Backoff{Base: 25 * time.Millisecond, Max: 250 * time.Millisecond},
			Budget:         Budget{Ratio: 0.2, MinPerSecond: 3, Window: 10 * time.Second},
		},
		Outlier: Outlier{
			ConsecutiveFailures: 5,
			BaseEjection:        30 * time.Second,
			MaxEjection:         5 * time.Minute,
			MaxEjectionPercent:  50,
			DecayAfter:          5 * time.Minute,
		},
	}
}
