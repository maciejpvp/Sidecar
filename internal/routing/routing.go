package routing

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	DefaultTimeout     = 5 * time.Second
	DefaultMaxAttempts = 3
)

// Separate from Service because Service holds a counter and cannot be copied.
type ServiceConfig struct {
	Instances   []string      // "host:port", no scheme (CONFIG.md §Validation 4)
	Timeout     time.Duration // 0 → DefaultTimeout
	MaxAttempts int           // 0 → DefaultMaxAttempts; 1 disables retries
}

type Service struct {
	Name        string
	Timeout     time.Duration
	MaxAttempts int

	instances []*url.URL
	next      atomic.Uint64
}

// Pick returns the next instance by round-robin, skipping any in exclude. It
// scans at most one full lap, so an instance the caller has already tried is
// only returned once every other instance has had a turn.
func (s *Service) Pick(exclude []*url.URL) (*url.URL, bool) {
	n := uint64(len(s.instances))
	if n == 0 {
		return nil, false
	}

	start := s.next.Add(1) - 1
	for i := uint64(0); i < n; i++ {
		candidate := s.instances[(start+i)%n]
		if !contains(exclude, candidate) {
			return candidate, true
		}
	}
	return nil, false
}

func (s *Service) InstanceCount() int { return len(s.instances) }

func contains(list []*url.URL, u *url.URL) bool {
	for _, v := range list {
		if v == u {
			return true
		}
	}
	return false
}

type Routes struct {
	services map[string]*Service
}

// parseInstance turns one configured address into the URL used to dial it.
//
// The host may be anything net.Dial can resolve — a DNS name, an IPv4 literal,
// or a bracketed IPv6 literal — since instances are wherever the operator runs
// them. Only the port is mandatory, and the sidecar never resolves the name
// itself: DNS stays with the dialler, so a name that moves is picked up without
// a config reload.
//
// The configured form is bare `host:port` — an instance is another sidecar's
// inbound port, and the scheme is not the operator's choice to make per
// instance: v1 speaks plain HTTP and TLS goes behind the Transport seam
// (DESIGN §6). Keeping the address scheme-less also gives one canonical string
// for the access log and for outlier state keys (`service|addr`), so those
// never disagree with the config file.
func parseInstance(addr string) (*url.URL, error) {
	if strings.Contains(addr, "/") {
		return nil, errors.New("want host:port without a scheme")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Most likely an unbracketed IPv6 address. It cannot be accepted even
		// in principle: in "2001:db8::7:15000" there is no way to tell whether
		// the last group is a port or part of the address.
		if strings.Count(addr, ":") > 1 && !strings.Contains(addr, "[") {
			return nil, errors.New("want host:port, with an IPv6 address in brackets: [2001:db8::7]:15000")
		}
		return nil, fmt.Errorf("want host:port: %w", err)
	}
	if host == "" {
		return nil, errors.New("want host:port: host is empty")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("want host:port: port %q is not 1..65535", port)
	}
	return &url.URL{Scheme: "http", Host: addr}, nil
}

func NewTable(services map[string]ServiceConfig) (*Routes, error) {
	r := &Routes{services: make(map[string]*Service, len(services))}
	for name, cfg := range services {
		instances := make([]*url.URL, 0, len(cfg.Instances))
		seen := make(map[string]bool, len(cfg.Instances))
		for _, raw := range cfg.Instances {
			u, err := parseInstance(raw)
			if err != nil {
				return nil, fmt.Errorf("service %q: instance %q: %w", name, raw, err)
			}
			// Duplicates would get two slots in the pool, two sets of outlier
			// state and a skewed share of the round-robin.
			if seen[u.Host] {
				return nil, fmt.Errorf("service %q: instance %q: listed twice", name, raw)
			}
			seen[u.Host] = true
			instances = append(instances, u)
		}

		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		maxAttempts := cfg.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = DefaultMaxAttempts
		}

		r.services[name] = &Service{
			Name:        name,
			Timeout:     timeout,
			MaxAttempts: maxAttempts,
			instances:   instances,
		}
	}
	return r, nil
}

// A service with no instances resolves fine here and fails at Pick.
func (r *Routes) GetService(name string) (*Service, bool) {
	s, ok := r.services[name]
	return s, ok
}
