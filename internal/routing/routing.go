package routing

import (
	"fmt"
	"net/url"
	"sync/atomic"
	"time"
)

const (
	DefaultTimeout     = 5 * time.Second
	DefaultMaxAttempts = 3
)

// Separate from Service because Service holds a counter and cannot be copied.
type ServiceConfig struct {
	Instances   []string
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

func NewTable(services map[string]ServiceConfig) (*Routes, error) {
	r := &Routes{services: make(map[string]*Service, len(services))}
	for name, cfg := range services {
		instances := make([]*url.URL, 0, len(cfg.Instances))
		for _, raw := range cfg.Instances {
			u, err := url.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("service %q: instance %q: %w", name, raw, err)
			}
			if u.Scheme == "" || u.Host == "" {
				return nil, fmt.Errorf("service %q: instance %q: want scheme://host[:port]", name, raw)
			}
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
