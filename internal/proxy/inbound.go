package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"sidecar/internal/accesslog"
	"sidecar/internal/config"
	"sidecar/internal/reqctx"
)

// Inbound is the mesh's way into this pod: it stamps request context and hands
// the request to the local app. No routing, no retries, no balancing — there is
// exactly one app (DESIGN §3.2).
type Inbound struct {
	app       *url.URL
	timeouts  config.Inbound
	log       *slog.Logger
	transport http.RoundTripper
}

func NewInbound(appAddr string, timeouts config.Inbound, log *slog.Logger) *Inbound {
	return &Inbound{
		app:       &url.URL{Scheme: "http", Host: appAddr},
		timeouts:  timeouts,
		log:       log,
		transport: http.DefaultTransport.(*http.Transport).Clone(),
	}
}

func (h *Inbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Set(reqctx.HeaderRequestID, reqctx.RequestID(r.Header.Get(reqctx.HeaderRequestID)))
	r.Header.Set(reqctx.HeaderTraceparent, reqctx.Traceparent(r.Header.Get(reqctx.HeaderTraceparent)))

	rec, w := accesslog.Start(w, accesslog.Inbound, r)
	defer rec.Emit(h.log)

	budget := h.budget(r.Header.Get(reqctx.HeaderTimeoutMs))
	rec.Deadline = budget
	// Set, not added to: the absolute form is host-local, so a caller on the
	// network has no business supplying one.
	r.Header.Set(reqctx.HeaderDeadline, reqctx.Deadline(time.Now().Add(budget)))

	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()

	h.reverseProxy(rec).ServeHTTP(w, r.WithContext(ctx))
}

// budget is what the caller asked for, clamped to what this service allows.
func (h *Inbound) budget(timeoutMs string) time.Duration {
	asked, ok := reqctx.Timeout(timeoutMs)
	if !ok {
		return h.timeouts.DefaultTimeout
	}
	return min(asked, h.timeouts.MaxTimeout)
}

func (h *Inbound) reverseProxy(rec *accesslog.Record) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(h.app)
			pr.SetXForwarded()
		},
		Transport: h.transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				writeError(w, rec, http.StatusGatewayTimeout, "deadline_exceeded", "app did not respond in time")
			case errors.Is(err, context.Canceled):
				// Caller hung up; the access line reports status 0.
			default:
				h.log.Error("app unreachable", "err", err, "method", r.Method, "path", r.URL.Path)
				writeError(w, rec, http.StatusBadGateway, "app_unavailable", "local app unavailable")
			}
		},
	}
}
