package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Source owns the config file: it keeps the config in effect behind an atomic
// pointer and reloads it when the file changes. Readers take one snapshot with
// Current, so a reload never shows them half of one config and half of another.
type Source struct {
	path string
	log  *slog.Logger

	current atomic.Pointer[Config]

	reloading sync.Mutex // serialises reloads, and guards stamp
	stamp     stamp

	mu   sync.Mutex
	subs []func(old, next *Config)

	tick func(time.Duration) (<-chan time.Time, func())
}

type Option func(*Source)

func WithLogger(l *slog.Logger) Option {
	return func(s *Source) { s.log = l }
}

// New loads the file at path; an error means there is no usable config.
func New(path string, opts ...Option) (*Source, error) {
	s := &Source{path: path, log: slog.Default(), tick: newTicker}
	for _, opt := range opts {
		opt(s)
	}

	s.stamp = statFile(path)
	next, err := Load(path)
	if err != nil {
		return nil, err
	}
	s.current.Store(next)
	s.log.Info("config_loaded", "config", path, "services", len(next.Services))
	return s, nil
}

// Current is the config in effect. It is never modified; a reload replaces it.
func (s *Source) Current() *Config {
	return s.current.Load()
}

// OnChange registers fn to run after every successful reload.
func (s *Source) OnChange(fn func(old, next *Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// Reload reads the file now; on error the previous config stays in effect.
func (s *Source) Reload() error {
	s.reloading.Lock()
	defer s.reloading.Unlock()

	s.stamp = statFile(s.path)
	return s.reload()
}

// Watch polls the file every reload.interval until ctx is done. The interval
// is restart-only, so it is read once.
func (s *Source) Watch(ctx context.Context) {
	every := s.Current().Reload.Interval
	if every <= 0 {
		return
	}
	tick, stop := s.tick(every)
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.reloadIfChanged()
		}
	}
}

func (s *Source) reloadIfChanged() {
	s.reloading.Lock()
	defer s.reloading.Unlock()

	// The stamp moves on every change, good or bad, so a broken file is rejected
	// once rather than on every tick until fixed.
	st := statFile(s.path)
	if st.same(s.stamp) {
		return
	}
	s.stamp = st
	s.reload()
}

func (s *Source) reload() error {
	old := s.Current()

	next, err := Load(s.path)
	if err == nil {
		if changed := next.keepStatic(old); len(changed) > 0 {
			// Restoring them can break a cross-field rule (the loop guard checks
			// the inbound address actually in effect), so validate again.
			if err = next.validate(); err != nil {
				err = fmt.Errorf("%s: %w", s.path, err)
			} else {
				s.log.Warn("config_restart_required", "config", s.path, "fields", changed)
			}
		}
	}
	if err != nil {
		s.log.Error("config_rejected", "config", s.path, "error", err, "keeping", "previous")
		return err
	}

	s.current.Store(next)
	s.log.Info("config_loaded", "config", s.path, "services", len(next.Services))

	s.mu.Lock()
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	for _, fn := range subs {
		fn(old, next)
	}
	return nil
}

// keepStatic restores the restart-only fields and returns those the file tried to
// change, so Current never claims a listener the process is not bound to.
func (c *Config) keepStatic(old *Config) []string {
	var changed []string
	keep(&changed, "listeners.inbound", &c.Listeners.Inbound, old.Listeners.Inbound)
	keep(&changed, "listeners.outbound", &c.Listeners.Outbound, old.Listeners.Outbound)
	keep(&changed, "app.address", &c.App.Address, old.App.Address)
	keep(&changed, "limits.maxHeaderBytes", &c.Limits.MaxHeaderBytes, old.Limits.MaxHeaderBytes)
	keep(&changed, "reload.interval", &c.Reload.Interval, old.Reload.Interval)
	return changed
}

func keep[T comparable](changed *[]string, name string, dst *T, old T) {
	if *dst != old {
		*changed = append(*changed, name)
		*dst = old
	}
}

// stamp identifies a version of the file. A missing file is the zero stamp, so
// it disappearing and coming back each count as one change.
type stamp struct {
	mod  time.Time
	size int64
}

func statFile(path string) stamp {
	fi, err := os.Stat(path)
	if err != nil {
		return stamp{}
	}
	return stamp{mod: fi.ModTime(), size: fi.Size()}
}

func (a stamp) same(b stamp) bool {
	return a.mod.Equal(b.mod) && a.size == b.size
}

func newTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}
