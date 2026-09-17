package routing

import "sync/atomic"

type service struct {
	endpoints []string
	next      atomic.Uint64
}

type Routes struct {
	services map[string]*service
}

func NewTable(services map[string][]string) *Routes {
	r := &Routes{services: make(map[string]*service, len(services))}
	for name, endpoints := range services {
		r.services[name] = &service{endpoints: endpoints}
	}
	return r
}

// Uses round-robin load balancing
func (r *Routes) GetService(name string) (string, bool) {
	s, ok := r.services[name]
	if !ok || len(s.endpoints) == 0 {
		return "", false
	}
	i := s.next.Add(1) - 1
	return s.endpoints[i%uint64(len(s.endpoints))], true
}
