package routing

import (
	"sync/atomic"
	"time"
)

const DefaultTimeout = 5 * time.Second

// Separate from Service because Service holds a counter and cannot be copied.
type ServiceConfig struct {
	Instances []string
	Timeout   time.Duration // 0 → DefaultTimeout
}

type Service struct {
	Name    string
	Timeout time.Duration

	instances []string
	next      atomic.Uint64
}

func (s *Service) Pick() (string, bool) {
	if len(s.instances) == 0 {
		return "", false
	}
	i := s.next.Add(1) - 1
	return s.instances[i%uint64(len(s.instances))], true
}

type Routes struct {
	services map[string]*Service
}

func NewTable(services map[string]ServiceConfig) *Routes {
	r := &Routes{services: make(map[string]*Service, len(services))}
	for name, cfg := range services {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = DefaultTimeout
		}
		r.services[name] = &Service{Name: name, Timeout: timeout, instances: cfg.Instances}
	}
	return r
}

// A service with no instances resolves fine here and fails at Pick.
func (r *Routes) GetService(name string) (*Service, bool) {
	s, ok := r.services[name]
	return s, ok
}
