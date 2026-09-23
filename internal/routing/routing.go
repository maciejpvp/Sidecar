package routing

import (
	"net/url"
	"sync/atomic"

	"sidecar/internal/config"
)

// Service is an upstream's configured policy plus its runtime state.
type Service struct {
	Name string
	config.Policy

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

func NewTable(services []config.Service) *Routes {
	r := &Routes{services: make(map[string]*Service, len(services))}
	for _, svc := range services {
		instances := make([]*url.URL, 0, len(svc.Instances))
		for _, addr := range svc.Instances {
			instances = append(instances, &url.URL{Scheme: "http", Host: addr})
		}
		r.services[svc.Name] = &Service{
			Name:      svc.Name,
			Policy:    svc.Policy,
			instances: instances,
		}
	}
	return r
}

// A service with no instances resolves fine here and fails at Pick.
func (r *Routes) GetService(name string) (*Service, bool) {
	s, ok := r.services[name]
	return s, ok
}

// Store is the table in effect, replaced whole on reload. The proxy resolves a
// service once per request, so in-flight requests finish on their original table.
type Store struct {
	current atomic.Pointer[Routes]
}

func NewStore(r *Routes) *Store {
	s := &Store{}
	s.current.Store(r)
	return s
}

func (s *Store) Swap(r *Routes) {
	s.current.Store(r)
}

func (s *Store) GetService(name string) (*Service, bool) {
	return s.current.Load().GetService(name)
}
