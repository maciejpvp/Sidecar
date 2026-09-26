package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/meshapi"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)} }

var (
	b1 = meshapi.Registration{Service: "service-b", Address: "10.0.0.1:15000", ID: "pod-1"}
	b2 = meshapi.Registration{Service: "service-b", Address: "10.0.0.2:15000", ID: "pod-2"}
)

func TestLeaseExpires(t *testing.T) {
	c := newClock()
	r := NewRegistry(c.now)
	r.Heartbeat(b1, 15*time.Second)
	r.Heartbeat(b2, 15*time.Second)

	c.advance(10 * time.Second)
	r.Heartbeat(b2, 15*time.Second) // b2 renews, b1 has gone silent
	c.advance(5 * time.Second)

	gone := r.Expire()
	if len(gone) != 1 || gone[0] != b1 {
		t.Fatalf("expired %v, want just %v", gone, b1)
	}
	_, instances, _ := r.View()
	if got := instances["service-b"]; !slices.Equal(got, []string{b2.Address}) {
		t.Errorf("instances = %v, want only %s", got, b2.Address)
	}
}

func TestVersionMovesOnlyOnVisibleChange(t *testing.T) {
	r := NewRegistry(nil)
	v0, _, _ := r.View()

	if change, _ := r.Heartbeat(b1, time.Minute); change != Registered {
		t.Errorf("first heartbeat = %v, want Registered", change)
	}
	v1, _, _ := r.View()
	if v1 == v0 {
		t.Error("registering did not change the version")
	}

	if change, _ := r.Heartbeat(b1, time.Minute); change != Renewed {
		t.Errorf("second heartbeat = %v, want Renewed", change)
	}
	if v, _, _ := r.View(); v != v1 {
		t.Error("a renewal changed the version, which wakes every long-poll for nothing")
	}

	r.Touch()
	if v, _, _ := r.View(); v == v1 {
		t.Error("Touch (a mesh file change) did not change the version")
	}
}

// Pod IPs are reused. The newest registration of an address wins, and the
// late goodbye of the pod that used to hold it removes nothing.
func TestAddressReuse(t *testing.T) {
	r := NewRegistry(nil)
	r.Heartbeat(b1, time.Minute)

	newPod := meshapi.Registration{Service: "service-c", Address: b1.Address, ID: "pod-9"}
	if change, prev := r.Heartbeat(newPod, time.Minute); change != Replaced || prev != "service-b" {
		t.Errorf("heartbeat = %v (previous %q), want Replaced from service-b", change, prev)
	}
	if r.Deregister(b1) {
		t.Error("the old pod's deregistration removed the new pod")
	}

	_, instances, _ := r.View()
	if len(instances["service-b"]) != 0 || len(instances["service-c"]) != 1 {
		t.Errorf("instances = %v, want the address under service-c only", instances)
	}
	if !r.Deregister(newPod) {
		t.Error("the holder's own deregistration was refused")
	}
}

func TestChangeWakesWaiters(t *testing.T) {
	r := NewRegistry(nil)
	_, _, changed := r.View()
	r.Heartbeat(b1, time.Minute)
	select {
	case <-changed:
	default:
		t.Fatal("a registration did not wake a waiter on the previous version")
	}
}

type fixture struct {
	t     *testing.T
	clock *clock
	reg   *Registry
	mesh  *config.Mesh
	srv   *Server
}

func newFixture(t *testing.T, warmup time.Duration) *fixture {
	c := newClock()
	reg := NewRegistry(c.now)
	mesh := config.DefaultMesh()
	mesh.Services = []config.ServicePolicy{{Name: "declared", Policy: mesh.Defaults}}
	mesh.Services[0].Timeout = 2 * time.Second
	return &fixture{t: t, clock: c, reg: reg, mesh: mesh,
		srv: NewServer(config.Static(mesh), reg, slog.New(slog.DiscardHandler), warmup)}
}

func (f *fixture) do(method, target string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		buf, _ := json.Marshal(b)
		r = bytes.NewReader(buf)
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, httptest.NewRequest(method, target, r))
	return rec
}

func (f *fixture) snapshot(target string) meshapi.Snapshot {
	f.t.Helper()
	rec := f.do(http.MethodGet, target, nil)
	if rec.Code != http.StatusOK {
		f.t.Fatalf("GET %s = %d %s", target, rec.Code, rec.Body)
	}
	var snap meshapi.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		f.t.Fatal(err)
	}
	return snap
}

func TestHeartbeatReturnsLease(t *testing.T) {
	f := newFixture(t, 0)
	rec := f.do(http.MethodPost, meshapi.PathHeartbeat, b1)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
	}
	var lease meshapi.Lease
	json.Unmarshal(rec.Body.Bytes(), &lease)
	if lease.TTL != 15*time.Second || lease.Heartbeat != 5*time.Second {
		t.Errorf("lease = %+v, want TTL 15s, heartbeat 5s from the mesh file", lease)
	}
}

