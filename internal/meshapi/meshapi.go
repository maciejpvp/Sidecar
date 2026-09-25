// Package meshapi is the wire contract between sidecars and the control plane
// (DESIGN §13). It is plain HTTP + JSON so it can be driven with curl:
//
//	POST /v1/heartbeat   Registration → Lease     register, or renew the lease
//	POST /v1/deregister  Registration → 204       leave now, not at lease expiry
//	GET  /v1/snapshot?version=V&wait=30s → Snapshot, or 304 when still at V
//
// A heartbeat is a full registration, not a renewal of one, so a control
// plane that restarted with an empty registry is refilled by the heartbeats it
// was going to receive anyway — no "unknown lease, please re-register" path.
package meshapi

import (
	"time"

	"sidecar/internal/config"
)

const (
	PathHeartbeat  = "/v1/heartbeat"
	PathDeregister = "/v1/deregister"
	PathSnapshot   = "/v1/snapshot"

	// MaxWait caps how long one snapshot long-poll is held open.
	MaxWait = 5 * time.Minute
)

// Registration names one instance.
type Registration struct {
	Service string `json:"service"`
	// Address is where other sidecars reach this instance, host:port.
	Address string `json:"address"`
	// ID is unique per sidecar process. The registry is keyed by address,
	// and pod IPs are reused: without an ID, the late deregistration of a pod
	// that is gone could remove the new pod now holding its IP.
	ID string `json:"id"`
}

// Lease is the control plane's answer to a heartbeat.
type Lease struct {
	TTL time.Duration `json:"ttl"`
	// Heartbeat is when to renew; a third of TTL, so one lost heartbeat is
	// not an expiry.
	Heartbeat time.Duration `json:"heartbeat"`
}

// Snapshot is everything a sidecar routes by: every service with its
// registered instances and its policy from the mesh file.
type Snapshot struct {
	// Version is opaque. It changes whenever the content may have changed,
	// and includes a per-process epoch, so a restarted control plane never
	// hands out a version an old sidecar already holds for other content.
	Version  string           `json:"version"`
	Services []config.Service `json:"services"`
}

// Error is the body of every non-2xx control-plane response.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
