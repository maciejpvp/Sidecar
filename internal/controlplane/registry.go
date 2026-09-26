// Package controlplane is the mesh's source of truth: sidecars register their
// instance and renew it with heartbeats, and fetch a versioned snapshot of
// every service's instances and policy by long-polling (DESIGN §13).
//
// State is in memory only. A control plane that restarts comes back empty and
// is refilled by heartbeats within one lease TTL; it refuses to serve
// snapshots until then (see Server), so sidecars keep their last table rather
// than being handed an empty mesh.
package controlplane

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"sidecar/internal/meshapi"
)

// Registry holds the registered instances and the snapshot version. Every
// change bumps the version and wakes every long-poll waiting on the old one.
type Registry struct {
	now func() time.Time

	mu      sync.Mutex
	byAddr  map[string]*lease // an address is one pod, so it is the key
	epoch   string
	counter uint64
	changed chan struct{} // closed and replaced on every change
}

type lease struct {
	service string
	id      string
	expires time.Time
}

func NewRegistry(now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{
		now:     now,
		byAddr:  make(map[string]*lease),
		epoch:   newEpoch(),
		changed: make(chan struct{}),
	}
}

// Change says what a Heartbeat did, for the log.
type Change int

const (
	Renewed    Change = iota // already registered as this; lease extended
	Registered               // new address
	Replaced                 // address was held by another sidecar process or service
)

// Heartbeat registers reg, or renews its lease, for ttl.
func (r *Registry) Heartbeat(reg meshapi.Registration, ttl time.Duration) (Change, string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	expires := r.now().Add(ttl)
	old, ok := r.byAddr[reg.Address]
	switch {
	case !ok:
		r.byAddr[reg.Address] = &lease{service: reg.Service, id: reg.ID, expires: expires}
		r.bump()
		return Registered, ""
	case old.service != reg.Service || old.id != reg.ID:
		// A new pod got a dead pod's IP before its lease ran out. The newest
		// heartbeat is the truth: the old pod cannot still be at this address.
		prev := old.service
		r.byAddr[reg.Address] = &lease{service: reg.Service, id: reg.ID, expires: expires}
		if prev != reg.Service {
			r.bump()
		}
		return Replaced, prev
	default:
		// A renewal changes nothing a sidecar can see, so it is not a new version.
		old.expires = expires
		return Renewed, ""
	}
}

// Deregister removes reg if it still holds its address, and reports whether it did.
func (r *Registry) Deregister(reg meshapi.Registration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	old, ok := r.byAddr[reg.Address]
	if !ok || old.service != reg.Service || old.id != reg.ID {
		return false
	}
	delete(r.byAddr, reg.Address)
	r.bump()
	return true
}

// Expire drops every lease past its expiry and returns what it dropped.
func (r *Registry) Expire() []meshapi.Registration {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	var gone []meshapi.Registration
	for addr, l := range r.byAddr {
		if !now.Before(l.expires) {
			gone = append(gone, meshapi.Registration{Service: l.service, Address: addr, ID: l.id})
			delete(r.byAddr, addr)
		}
	}
	if len(gone) > 0 {
		r.bump()
	}
	return gone
}

// Touch bumps the version without a registry change: the mesh file changed,
// and every sidecar should fetch the new policy.
func (r *Registry) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bump()
}

// View is a consistent read: the version, the instances by service, sorted,
// and a channel closed at the next change.
func (r *Registry) View() (version string, instances map[string][]string, changed <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	instances = make(map[string][]string)
	for addr, l := range r.byAddr {
		instances[l.service] = append(instances[l.service], addr)
	}
	for _, name := range slices.Collect(maps.Keys(instances)) {
		slices.Sort(instances[name])
	}
	return r.version(), instances, r.changed
}

// Len is the number of registered instances.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byAddr)
}

// bump must be called with mu held.
func (r *Registry) bump() {
	r.counter++
	close(r.changed)
	r.changed = make(chan struct{})
}

func (r *Registry) version() string {
	return fmt.Sprintf("%s-%d", r.epoch, r.counter)
}

func newEpoch() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}
