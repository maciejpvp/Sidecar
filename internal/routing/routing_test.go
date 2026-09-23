package routing

import (
	"net/url"
	"testing"
	"time"

	"sidecar/internal/config"
)

// Instances are dialled over plain HTTP in v1; the scheme is not per-instance config.
func TestInstanceDialsHTTP(t *testing.T) {
	table := NewTable([]config.Service{config.NewService("svc", "10.0.0.7:15000")})
	svc, _ := table.GetService("svc")

	got, ok := svc.Pick(nil)
	if !ok {
		t.Fatal("Pick found no instance")
	}
	if got.Scheme != "http" {
		t.Errorf("scheme = %q, want http", got.Scheme)
	}
	// Host stays the configured string, so logs and outlier keys match the config.
	if got.Host != "10.0.0.7:15000" {
		t.Errorf("host = %q, want 10.0.0.7:15000", got.Host)
	}
	if got.Path != "" {
		t.Errorf("path = %q, want empty", got.Path)
	}
}

// Routing owns no defaults: the resolved policy reaches the proxy unchanged.
func TestNewTableCarriesPolicy(t *testing.T) {
	custom := config.NewService("custom", "10.0.0.7:15000")
	custom.Timeout = time.Second
	custom.Retry.MaxAttempts = 1
	custom.Retry.Backoff.Base = 7 * time.Millisecond

	table := NewTable([]config.Service{custom, config.NewService("default", "10.0.0.8:15000")})

	svc, _ := table.GetService("custom")
	if svc.Timeout != time.Second || svc.Retry.MaxAttempts != 1 || svc.Retry.Backoff.Base != 7*time.Millisecond {
		t.Errorf("policy lost: timeout = %v, maxAttempts = %d, backoff.base = %v",
			svc.Timeout, svc.Retry.MaxAttempts, svc.Retry.Backoff.Base)
	}

	svc, _ = table.GetService("default")
	if want := config.NewService("x").Policy; svc.Policy != want {
		t.Errorf("default policy = %+v, want %+v", svc.Policy, want)
	}
}

func TestUnknownService(t *testing.T) {
	table := NewTable(nil)
	if _, ok := table.GetService("nope"); ok {
		t.Error("GetService found a service in an empty table")
	}
}

// Pick walks the pool in order and skips what the caller has already tried,
// which is what keeps retries off an instance that just failed.
func TestPickRoundRobinSkipsExcluded(t *testing.T) {
	table := NewTable([]config.Service{
		config.NewService("svc", "10.0.0.1:15000", "10.0.0.2:15000", "10.0.0.3:15000"),
	})
	svc, _ := table.GetService("svc")

	first, ok := svc.Pick(nil)
	if !ok {
		t.Fatal("Pick found no instance")
	}
	second, ok := svc.Pick([]*url.URL{first})
	if !ok {
		t.Fatal("Pick found no second instance")
	}
	if second.Host == first.Host {
		t.Errorf("Pick returned %s twice, want a different instance", first.Host)
	}

	// Every instance excluded: nowhere left to send it, which the proxy turns
	// into 503 no_healthy_upstream rather than a retry.
	if _, ok := svc.Pick(svc.instances); ok {
		t.Error("Pick returned an instance although all were excluded")
	}
}

func TestPickWithNoInstances(t *testing.T) {
	table := NewTable([]config.Service{config.NewService("svc")})
	svc, _ := table.GetService("svc")

	if _, ok := svc.Pick(nil); ok {
		t.Error("Pick returned an instance from an empty pool")
	}
	if svc.InstanceCount() != 0 {
		t.Errorf("InstanceCount = %d, want 0", svc.InstanceCount())
	}
}

// Round-robin should spread requests evenly, not favour the first instance.
func TestPickSpreadsEvenly(t *testing.T) {
	table := NewTable([]config.Service{config.NewService("svc", "10.0.0.1:15000", "10.0.0.2:15000")})
	svc, _ := table.GetService("svc")

	counts := map[string]int{}
	for range 10 {
		got, ok := svc.Pick(nil)
		if !ok {
			t.Fatal("Pick found no instance")
		}
		counts[got.Host]++
	}

	for host, n := range counts {
		if n != 5 {
			t.Errorf("%s served %d of 10 picks, want 5", host, n)
		}
	}
}

// A request that resolved its service before a swap keeps a working *Service.
func TestStoreSwap(t *testing.T) {
	first := NewTable([]config.Service{config.NewService("svc", "10.0.0.1:15000")})
	second := NewTable([]config.Service{
		config.NewService("svc", "10.0.0.2:15000"),
		config.NewService("added", "10.0.0.3:15000"),
	})

	store := NewStore(first)
	inFlight, ok := store.GetService("svc")
	if !ok {
		t.Fatal("svc missing before swap")
	}
	if _, ok := store.GetService("added"); ok {
		t.Fatal("added resolved before it was configured")
	}

	store.Swap(second)

	svc, ok := store.GetService("svc")
	if !ok {
		t.Fatal("svc missing after swap")
	}
	if got, _ := svc.Pick(nil); got.Host != "10.0.0.2:15000" {
		t.Errorf("after swap svc picks %s, want 10.0.0.2:15000", got.Host)
	}
	if _, ok := store.GetService("added"); !ok {
		t.Error("added not resolvable after swap")
	}
	if got, _ := inFlight.Pick(nil); got.Host != "10.0.0.1:15000" {
		t.Errorf("in-flight service picks %s, want its original 10.0.0.1:15000", got.Host)
	}
}