func TestHeartbeatRejectsBadRegistration(t *testing.T) {
	f := newFixture(t, 0)
	for name, body := range map[string]any{
		"not json":         "{",
		"unknown field":    `{"service":"a","address":"10.0.0.1:1","id":"x","weight":3}`,
		"bad service name": meshapi.Registration{Service: "Bad_Name", Address: "10.0.0.1:15000", ID: "x"},
		"address scheme":   meshapi.Registration{Service: "a", Address: "http://10.0.0.1:15000", ID: "x"},
		"no id":            meshapi.Registration{Service: "a", Address: "10.0.0.1:15000"},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := f.do(http.MethodPost, meshapi.PathHeartbeat, body); rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
	if f.reg.Len() != 0 {
		t.Error("a rejected registration was stored")
	}
}

func TestSnapshotContent(t *testing.T) {
	f := newFixture(t, 0)
	f.do(http.MethodPost, meshapi.PathHeartbeat, b2)
	f.do(http.MethodPost, meshapi.PathHeartbeat, b1)

	snap := f.snapshot(meshapi.PathSnapshot)
	if len(snap.Services) != 2 {
		t.Fatalf("services = %+v, want declared and service-b", snap.Services)
	}
	declared, b := snap.Services[0], snap.Services[1]
	if declared.Name != "declared" || len(declared.Instances) != 0 || declared.Timeout != 2*time.Second {
		t.Errorf("declared = %+v, want no instances and its 2s timeout", declared)
	}
	if b.Name != "service-b" || !slices.Equal(b.Instances, []string{b1.Address, b2.Address}) {
		t.Errorf("service-b = %+v, want both instances, sorted", b)
	}
	if b.Timeout != f.mesh.Defaults.Timeout {
		t.Errorf("service-b timeout = %v, want the default for an undeclared service", b.Timeout)
	}
	// What a sidecar will run on it.
	if err := config.Snapshot(snap.Services); err != nil {
		t.Errorf("snapshot does not validate: %v", err)
	}
}

func TestSnapshotLongPoll(t *testing.T) {
	f := newFixture(t, 0)
	v := f.snapshot(meshapi.PathSnapshot).Version

	if rec := f.do(http.MethodGet, meshapi.PathSnapshot+"?version="+v+"&wait=0s", nil); rec.Code != http.StatusNotModified {
		t.Errorf("current version, no wait = %d, want 304", rec.Code)
	}

	got := make(chan meshapi.Snapshot)
	go func() { got <- f.snapshot(meshapi.PathSnapshot + "?version=" + v + "&wait=1m") }()

	// Nothing may come back until something changes.
	select {
	case s := <-got:
		t.Fatalf("long-poll answered with nothing new: %+v", s)
	case <-time.After(50 * time.Millisecond):
	}

	f.do(http.MethodPost, meshapi.PathHeartbeat, b1)
	select {
	case s := <-got:
		if s.Version == v {
			t.Error("woken long-poll returned the old version")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not wake the long-poll")
	}
}

func TestWarmup(t *testing.T) {
	f := newFixture(t, 15*time.Second)

	rec := f.do(http.MethodGet, meshapi.PathSnapshot, nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "15" {
		t.Errorf("snapshot during warmup = %d, Retry-After %q; want 503, 15", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := f.do(http.MethodGet, "/readyz", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz during warmup = %d, want 503", rec.Code)
	}
	// Heartbeats are what warmup waits for, so they are accepted throughout.
	if rec := f.do(http.MethodPost, meshapi.PathHeartbeat, b1); rec.Code != http.StatusOK {
		t.Errorf("heartbeat during warmup = %d, want 200", rec.Code)
	}

	f.clock.advance(15 * time.Second)
	if snap := f.snapshot(meshapi.PathSnapshot); len(snap.Services) != 2 {
		t.Errorf("after warmup: %+v, want the instance registered during it", snap.Services)
	}
}

func TestDeregisterIsIdempotent(t *testing.T) {
	f := newFixture(t, 0)
	f.do(http.MethodPost, meshapi.PathHeartbeat, b1)
	for range 2 {
		if rec := f.do(http.MethodPost, meshapi.PathDeregister, b1); rec.Code != http.StatusNoContent {
			t.Errorf("deregister = %d, want 204", rec.Code)
		}
	}
	if f.reg.Len() != 0 {
		t.Error("instance still registered")
	}
}

func TestMeshReloadIsANewVersion(t *testing.T) {
	path := t.TempDir() + "/mesh.yaml"
	write := func(timeout string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("services:\n  - name: declared\n    timeout: "+timeout+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("1s")
	mesh, err := config.New(path, config.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(mesh, NewRegistry(nil), slog.New(slog.DiscardHandler), 0)
	before := srv.Snapshot()

	write("3s")
	if err := mesh.Reload(); err != nil {
		t.Fatal(err)
	}
	after := srv.Snapshot()
	if after.Version == before.Version {
		t.Error("a policy change kept the version, so no sidecar would fetch it")
	}
	if got := after.Services[0].Timeout; got != 3*time.Second {
		t.Errorf("timeout = %v, want the reloaded 3s", got)
	}
}
