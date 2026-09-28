// Package reqctx owns the headers that carry request context between hops:
// correlation ids and the two deadline forms (DESIGN §3.2, §4, §5.3).
package reqctx

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

const (
	HeaderRequestID   = "X-Request-Id"
	HeaderTraceparent = "Traceparent"
	HeaderDeadline    = "X-Sidecar-Deadline"   // absolute unix ms, within one host
	HeaderTimeoutMs   = "X-Request-Timeout-Ms" // remaining ms, on the wire between sidecars
)

// A caller's id ends up in our log field, so it is bounded and printable.
const maxRequestIDLen = 128

// RequestID keeps the caller's id when it is usable, else mints one.
func RequestID(id string) string {
	if id != "" && len(id) <= maxRequestIDLen && !strings.ContainsFunc(id, unprintable) {
		return id
	}
	return randomHex(16)
}

// Traceparent continues the caller's trace with a fresh span for this hop, and
// starts a new trace when there is nothing valid to continue.
func Traceparent(header string) string {
	trace, flags := parse(header)
	if trace == "" {
		// A new trace that nothing samples is a trace nobody will ever look at.
		trace, flags = randomHex(16), "01"
	}
	return "00-" + trace + "-" + randomHex(8) + "-" + flags
}

// TraceID is the trace-id to log, "" when the header holds nothing usable.
func TraceID(header string) string {
	trace, _ := parse(header)
	return trace
}

// Timeout reads the remaining budget a caller sent on the wire.
func Timeout(header string) (time.Duration, bool) {
	ms, err := strconv.Atoi(header)
	if err != nil || ms <= 0 || int64(ms) > maxTimeoutMillis {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// maxTimeoutMillis is the largest millisecond value that can be represented
// by time.Duration without overflowing during conversion.
const maxTimeoutMillis = int64((1<<63 - 1) / int64(time.Millisecond))

// Deadline formats an absolute deadline for the local app; both sides of the
// loopback read the same clock, so absolute is safe here and skew-free budgets
// stay on the wire.
func Deadline(t time.Time) string {
	return strconv.FormatInt(t.UnixMilli(), 10)
}

// parse returns the trace-id and flags of a W3C traceparent
// (version-traceid-spanid-flags). Every field is checked, so a half-broken
// header is not half-trusted.
func parse(traceparent string) (trace, flags string) {
	parts := strings.Split(traceparent, "-")
	if len(parts) < 4 {
		return "", ""
	}
	if !isHex(parts[0], 2) || !isHex(parts[1], 32) || !isHex(parts[2], 16) || !isHex(parts[3], 2) {
		return "", ""
	}
	if allZero(parts[1]) || allZero(parts[2]) {
		return "", ""
	}
	return parts[1], parts[3]
}

func isHex(s string, n int) bool {
	return len(s) == n && !strings.ContainsFunc(s, notLowerHex)
}

func allZero(s string) bool { return strings.Trim(s, "0") == "" }

func notLowerHex(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }

func unprintable(r rune) bool { return r < 0x20 || r > 0x7e }

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
