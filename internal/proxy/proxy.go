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

	"sidecar/internal/routing"
)

// Bodies up to this size are buffered so a retry can replay them. Anything
// larger, or of unknown length, is streamed straight through and not retried.
const maxReplayBodyBytes = 1 << 20

type Resolver interface {
	GetService(name string) (*routing.Service, bool)
}

type Handler struct {
	routes    Resolver
	log       *slog.Logger
	transport http.RoundTripper
}

func New(routes Resolver, log *slog.Logger) *Handler {
	return &Handler{
		routes: routes,
		log:    log,
		// Owned rather than http.DefaultTransport so the connection pool is
		// ours to tune, and so TLS plugs in here later.
		transport: http.DefaultTransport.(*http.Transport).Clone(),
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

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := serviceName(r)
	log := h.log.With("service", name, "method", r.Method, "path", r.URL.Path)

	if name == "" {
		log.Warn("request has no target service")
		writeError(w, http.StatusBadRequest, "no_route", "no target service in request")
		return
	}

	svc, ok := h.routes.GetService(name)
	if !ok {
		log.Warn("service not found")
		writeError(w, http.StatusNotFound, "no_route", "unknown service")
		return
	}

	// Covers the response body too, not just time to first byte.
	ctx, cancel := context.WithTimeout(r.Context(), svc.Timeout)
	defer cancel()

	out := r.WithContext(ctx)
	if err := bufferBody(out); err != nil {
		log.Error("failed to buffer request body", "error", err)
		writeError(w, http.StatusBadRequest, "bad_request", "failed to read request body")
		return
	}

	log.Debug("forwarding request", "timeout", svc.Timeout.String(), "maxAttempts", svc.MaxAttempts)
	h.reverseProxy(svc, log).ServeHTTP(w, out)
}

// bufferBody makes the request replayable by reading it into memory and
// handing out a fresh reader per attempt. Server-side requests have no GetBody
// of their own, so without this a retry would silently send an empty body.
func bufferBody(r *http.Request) error {
	if r.Body == nil || r.ContentLength <= 0 || r.ContentLength > maxReplayBodyBytes {
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

func (h *Handler) reverseProxy(svc *routing.Service, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		// No SetURL: the instance is chosen per attempt inside the transport.
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetXForwarded()
		},
		Transport: &attemptTripper{svc: svc, base: h.transport, log: log},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, ErrNoHealthyUpstream):
				log.Warn("no instances available")
				writeError(w, http.StatusServiceUnavailable, "no_healthy_upstream", "no instances available")
			case errors.Is(err, context.DeadlineExceeded):
				log.Warn("deadline exceeded", "err", err)
				writeError(w, http.StatusGatewayTimeout, "deadline_exceeded", "upstream did not respond in time")
			case errors.Is(err, context.Canceled):
				// Caller hung up; no one left to send a status to.
				log.Debug("client cancelled", "err", err)
			default:
				log.Error("upstream failed", "err", err)
				writeError(w, http.StatusBadGateway, "upstream_connect_failed", "upstream unavailable")
			}
		},
	}
}
