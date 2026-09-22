package routing

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestInstanceAddressForm(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr string // substring; empty means the address is accepted
	}{
		// The host is whatever the dialler can resolve: instances live wherever
		// the operator runs them, so nothing here narrows that on purpose.
		{name: "ipv4", addr: "10.0.0.7:15000"},
		{name: "loopback", addr: "127.0.0.1:8080"},
		{name: "dns single label", addr: "orders-svc:15000"},
		{name: "dns dotted", addr: "orders-svc.internal:15000"},
		{name: "dns fully qualified", addr: "orders-svc.internal.:15000"},
		{name: "dns mixed case", addr: "ORDERS-SVC.Internal:15000"},
		{name: "dns punycode", addr: "xn--caf-dma.example:15000"},
		{name: "ipv6 loopback", addr: "[::1]:15000"},
		{name: "ipv6", addr: "[2001:db8::7]:15000"},
		{name: "ipv6 zone", addr: "[fe80::1%eth0]:15000"},

		// An unbracketed IPv6 address cannot be accepted even in principle:
		// the last group is ambiguously a port or part of the address. The
		// error has to say so, since the fix is not obvious.
		{name: "ipv6 unbracketed", addr: "2001:db8::7:15000", wantErr: "in brackets"},
		{name: "ipv6 loopback unbracketed", addr: "::1:15000", wantErr: "in brackets"},

		// The old spike form. Worth a precise error rather than a dial that
		// fails much later, since every doc and log line now says host:port.
		{name: "scheme", addr: "http://10.0.0.7:15000", wantErr: "without a scheme"},
		{name: "scheme and path", addr: "https://www.example.com/", wantErr: "without a scheme"},
		{name: "trailing path", addr: "10.0.0.7:15000/v1", wantErr: "without a scheme"},

		{name: "no port", addr: "10.0.0.7", wantErr: "want host:port"},
		{name: "dns name with no port", addr: "orders-svc.internal", wantErr: "want host:port"},
		{name: "no host", addr: ":15000", wantErr: "host is empty"},
		{name: "port zero", addr: "10.0.0.7:0", wantErr: "not 1..65535"},
		{name: "port too high", addr: "10.0.0.7:70000", wantErr: "not 1..65535"},
		{name: "named port", addr: "10.0.0.7:http", wantErr: "not 1..65535"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTable(map[string]ServiceConfig{"svc": {Instances: []string{tc.addr}}})

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("NewTable(%q) = %v, want it accepted", tc.addr, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("NewTable(%q) = nil, want an error", tc.addr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			// The operator needs to know which entry to go and fix.
			if !strings.Contains(err.Error(), "svc") || !strings.Contains(err.Error(), tc.addr) {
				t.Errorf("error = %q, want it to name the service and the address", err)
			}
		})
	}
}

// Instances are dialled over plain HTTP in v1; the scheme is not per-instance config.
func TestInstanceDialsHTTP(t *testing.T) {
	table, err := NewTable(map[string]ServiceConfig{"svc": {Instances: []string{"10.0.0.7:15000"}}})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
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

// A duplicate would get two slots in the pool and two sets of outlier state.
func TestRejectsDuplicateInstance(t *testing.T) {
	_, err := NewTable(map[string]ServiceConfig{
		"svc": {Instances: []string{"10.0.0.7:15000", "10.0.0.8:15000", "10.0.0.7:15000"}},
	})
	if err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Errorf("error = %v, want it to reject the duplicate", err)
	}
}

// The same address under two services is two different upstreams, not a duplicate.
func TestSameAddressInTwoServices(t *testing.T) {
	_, err := NewTable(map[string]ServiceConfig{
		"a": {Instances: []string{"10.0.0.7:15000"}},
		"b": {Instances: []string{"10.0.0.7:15000"}},
	})
	if err != nil {
		t.Errorf("NewTable = %v, want it accepted", err)
	}
}

func TestDefaultsApplied(t *testing.T) {
	table, err := NewTable(map[string]ServiceConfig{
		"explicit": {Instances: []string{"10.0.0.7:15000"}, Timeout: time.Second, MaxAttempts: 1},
		"default":  {Instances: []string{"10.0.0.8:15000"}},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}

	svc, _ := table.GetService("default")
	if svc.Timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want %v", svc.Timeout, DefaultTimeout)
	}
	if svc.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("maxAttempts = %d, want %d", svc.MaxAttempts, DefaultMaxAttempts)
	}

	svc, _ = table.GetService("explicit")
	if svc.Timeout != time.Second || svc.MaxAttempts != 1 {
		t.Errorf("overrides lost: timeout = %v, maxAttempts = %d", svc.Timeout, svc.MaxAttempts)
	}
}

func TestUnknownService(t *testing.T) {
	table, err := NewTable(nil)
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
	if _, ok := table.GetService("nope"); ok {
		t.Error("GetService found a service in an empty table")
	}
}

// Pick walks the pool in order and skips what the caller has already tried,
// which is what keeps retries off an instance that just failed.
func TestPickRoundRobinSkipsExcluded(t *testing.T) {
	table, err := NewTable(map[string]ServiceConfig{
		"svc": {Instances: []string{"10.0.0.1:15000", "10.0.0.2:15000", "10.0.0.3:15000"}},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
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
	table, err := NewTable(map[string]ServiceConfig{"svc": {}})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
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
	table, err := NewTable(map[string]ServiceConfig{
		"svc": {Instances: []string{"10.0.0.1:15000", "10.0.0.2:15000"}},
	})
	if err != nil {
		t.Fatalf("NewTable: %v", err)
	}
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
