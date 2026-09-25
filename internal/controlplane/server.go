package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/meshapi"
)

const (
	defaultWait  = 30 * time.Second
	maxBodyBytes = 16 << 10
	maxIDLength  = 128
)

// Server is the control plane's HTTP API (package meshapi documents the wire).
type Server struct {
	reg  *Registry
	mesh *config.Source
	log  *slog.Logger
	now  func() time.Time

	// readyAt is when snapshots start being served; see NewServer.
	readyAt time.Time

	mux *http.ServeMux
}

// NewServer serves reg with the policy in mesh.
//
// warmup is how long after start snapshots are refused with 503. A control
// plane that has just restarted has an empty registry; handing that out would
// make every sidecar drop every route. After one lease TTL, every live
// instance has heartbeated at least twice, so warmup should be the lease TTL
// (what cmd/controlplane passes). Zero is for tests and first-ever installs.
func NewServer(mesh *config.Source, reg *Registry, log *slog.Logger, warmup time.Duration) *Server {
	s := &Server{
		reg:  reg,
		mesh: mesh,
		log:  log,
		now:  reg.now,
		mux:  http.NewServeMux(),
	}
	s.readyAt = s.now().Add(warmup)

	s.mux.HandleFunc("POST "+meshapi.PathHeartbeat, s.heartbeat)
	s.mux.HandleFunc("POST "+meshapi.PathDeregister, s.deregister)
	s.mux.HandleFunc("GET "+meshapi.PathSnapshot, s.snapshot)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if left := s.warmingUp(); left > 0 {
			writeError(w, http.StatusServiceUnavailable, "warming_up", fmt.Sprintf("serving snapshots in %v", left.Round(time.Second)))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mesh.OnChange(func(_, next *config.Mesh) {
		// New policy is a new snapshot for every sidecar, even with no
		// registry change. The mesh is stored before this runs, so any
		// snapshot built at the new version carries the new policy.
		reg.Touch()
		s.log.Info("mesh_policy_changed", "services", len(next.Services))
	})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// Run expires leases until ctx is done. Once a second is plenty: a lease TTL
// is at least 3s, and callers retry around a dead instance meanwhile.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.expire()
		}
	}
}

func (s *Server) expire() {
	for _, gone := range s.reg.Expire() {
		// The pod died without deregistering: OOM kill, node loss, network
		// partition. Callers have been retrying around it since it stopped
		// answering; this is when it leaves their tables.
		s.log.Warn("instance_expired", "service", gone.Service, "address", gone.Address, "id", gone.ID)
	}
}

// Snapshot is the current snapshot.
func (s *Server) Snapshot() meshapi.Snapshot {
	// Version first: if the mesh changes between the two reads, the policy is
	// newer than the version, and the Touch that follows sends it again under
	// a new version — never the other way round.
	version, instances, _ := s.reg.View()
	return s.build(version, instances)
}

func (s *Server) build(version string, instances map[string][]string) meshapi.Snapshot {
	mesh := s.mesh.Current()

	// A service the mesh file names is listed even with no instances, so its
	// callers get 503 no_healthy_upstream (it exists, it is down) rather than
	// 404 no_route. An unnamed one exists while something is registered.
	names := make([]string, 0, len(mesh.Services)+len(instances))
	for _, svc := range mesh.Services {
		names = append(names, svc.Name)
	}
	for name := range instances {
		names = append(names, name)
	}
	slices.Sort(names)
	names = slices.Compact(names)

	snap := meshapi.Snapshot{Version: version, Services: make([]config.Service, 0, len(names))}
	for _, name := range names {
		addrs := instances[name]
		if addrs == nil {
			addrs = []string{}
		}
		snap.Services = append(snap.Services, config.Service{Name: name, Instances: addrs, Policy: mesh.PolicyFor(name)})
	}
	return snap
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.readRegistration(w, r)
	if !ok {
		return
	}

	ttl := s.mesh.Current().Registry.LeaseTTL
	change, prev := s.reg.Heartbeat(reg, ttl)
	log := s.log.With("service", reg.Service, "address", reg.Address, "id", reg.ID)
	switch change {
	case Registered:
		log.Info("instance_registered")
	case Replaced:
		log.Info("instance_replaced", "previousService", prev)
	default:
		log.Debug("instance_renewed")
	}

	writeJSON(w, http.StatusOK, meshapi.Lease{TTL: ttl, Heartbeat: s.mesh.Current().Registry.Heartbeat()})
}

func (s *Server) deregister(w http.ResponseWriter, r *http.Request) {
	reg, ok := s.readRegistration(w, r)
	if !ok {
		return
	}
	if s.reg.Deregister(reg) {
		s.log.Info("instance_deregistered", "service", reg.Service, "address", reg.Address, "id", reg.ID)
	}
	// Idempotent: gone is gone, whether we removed it or it had already left.
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) readRegistration(w http.ResponseWriter, r *http.Request) (meshapi.Registration, bool) {
	var reg meshapi.Registration
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reg); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "body: "+err.Error())
		return reg, false
	}

	// The same rules a sidecar applies to the snapshot, so nothing registered
	// here can make a snapshot every sidecar rejects.
	var errs []error
	if err := config.ServiceName(reg.Service); err != nil {
		errs = append(errs, fmt.Errorf("service: %w", err))
	}
	if err := config.InstanceAddr(reg.Address); err != nil {
		errs = append(errs, fmt.Errorf("address: %q: %w", reg.Address, err))
	}
	if reg.ID == "" || len(reg.ID) > maxIDLength {
		errs = append(errs, fmt.Errorf("id: must be 1..%d characters", maxIDLength))
	}
	if err := errors.Join(errs...); err != nil {
		writeError(w, http.StatusBadRequest, "bad_registration", err.Error())
		return reg, false
	}
	return reg, true
}

// snapshot answers at once if the caller's version is stale, and otherwise
// holds the request until something changes or wait runs out (304).
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	if left := s.warmingUp(); left > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(left.Seconds()))))
		writeError(w, http.StatusServiceUnavailable, "warming_up",
			"control plane restarted recently and is waiting for instances to re-register")
		return
	}

	wait := defaultWait
	if v := r.URL.Query().Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "wait: want a duration like 30s")
			return
		}
		wait = min(d, meshapi.MaxWait)
	}
	have := r.URL.Query().Get("version")

	version, instances, changed := s.reg.View()
	if version == have && wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-changed:
			version, instances, _ = s.reg.View()
		case <-t.C:
		case <-r.Context().Done():
			return
		}
	}

	if version == have {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, s.build(version, instances))
}

func (s *Server) warmingUp() time.Duration {
	return s.readyAt.Sub(s.now())
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, meshapi.Error{Code: code, Message: msg})
}
