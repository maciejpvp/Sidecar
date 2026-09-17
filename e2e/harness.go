// Package e2e wires real sidecars and toy services together so a request can be
// followed end to end. Shared by the tests here and the example in ./demo.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"time"

	"sidecar/internal/proxy"
	"sidecar/internal/routing"
)

// Received is what an Echo reports about the request it got: the view from the
// far side of the sidecar hop.
type Received struct {
	Service         string `json:"service"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	Host            string `json:"host"`
	XForwardedFor   string `json:"xForwardedFor"`
	XForwardedHost  string `json:"xForwardedHost"`
	XForwardedProto string `json:"xForwardedProto"`
}

// Echo is a toy service: it answers every request by describing it.
type Echo struct {
	Name string
	URL  string

	requests atomic.Uint64
	srv      *httptest.Server
}

// StartEcho starts an echo service on its own loopback port.
func StartEcho(name string) *Echo {
	e := &Echo{Name: name}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Received{
			Service:         name,
			Method:          r.Method,
			Path:            r.URL.Path,
			Host:            r.Host,
			XForwardedFor:   r.Header.Get("X-Forwarded-For"),
			XForwardedHost:  r.Header.Get("X-Forwarded-Host"),
			XForwardedProto: r.Header.Get("X-Forwarded-Proto"),
		})
	}))
	e.URL = e.srv.URL
	return e
}

// Requests counts what this instance has served, for load-balancing assertions.
func (e *Echo) Requests() uint64 { return e.requests.Load() }

// Close stops the service; its URL then refuses connections, which is how tests
// produce an unreachable upstream.
func (e *Echo) Close() { e.srv.Close() }

// Sidecar is a running outbound listener with its own routing table.
type Sidecar struct {
	Addr string

	srv *http.Server
}

// StartSidecar serves the outbound proxy on addr; "127.0.0.1:0" picks a free
// port. Routes map a service name to instance URLs — full URLs, because the
// proxy parses them with url.Parse.
func StartSidecar(addr string, routes map[string][]string, log *slog.Logger) (*Sidecar, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("bind outbound listener on %s: %w", addr, err)
	}

	s := &Sidecar{
		Addr: ln.Addr().String(),
		srv:  &http.Server{Handler: proxy.New(routing.NewTable(routes), log)},
	}
	go s.srv.Serve(ln)
	return s, nil
}

// Close drains the listener.
func (s *Sidecar) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.srv.Shutdown(ctx)
}

// Call makes the request an app would make: connect to the local sidecar, name
// the service in Host. The path is the upstream's own — the sidecar claims no
// part of it. CallViaProxy is the other addressing form.
func Call(sidecarAddr, service, path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "http://"+sidecarAddr+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = service
	return http.DefaultClient.Do(req)
}

// CallViaProxy calls as Call does, but through a client using the sidecar as its
// HTTP proxy: the name arrives in the request line (r.URL.Host), which is what
// an app gets for free from http_proxy.
func CallViaProxy(sidecarAddr, service, path string) (*http.Response, error) {
	sidecarURL, err := url.Parse("http://" + sidecarAddr)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(sidecarURL)}}
	return client.Get("http://" + service + path)
}

// ReadReceived decodes an Echo's reply and closes the body.
func ReadReceived(res *http.Response) (Received, error) {
	defer res.Body.Close()
	var got Received
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		return got, fmt.Errorf("decode echo response: %w", err)
	}
	return got, nil
}

// SidecarError is the body the sidecar returns for its own failures (DESIGN §7);
// upstream responses never look like this.
type SidecarError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ReadSidecarError decodes a sidecar error body and closes it.
func ReadSidecarError(res *http.Response) (SidecarError, error) {
	defer res.Body.Close()
	var got SidecarError
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		return got, fmt.Errorf("decode sidecar error: %w", err)
	}
	return got, nil
}
