package accesslog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
)

func TestResponseWriterStatus(t *testing.T) {
	tests := []struct {
		name string
		do   func(w http.ResponseWriter)
		want int
	}{
		{"explicit status", func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }, 503},
		{"implicit 200 on first write", func(w http.ResponseWriter) { w.Write([]byte("hello")) }, 200},
		// A client that hung up before anything was written: 0, not a made-up 200.
		{"nothing written", func(http.ResponseWriter) {}, 0},
		{"first status wins", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			w.WriteHeader(http.StatusOK)
		}, 404},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := &responseWriter{ResponseWriter: httptest.NewRecorder()}

			tc.do(w)

			if got := w.Status(); got != tc.want {
				t.Errorf("Status() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Without Unwrap, http.ResponseController cannot reach the real writer, and
// httputil.ReverseProxy loses response flushing and protocol upgrades with it.
func TestResponseWriterUnwrapKeepsFlushing(t *testing.T) {
	w := &responseWriter{ResponseWriter: httptest.NewRecorder()}

	if err := http.NewResponseController(w).Flush(); err != nil {
		t.Errorf("Flush() = %v, want nil — is Unwrap still there?", err)
	}
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := traceID(tc.traceparent); got != tc.want {
				t.Errorf("traceID(%q) = %q, want %q", tc.traceparent, got, tc.want)
			}
		})
	}
}

// emit writes rec and decodes the line the way a log consumer reads it.
func emit(t *testing.T, rec *Record) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	rec.Emit(slog.New(slog.NewJSONHandler(&buf, nil)))

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("access line is not JSON: %v\n%s", err, buf.String())
	}
	return line
}

func TestEmitFields(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/orders/42", nil)
	r.Header.Set("X-Request-Id", "7f3a")
	r.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")

	rec, w := Start(httptest.NewRecorder(), Outbound, r)
	rec.Target = "orders-svc"
	rec.Deadline = 2600 * time.Millisecond
	rec.Attempt(&url.URL{Scheme: "http", Host: "10.0.0.7:15000"})
	rec.Attempt(&url.URL{Scheme: "http", Host: "10.0.0.8:15000"})
	w.WriteHeader(http.StatusOK)

	line := emit(t, rec)

	// Numbers arrive as float64 from encoding/json, whatever slog wrote.
	want := map[string]any{
		"msg":          "access",
		"dir":          "outbound",
		"requestId":    "7f3a",
		"traceId":      "4bf92f3577b34da6a3ce929d0e0e4736",
		"method":       http.MethodPost,
		"target":       "orders-svc",
		"path":         "/v1/orders/42",
		"status":       float64(200),
		"attempts":     float64(2),
		"deadlineMs":   float64(2600),
		"sidecarError": "",
	}
	for key, want := range want {
		if got := line[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}

	// One entry per attempt, in the order they were tried.
	if got := line["instances"]; !reflect.DeepEqual(got, []any{"10.0.0.7:15000", "10.0.0.8:15000"}) {
		t.Errorf("instances = %#v, want both instances in order", got)
	}
	if _, ok := line["durationMs"].(float64); !ok {
		t.Errorf("durationMs = %#v, want a number", line["durationMs"])
	}
}

// sidecarError comes off the header writeError already sets, so the proxy needs
// no second channel to report it.
func TestEmitReadsSidecarErrorFromHeader(t *testing.T) {
	rec, w := Start(httptest.NewRecorder(), Outbound, httptest.NewRequest(http.MethodGet, "/", nil))
	w.Header().Set("X-Sidecar-Error", "no_route")
	w.WriteHeader(http.StatusNotFound)

	line := emit(t, rec)

	if got := line["sidecarError"]; got != "no_route" {
		t.Errorf("sidecarError = %#v, want no_route", got)
	}
	if got := line["status"]; got != float64(404) {
		t.Errorf("status = %#v, want 404", got)
	}
}

// A request that never reached an instance still logs an array, so no consumer
// has to special-case null.
func TestEmitLogsEmptyInstancesAsArray(t *testing.T) {
	rec, _ := Start(httptest.NewRecorder(), Outbound, httptest.NewRequest(http.MethodGet, "/", nil))

	line := emit(t, rec)

	if got := line["instances"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("instances = %#v, want []", got)
	}
	for _, key := range []string{"attempts", "status"} {
		if got := line[key]; got != float64(0) {
			t.Errorf("%s = %#v, want 0", key, got)
		}
	}
}
