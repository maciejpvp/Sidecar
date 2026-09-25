package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"sidecar/internal/meshapi"
)

// Registrar keeps this instance registered for exactly as long as it should
// receive traffic: from the first healthy probe of the local app until the app
// stops answering, or the sidecar shuts down.
type Registrar struct {
	client *Client
	log    *slog.Logger
	reg    meshapi.Registration
	health func(context.Context) error

	// Probe is how often the app is checked. It is also the bound on how
	// long a dying app keeps being sent traffic, so it is short.
	Probe time.Duration

	registered atomic.Bool
}

// NewRegistrar registers service at advertise. health reports the local app's
// health; see HTTPHealth.
func NewRegistrar(client *Client, log *slog.Logger, service, advertise string, health func(context.Context) error) *Registrar {
	return &Registrar{
		client: client,
		log:    log.With("service", service, "address", advertise),
		reg:    meshapi.Registration{Service: service, Address: advertise, ID: newID()},
		health: health,
		Probe:  time.Second,
	}
}

// Registered reports whether this instance currently holds a lease.
func (r *Registrar) Registered() bool { return r.registered.Load() }

// Registration is what this sidecar registers as.
func (r *Registrar) Registration() meshapi.Registration { return r.reg }

// Run keeps the registration up to date until ctx is done, then deregisters
// before returning. Shutdown waits for it: leaving the mesh comes first, so
// callers stop sending new work before this sidecar starts draining.
func (r *Registrar) Run(ctx context.Context) {
	defer r.leave("shutdown")

	var (
		nextBeat time.Time
		retry    = backoff{min: time.Second, max: 5 * time.Second}
		warned   bool
	)
	for {
		err := r.health(ctx)
		switch {
		case ctx.Err() != nil:
			return

		case err != nil:
			if r.registered.Load() {
				r.log.Warn("app_unhealthy", "error", err)
				r.leave("app unhealthy")
			} else if !warned {
				r.log.Info("waiting_for_app", "error", err)
				warned = true
			}
			nextBeat = time.Time{} // register as soon as it recovers

		case !time.Now().Before(nextBeat):
			hbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			lease, err := r.client.Heartbeat(hbCtx, r.reg)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				// The lease, if any, is still good until TTL; retrying faster
				// than the heartbeat keeps it so through a blip.
				wait := retry.next()
				r.log.Warn("heartbeat_failed", "error", err, "retryIn", wait.String())
				nextBeat = time.Now().Add(wait)
				break
			}
			retry.reset()
			warned = false
			if !r.registered.Swap(true) {
				r.log.Info("registered", "leaseTTL", lease.TTL.String(), "heartbeat", lease.Heartbeat.String())
			}
			nextBeat = time.Now().Add(lease.Heartbeat)
		}

		if !sleep(ctx, r.Probe) {
			return
		}
	}
}

// leave deregisters if registered. It uses its own short deadline: it runs
// when the caller's context is already done.
func (r *Registrar) leave(why string) {
	if !r.registered.Swap(false) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.client.Deregister(ctx, r.reg); err != nil {
		// Not fatal: the lease expires on its own within one TTL.
		r.log.Warn("deregister_failed", "reason", why, "error", err)
		return
	}
	r.log.Info("deregistered", "reason", why)
}

// HTTPHealth probes http://addr+path; any 2xx or 3xx is healthy. An empty path
// means always healthy.
func HTTPHealth(addr, path string) func(context.Context) error {
	if path == "" {
		return func(context.Context) error { return nil }
	}
	client := &http.Client{
		Timeout: time.Second,
		// A redirect is an answer; following it could leave the pod.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	target := "http://" + addr + path
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		res, err := client.Do(req)
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 400 {
			return fmt.Errorf("GET %s: %s", target, res.Status)
		}
		return nil
	}
}

func newID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
