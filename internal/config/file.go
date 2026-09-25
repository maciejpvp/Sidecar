package config

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The YAML shapes shared by both files. Every leaf is a pointer so "not set"
// differs from "set to zero": that is what lets a service inherit `defaults`
// field by field while still setting `perTryTimeout: 0s` on purpose.

type filePolicy struct {
	Timeout       *duration   `yaml:"timeout"`
	PerTryTimeout *duration   `yaml:"perTryTimeout"`
	Retry         fileRetry   `yaml:"retry"`
	Outlier       fileOutlier `yaml:"outlier"`
}

type fileRetry struct {
	MaxAttempts    *int        `yaml:"maxAttempts"`
	MaxBodyBytes   *int64      `yaml:"maxBodyBytes"`
	MinAttemptTime *duration   `yaml:"minAttemptTime"`
	Backoff        fileBackoff `yaml:"backoff"`
	Budget         fileBudget  `yaml:"budget"`
}

type fileBackoff struct {
	Base *duration `yaml:"base"`
	Max  *duration `yaml:"max"`
}

type fileBudget struct {
	Ratio        *float64  `yaml:"ratio"`
	MinPerSecond *int      `yaml:"minPerSecond"`
	Window       *duration `yaml:"window"`
}

type fileOutlier struct {
	ConsecutiveFailures *int      `yaml:"consecutiveFailures"`
	BaseEjection        *duration `yaml:"baseEjection"`
	MaxEjection         *duration `yaml:"maxEjection"`
	MaxEjectionPercent  *int      `yaml:"maxEjectionPercent"`
	DecayAfter          *duration `yaml:"decayAfter"`
}

type fileReload struct {
	Interval *duration `yaml:"interval"`
}

type fileShutdown struct {
	DrainTimeout *duration `yaml:"drainTimeout"`
}

type fileLog struct {
	Level *level `yaml:"level"`
}

// duration reads Go's `250ms` / `5s` format, which yaml.v3 cannot decode itself.
type duration time.Duration

func (d *duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("want a duration like 250ms, 5s or 1m")
	}
	// n.Value rather than a decoded string, so unquoted `0` still parses.
	parsed, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("%q is not a duration like 250ms, 5s or 1m", n.Value)
	}
	*d = duration(parsed)
	return nil
}

type level slog.Level

func (l *level) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return errors.New("want one of debug, info, warn, error")
	}
	return l.parse(n.Value)
}

func (l *level) parse(s string) error {
	switch strings.ToLower(s) {
	case "debug":
		*l = level(slog.LevelDebug)
	case "info":
		*l = level(slog.LevelInfo)
	case "warn", "warning":
		*l = level(slog.LevelWarn)
	case "error":
		*l = level(slog.LevelError)
	default:
		return fmt.Errorf("%q is not one of debug, info, warn, error", s)
	}
	return nil
}

func (p *Policy) apply(f filePolicy) {
	setDuration(&p.Timeout, f.Timeout)
	setDuration(&p.PerTryTimeout, f.PerTryTimeout)
	p.Retry.apply(f.Retry)
	p.Outlier.apply(f.Outlier)
}

func (r *Retry) apply(f fileRetry) {
	set(&r.MaxAttempts, f.MaxAttempts)
	set(&r.MaxBodyBytes, f.MaxBodyBytes)
	setDuration(&r.MinAttemptTime, f.MinAttemptTime)
	r.Backoff.apply(f.Backoff)
	r.Budget.apply(f.Budget)
}

func (b *Backoff) apply(f fileBackoff) {
	setDuration(&b.Base, f.Base)
	setDuration(&b.Max, f.Max)
}

func (b *Budget) apply(f fileBudget) {
	set(&b.Ratio, f.Ratio)
	set(&b.MinPerSecond, f.MinPerSecond)
	setDuration(&b.Window, f.Window)
}

func (o *Outlier) apply(f fileOutlier) {
	set(&o.ConsecutiveFailures, f.ConsecutiveFailures)
	setDuration(&o.BaseEjection, f.BaseEjection)
	setDuration(&o.MaxEjection, f.MaxEjection)
	set(&o.MaxEjectionPercent, f.MaxEjectionPercent)
	setDuration(&o.DecayAfter, f.DecayAfter)
}

func set[T any](dst, src *T) {
	if src != nil {
		*dst = *src
	}
}

func setDuration(dst *time.Duration, src *duration) {
	if src != nil {
		*dst = time.Duration(*src)
	}
}
