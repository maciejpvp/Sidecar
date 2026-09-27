// Package accesslog records one line per request, in the shape DESIGN §11
// specifies. Its own package: the inbound listener (§3.2) logs the same shape.
package accesslog

import (
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"sidecar/internal/reqctx"
)

type Dir string

const (
	Outbound Dir = "outbound"
	Inbound  Dir = "inbound"
)

// Record is one request's access line, filled in as the request progresses.
type Record struct {
	Target string

	// Deadline: the service timeout today, min(propagated, timeout) after §5.3.
	Deadline time.Duration

	dir          Dir
	requestID    string
	traceID      string
	method       string
	path         string
	attempts     int
	instances    []string
	sidecarError string
	start        time.Time
	w            *responseWriter
}

// Start opens a record for r and wraps w, so the status that reaches the client
// is observable.
func Start(w http.ResponseWriter, dir Dir, r *http.Request) (*Record, http.ResponseWriter) {
	rw := &responseWriter{ResponseWriter: w}
	return &Record{
		dir:       dir,
		requestID: r.Header.Get(reqctx.HeaderRequestID),
		traceID:   reqctx.TraceID(r.Header.Get(reqctx.HeaderTraceparent)),
		method:    r.Method,
		path:      r.URL.Path,
		start:     time.Now(),
		w:         rw,
	}, rw
}

// Attempt records one try against target: one entry per attempt, so a retry
// lapping over a small pool stays visible.
func (rec *Record) Attempt(target *url.URL) {
	if rec == nil {
		return // a transport built without a record still works
	}
	rec.attempts++
	rec.instances = append(rec.instances, target.Host)
}

// Fail records the code this sidecar answered with — not read back off
// X-Sidecar-Error, which also carries upstream sidecars' codes (§7).
func (rec *Record) Fail(code string) {
	if rec == nil || rec.sidecarError != "" {
		return // the first code is the one the client got
	}
	rec.sidecarError = code
}

// Emit writes the line. log supplies "self" and nothing else, so no key lands
// twice.
func (rec *Record) Emit(log *slog.Logger) {
	instances := rec.instances
	if instances == nil {
		instances = []string{} // [] rather than null
	}

	log.Info("access",
		"dir", string(rec.dir),
		"requestId", rec.requestID,
		"traceId", rec.traceID,
		"method", rec.method,
		"target", rec.Target,
		"path", rec.path,
		"status", rec.w.Status(),
		"attempts", rec.attempts,
		"instances", instances,
		"durationMs", time.Since(rec.start).Milliseconds(),
		"deadlineMs", rec.Deadline.Milliseconds(),
		"sidecarError", rec.sidecarError,
	)
}
