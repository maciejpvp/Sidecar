// Package accesslog records one line per request, in the shape DESIGN §11
// specifies. Its own package because the inbound listener (§3.2) will log the
// same shape with dir "inbound".
package accesslog

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Dir string

const (
	Outbound Dir = "outbound"
	Inbound  Dir = "inbound"
)

// Record is one request's access line, filled in as the request progresses.
type Record struct {
	Target string

	// Deadline is the budget the request was given: the service timeout today,
	// min(propagated budget, timeout) once §5.3 lands.
	Deadline time.Duration

	dir       Dir
	requestID string
	traceID   string
	method    string
	path      string
	attempts  int
	instances []string
	start     time.Time
	w         *responseWriter
}

// Start opens a record for r and wraps w, so the status that reaches the client
// is observable. The caller passes the returned writer down the chain.
func Start(w http.ResponseWriter, dir Dir, r *http.Request) (*Record, http.ResponseWriter) {
	rw := &responseWriter{ResponseWriter: w}
	return &Record{
		dir: dir,
		// Empty until the inbound listener stamps these (§3.2).
		requestID: r.Header.Get("X-Request-Id"),
		traceID:   traceID(r.Header.Get("Traceparent")),
		method:    r.Method,
		path:      r.URL.Path,
		start:     time.Now(),
		w:         rw,
	}, rw
}

// Attempt records one try against target: one entry per attempt, not a set of
// distinct instances, so a retry lapping over a small pool stays visible.
func (rec *Record) Attempt(target *url.URL) {
	if rec == nil {
		return // a transport built without a record still works
	}
	rec.attempts++
	rec.instances = append(rec.instances, target.Host)
}

// Emit writes the line. log supplies "self"; everything else comes from the
// record, so no key lands twice in one line.
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
		"sidecarError", rec.w.Header().Get("X-Sidecar-Error"),
	)
}

// traceID is the trace-id field of a W3C traceparent
// (version-traceid-spanid-flags), or "" when there is none to read.
func traceID(traceparent string) string {
	parts := strings.Split(traceparent, "-")
	if len(parts) < 4 {
		return ""
	}
	return parts[1]
}
