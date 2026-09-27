package reqctx

import (
	"strings"
	"testing"
	"time"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		keep bool
	}{
		{"usable", "7f3a2b1c", true},
		{"absent", "", false},
		{"too long", strings.Repeat("a", maxRequestIDLen+1), false},
		{"at the limit", strings.Repeat("a", maxRequestIDLen), true},
		{"control characters", "id\nwith-a-newline", false},
		{"non-ascii", "idą", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := RequestID(tc.id)

			if tc.keep {
				if got != tc.id {
					t.Errorf("RequestID(%q) = %q, want it kept", tc.id, got)
				}
				return
			}
			if got == tc.id {
				t.Errorf("RequestID(%q) kept it, want a fresh one", tc.id)
			}
			if !isHex(got, 32) {
				t.Errorf("generated %q, want 32 hex digits", got)
			}
		})
	}
}

func TestTraceparent(t *testing.T) {
	const caller = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	t.Run("continues a valid trace with a new span", func(t *testing.T) {
		got := Traceparent(caller)

		if TraceID(got) != TraceID(caller) {
			t.Errorf("trace-id = %q, want the caller's %q", TraceID(got), TraceID(caller))
		}
		if span(t, got) == span(t, caller) {
			t.Errorf("span-id = %q, want a new one for this hop", span(t, got))
		}
	})

	t.Run("starts a new trace when there is nothing to continue", func(t *testing.T) {
		for _, header := range []string{"", "garbage", "not-a-valid-header"} {
			got := Traceparent(header)

			if TraceID(got) == "" {
				t.Errorf("Traceparent(%q) = %q, want a usable traceparent", header, got)
			}
			// Sampled, or the trace is generated and then never looked at.
			if flags := field(t, got, 3); flags != "01" {
				t.Errorf("flags = %q, want 01", flags)
			}
		}
	})

	t.Run("two hops never share a span", func(t *testing.T) {
		if a, b := Traceparent(caller), Traceparent(caller); span(t, a) == span(t, b) {
			t.Errorf("both hops got span %q", span(t, a))
		}
	})
}

func TestTraceID(t *testing.T) {
	tests := []struct {
		name        string
		traceparent string
		want        string
	}{
		{"valid", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "4bf92f3577b34da6a3ce929d0e0e4736"},
		{"absent", "", ""},
		{"not a traceparent", "garbage", ""},
		{"truncated", "00-4bf92f3577b34da6a3ce929d0e0e4736", ""},
		// Four fields are not enough: whatever sits in the second one would end up
		// in the correlation field, so it has to look like a trace id too.
		{"four fields of junk", "not-a-valid-header", ""},
		{"trace-id too short", "00-4bf92f3577b34da6a3ce929d0e0e473-00f067aa0ba902b7-01", ""},
		{"uppercase hex", "00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01", ""},
		{"trace-id all zeroes", "00-00000000000000000000000000000000-00f067aa0ba902b7-01", ""},
		// A broken span-id is not a reason to half-trust the rest.
		{"span-id all zeroes", "00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", ""},
		{"span-id too short", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b-01", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraceID(tc.traceparent); got != tc.want {
				t.Errorf("TraceID(%q) = %q, want %q", tc.traceparent, got, tc.want)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	tests := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"2600", 2600 * time.Millisecond, true},
		{"", 0, false},
		{"soon", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"1.5", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.header, func(t *testing.T) {
			got, ok := Timeout(tc.header)
			if ok != tc.ok || got != tc.want {
				t.Errorf("Timeout(%q) = %v, %v; want %v, %v", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestDeadline(t *testing.T) {
	at := time.UnixMilli(1789200000123)

	if got := Deadline(at); got != "1789200000123" {
		t.Errorf("Deadline() = %q, want unix ms", got)
	}
}

func field(t *testing.T, traceparent string, i int) string {
	t.Helper()
	parts := strings.Split(traceparent, "-")
	if len(parts) < 4 {
		t.Fatalf("traceparent %q has %d fields, want 4", traceparent, len(parts))
	}
	return parts[i]
}

func span(t *testing.T, traceparent string) string {
	t.Helper()
	return field(t, traceparent, 2)
}
