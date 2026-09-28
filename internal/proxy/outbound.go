package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"

	"sidecar/internal/accesslog"
	"sidecar/internal/routing"
)

type Resolver interface {
	GetService(name string) (*routing.Service, bool)
}

// readiness is implemented by a Resolver that can be empty because it has not
// been filled yet (routing.Store before the first control-plane snapshot).
type readiness interface {
	Ready() bool
}

type Outbound struct {
	routes    Resolver
	log       *slog.Logger
	transport http.RoundTripper
}

func NewOutbound(routes Resolver, log *slog.Logger) *Outbound {
	return &Outbound{
		routes: routes,
		log:    log,
		// Owned rather than http.DefaultTransport so the connection pool is
		// ours to tune, and so TLS plugs in here later.
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
}

// CloseIdleConnections drops idle pooled connections after a routing swap.
// Connections busy at that moment linger until the transport's idle timeout.
func (h *Outbound) CloseIdleConnections() {
	if t, ok := h.transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

func serviceName(r *http.Request) string {
	name := r.URL.Host
	if name == "" {
		name = r.Host
	}
	if host, _, err := net.SplitHostPort(name); err == nil {
		name = host
	}
	return strings.ToLower(name)
}

func (h *Outbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := serviceName(r)
	rec, w := accesslog.Start(w, accesslog.Outbound, r)
	rec.Target = name
	defer rec.Emit(h.log)

	// For the two lines below that carry an error the access line cannot.
	log := h.log.With("target", name, "method", r.Method, "path", r.URL.Path)

	if name == "" {
		writeError(w, rec, http.StatusBadRequest, "no_route", "no target service in request")
		return
	}

	if r, ok := h.routes.(readiness); ok && !r.Ready() {
		// Every name is unknown before the first snapshot, and a 404 would
		// tell the app the service does not exist. It is the sidecar that is
		// not ready, and trying again shortly will work.
		writeError(w, rec, http.StatusServiceUnavailable, "mesh_not_ready", "sidecar has no routes from the control plane yet")
		return
	}

	svc, ok := h.routes.GetService(name)
	if !ok {
		writeError(w, rec, http.StatusNotFound, "no_route", "unknown service")
		return
	}
	rec.Deadline = svc.Timeout

	// Covers the response body too, not just time to first byte.
	ctx, cancel := context.WithTimeout(r.Context(), svc.Timeout)
	defer cancel()

	out := r.WithContext(ctx)
	if err := bufferBody(out, svc.Retry.MaxBodyBytes); err != nil {
		log.Error("failed to buffer request body", "error", err)
		writeError(w, rec, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	h.reverseProxy(svc, log, rec).ServeHTTP(w, out)
}

// bufferBody makes the request replayable by reading it into memory and
// handing out a fresh reader per attempt. Server-side requests have no GetBody
// of their own, so without this a retry would silently send an empty body.
// Bodies over limit, or of unknown length, are streamed and never retried.
func bufferBody(r *http.Request, limit int64) error {
	if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > limit {
		return nil
	}

	buf, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return err
	}

	r.Body = io.NopCloser(bytes.NewReader(buf))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(buf)), nil
	}
	return nil
}

func (h *Outbound) reverseProxy(svc *routing.Service, log *slog.Logger, rec *accesslog.Record) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// No SetURL: the instance is chosen per attempt inside the transport.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
		},
		Transport: &attemptTripper{svc: svc, base: h.transport, log: log, rec: rec},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, ErrNoHealthyUpstream):
				writeError(w, rec, http.StatusServiceUnavailable, "no_healthy_upstream", "no instances available")
			case errors.Is(err, context.DeadlineExceeded):
				writeError(w, rec, http.StatusGatewayTimeout, "deadline_exceeded", "upstream did not respond in time")
			case errors.Is(err, context.Canceled):
			default:
				log.Error("upstream failed", "err", err)
				writeError(w, rec, http.StatusBadGateway, "upstream_connect_failed", "upstream unavailable")
			}
		},
	}
}
