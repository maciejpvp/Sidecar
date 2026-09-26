// Package config holds both configuration files and every rule about them:
// parse, apply defaults, validate (format: docs/CONFIG.md). Whatever this
// package accepts, the rest of the program uses without checking again.
//
// There are two files, owned by two processes:
//
//   - Sidecar (sidecar.yaml, optional): how one sidecar runs — listeners, the
//     local app, where the control plane is, which service this pod is. Read
//     once at startup; in Kubernetes it usually comes from environment
//     variables alone.
//   - Mesh (mesh.yaml): what every sidecar should do — per-service timeouts,
//     retries and outlier settings. Read by the control plane, hot-reloaded,
//     and delivered to sidecars inside each snapshot together with the
//     instances that registered themselves.
//
// Instances are in neither file: they come from registration (DESIGN §13).
package config

import (
	"errors"
	"io"
	"time"

	"gopkg.in/yaml.v3"
)

// Service is one resolved route: a name, the instances currently registered
// for it, and the policy the mesh file gives it. It is what a control-plane
// snapshot carries and what routing builds a table from.
type Service struct {
	Name      string   `json:"name"`
	Instances []string `json:"instances"` // host:port
	Policy    `json:"policy"`
}

// NewService is a service with the default policy. Zero never means "default"
// outside this package, so services built in code must start here, not Service{}.
func NewService(name string, instances ...string) Service {
	return Service{Name: name, Instances: instances, Policy: DefaultPolicy()}
}

// Policy is everything about calling a service except where it is. Durations
// travel as nanoseconds on the control-plane wire.
type Policy struct {
	Timeout       time.Duration `json:"timeout"`
	PerTryTimeout time.Duration `json:"perTryTimeout"` // 0 = no per-attempt limit
	Retry         Retry         `json:"retry"`
	Outlier       Outlier       `json:"outlier"`
}

type Retry struct {
	MaxAttempts    int           `json:"maxAttempts"`  // total attempts including the first; 1 = no retries
	MaxBodyBytes   int64         `json:"maxBodyBytes"` // larger bodies are streamed and never retried
	MinAttemptTime time.Duration `json:"minAttemptTime"`
	Backoff        Backoff       `json:"backoff"`
	Budget         Budget        `json:"budget"`
}

type Backoff struct {
	Base time.Duration `json:"base"`
	Max  time.Duration `json:"max"`
}

type Budget struct {
	Ratio        float64       `json:"ratio"`
	MinPerSecond int           `json:"minPerSecond"`
	Window       time.Duration `json:"window"`
}

type Outlier struct {
	ConsecutiveFailures int           `json:"consecutiveFailures"`
	BaseEjection        time.Duration `json:"baseEjection"`
	MaxEjection         time.Duration `json:"maxEjection"`
	MaxEjectionPercent  int           `json:"maxEjectionPercent"`
	DecayAfter          time.Duration `json:"decayAfter"`
}

// DefaultPolicy is the Default column of CONFIG.md for per-service knobs.
func DefaultPolicy() Policy {
	return Policy{
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

// decode reads exactly one YAML document from r into dst.
func decode(r io.Reader, dst any) error {
	dec := yaml.NewDecoder(r)
	// A typo must fail loudly, not quietly fall back to a default.
	dec.KnownFields(true)

	if err := dec.Decode(dst); err != nil {
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
			return nil
		}
		if err != nil {
			return err
		}
		if !blank(&extra) {
			return errors.New("want a single YAML document")
		}
	}
}

// blank reports whether a document is empty, which is what a bare `---` decodes to.
func blank(n *yaml.Node) bool {
	if n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		n = n.Content[0]
	}
	return n.Kind == yaml.ScalarNode && n.Tag == "!!null"
}
