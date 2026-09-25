package discovery

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/meshapi"
)

// Watcher long-polls the control plane and applies every new snapshot that
// validates. Until the first one arrives it is not ready, and the sidecar
// answers 503 mesh_not_ready; after that it only ever moves forward from a
// good table, whatever the control plane does.
type Watcher struct {
	client *Client
	log    *slog.Logger
	apply  func([]config.Service)

	// Wait is how long each long-poll is held open by the control plane.
	Wait time.Duration

	current atomic.Pointer[meshapi.Snapshot]
	ready   chan struct{}
}

// NewWatcher calls apply, from the goroutine running Run, with the services of
// every accepted snapshot.
func NewWatcher(client *Client, log *slog.Logger, apply func([]config.Service)) *Watcher {
	return &Watcher{client: client, log: log, apply: apply, Wait: 30 * time.Second, ready: make(chan struct{})}
}

// Ready is closed when the first snapshot has been applied.
func (w *Watcher) Ready() <-chan struct{} { return w.ready }

// Current is the snapshot in effect, or nil before the first.
func (w *Watcher) Current() *meshapi.Snapshot { return w.current.Load() }

// Run polls until ctx is done.
func (w *Watcher) Run(ctx context.Context) {
	var (
		version string
		retry   = backoff{min: 250 * time.Millisecond, max: 10 * time.Second}
		failing bool
	)

	for ctx.Err() == nil {
		// The control plane answers by Wait at the latest; the margin covers
		// the network. Past that the connection is dead, not slow.
		pollCtx, cancel := context.WithTimeout(ctx, w.Wait+10*time.Second)
		snap, err := w.client.Snapshot(pollCtx, version, w.Wait)
		cancel()

		if err != nil {
			if ctx.Err() != nil {
				return
			}
			level := slog.LevelWarn
			switch {
			case errors.Is(err, ErrWarmingUp):
				level = slog.LevelInfo
			case failing:
				// Said once; the rest are the same outage.
				level = slog.LevelDebug
			}
			w.log.Log(ctx, level, "snapshot_fetch_failed", "error", err, "keeping", w.keeping())
			failing = true
			sleep(ctx, retry.next())
			continue
		}
		if failing {
			w.log.Info("control_plane_reachable")
			failing = false
		}
		retry.reset()
		if snap == nil {
			continue // wait ran out, nothing new
		}

		// Whether or not it is applied, this version has been seen: a bad
		// snapshot is not fetched again until the control plane changes.
		version = snap.Version
		if err := config.Snapshot(snap.Services); err != nil {
			w.log.Error("snapshot_rejected", "version", snap.Version, "error", err, "keeping", w.keeping())
			continue
		}

		w.apply(snap.Services)
		prev := w.current.Swap(snap)
		if prev == nil {
			close(w.ready)
		}
		w.log.Info("snapshot_applied", "version", snap.Version, "services", len(snap.Services), "instances", countInstances(snap))
	}
}

func (w *Watcher) keeping() string {
	if cur := w.current.Load(); cur != nil {
		return cur.Version
	}
	return "nothing yet"
}

func countInstances(s *meshapi.Snapshot) int {
	n := 0
	for _, svc := range s.Services {
		n += len(svc.Instances)
	}
	return n
}
