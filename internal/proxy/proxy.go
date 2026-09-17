package proxy

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
)

const TargetHeader = "X-Target-Service"

type Resolver interface {
	GetService(name string) (string, bool)
}

type Handler struct {
	routes Resolver
	log    *slog.Logger
}

func New(routes Resolver, log *slog.Logger) *Handler {
	return &Handler{routes: routes, log: log}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get(TargetHeader)
	log := h.log.With("service", name, "method", r.Method, "path", r.URL.Path)

	addr, ok := h.routes.GetService(name)
	if !ok {
		log.Warn("service not found")
		writeError(w, http.StatusNotFound, "service_not_found", "service not found")
		return
	}

	target, err := url.Parse(addr)
	if err != nil {
		log.Error("failed to parse target address", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_server_error", "failed to parse target address")
		return
	}

	log.Debug("forwarding request", "target", target.String())
	h.reverseProxy(target, log).ServeHTTP(w, r)
}

func (h *Handler) reverseProxy(target *url.URL, log *slog.Logger) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			pr.Out.Header.Del(TargetHeader)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("upstream failed", "err", err)
			writeError(w, http.StatusBadGateway, "upstream_connect_failed", "upstream unavailable")
		},
	}
}
