package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"time"

	"sidecar/internal/routing"
)

var ErrNoHealthyUpstream = errors.New("no healthy upstream")

const (
	backoffBase = 25 * time.Millisecond
	backoffMax  = 250 * time.Millisecond

	// Don't start an attempt that cannot plausibly finish.
	minAttemptTime = 20 * time.Millisecond

	// Error bodies of discarded attempts are read this far to keep the
	// connection reusable, and no further.
	drainLimit = 64 << 10
)

// attemptTripper turns one proxied request into up to MaxAttempts attempts
// against different instances. It sits at the transport layer because that is
// the last point where a response is still a value in hand: nothing has been
// written to the client yet, so a failed attempt can be discarded for free.
type attemptTripper struct {
	svc  *routing.Service
	base http.RoundTripper
	log  *slog.Logger
}

func (t *attemptTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// A lap resets tried, and there are only MaxAttempts picks, so it never
	// holds more than this many entries.
	tried := make([]*url.URL, 0, min(t.svc.MaxAttempts, t.svc.InstanceCount()))

	for attempt := 1; ; attempt++ {
		// Every instance has had a turn, so start a fresh lap. Spreading
		// attempts across instances first is a preference, not a hard limit:
		// MaxAttempts is the only cap, and a single-instance service would
		// otherwise never get a second try.
		if len(tried) >= t.svc.InstanceCount() {
			tried = tried[:0]
		}

		target, ok := t.svc.Pick(tried)
		if !ok {
			return nil, ErrNoHealthyUpstream
		}
		tried = append(tried, target)

		out, err := t.request(req, target)
		if err != nil {
			return nil, err
		}

		res, err := t.base.RoundTrip(out)
		retriable, requestSent := classify(res, err)
		if !retriable || !eligible(req, requestSent) {
			return res, err
		}

		delay := backoff(attempt)
		if attempt >= t.svc.MaxAttempts || !hasTimeFor(req.Context(), delay) {
			return res, err
		}

		t.log.Debug("retrying", "attempt", attempt, "instance", target.Host,
			"status", statusOf(res), "err", err, "backoff", delay.String())

		// Committed to another attempt, so this result is dropped.
		drain(res)
		if !sleep(req.Context(), delay) {
			return nil, req.Context().Err()
		}
	}
}

// request builds one attempt. The RoundTripper contract forbids mutating the
// request we were handed, and a replayed body must be a fresh reader.
func (t *attemptTripper) request(req *http.Request, target *url.URL) (*http.Request, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme = target.Scheme
	out.URL.Host = target.Host
	// An empty Host makes net/http derive it from URL.Host, which is what
	// ProxyRequest.SetURL did before instance choice moved in here.
	out.Host = ""

	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		out.Body = body
	}
	return out, nil
}

// classify reports whether the attempt failed in a way worth repeating, and
// whether the request had already reached the upstream when it failed.
func classify(res *http.Response, err error) (retriable, requestSent bool) {
	if err != nil {
		// The caller gave up or the overall deadline passed; there is no time
		// left to spend on another instance.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, true
		}
		// A failed dial means the request provably never left this machine.
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			return true, false
		}
		return true, true
	}

	switch res.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true, true
	}
	// 500 is an application bug and 429 is a request to slow down: repeating
	// either is useless at best.
	return false, true
}

// eligible reports whether this request may be sent a second time at all.
func eligible(req *http.Request, requestSent bool) bool {
	// Streamed or oversized bodies are not buffered, so there is nothing to replay.
	if req.Body != nil && req.GetBody == nil {
		return false
	}
	if !requestSent {
		return true
	}
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// backoff is exponential with full jitter, which spreads retries out instead of
// letting every client hit a recovering upstream on the same tick.
func backoff(attempt int) time.Duration {
	if attempt > 8 {
		attempt = 8 // keep the shift from overflowing
	}
	d := backoffBase << (attempt - 1)
	if d > backoffMax {
		d = backoffMax
	}
	return rand.N(d)
}

func hasTimeFor(ctx context.Context, delay time.Duration) bool {
	deadline, ok := ctx.Deadline()
	if !ok {
		return true
	}
	return time.Until(deadline) > delay+minAttemptTime
}

func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func drain(res *http.Response) {
	if res == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, drainLimit))
	res.Body.Close()
}

func statusOf(res *http.Response) int {
	if res == nil {
		return 0
	}
	return res.StatusCode
}
