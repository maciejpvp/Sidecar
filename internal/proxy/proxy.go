package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"sidecar/internal/routing"
)

type Resolver interface {
	GetService(name string) (*routing.Service, bool)
}

type Handler struct {
	routes Resolver
	log    *slog.Logger
}

func New(routes Resolver, log *slog.Logger) *Handler {
	return &Handler{routes: routes, log: log}
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

	addr, ok := svc.Pick()
	if !ok {
		log.Warn("service has no instances")
		writeError(w, http.StatusServiceUnavailable, "no_healthy_upstream", "no instances available")
		return
	}

	target, err := url.Parse(addr)
	if err != nil {
		log.Error("failed to parse target address", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_server_error", "failed to parse target address")
		return
	}

	// Covers the response body too, not just time to first byte.
	ctx, cancel := context.WithTimeout(r.Context(), svc.Timeout)
	defer cancel()

	log.Debug("forwarding request", "target", target.String(), "timeout", svc.Timeout.String())
	h.reverseProxy(target, log).ServeHTTP(w, r.WithContext(ctx))
}

func (h *Handler) reverseProxy(target *url.URL, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
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
