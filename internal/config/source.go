package config

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Source owns the mesh file: it keeps the mesh config in effect behind an
// atomic pointer and reloads it when the file changes. Readers take one
// snapshot with Current, so a reload never shows them half of one config and
// half of another.
//
// In Kubernetes the file is a mounted ConfigMap. The kubelet updates it by
// swapping a symlink, which os.Stat follows, so the poll sees the new file.
type Source struct {
	path string
	log  *slog.Logger

	current atomic.Pointer[Mesh]

	reloading sync.Mutex // serialises reloads, and guards stamp
	stamp     stamp

	mu   sync.Mutex
	subs []func(old, next *Mesh)

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
	next, err := LoadMesh(path)
	if err != nil {
		return nil, err
	}
	s.current.Store(next)
	s.log.Info("config_loaded", "config", path, "services", len(next.Services))
	return s, nil
}

// Static is a Source with no file behind it: m never changes, and Watch and
// Reload do nothing. For a control plane run without a mesh file, and tests.
func Static(m *Mesh) *Source {
	s := &Source{log: slog.Default(), tick: newTicker}
	s.current.Store(m)
	return s
}

// Current is the config in effect. It is never modified; a reload replaces it.
func (s *Source) Current() *Mesh {
	return s.current.Load()
}

// OnChange registers fn to run after every successful reload.
func (s *Source) OnChange(fn func(old, next *Mesh)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// Reload reads the file now; on error the previous config stays in effect.
func (s *Source) Reload() error {
	if s.path == "" {
		return nil
	}
	s.reloading.Lock()
	defer s.reloading.Unlock()

	s.stamp = statFile(s.path)
	return s.reload()
}

// Watch polls the file every reload.interval until ctx is done. The interval
// is restart-only, so it is read once.
func (s *Source) Watch(ctx context.Context) {
	every := s.Current().Reload.Interval
	if every <= 0 || s.path == "" {
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

	next, err := LoadMesh(s.path)
	if err == nil {
		if changed := next.keepStatic(old); len(changed) > 0 {
			s.log.Warn("config_restart_required", "config", s.path, "fields", changed)
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
