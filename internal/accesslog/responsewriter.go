package accesslog

import "net/http"

// responseWriter records the status that reached the client. It stays 0 when
// nothing did, which is what a client hanging up mid-request looks like — not
// a 200.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	// net/http ignores a second WriteHeader, so the first one is the truth.
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // the implicit 200 net/http would send
	}
	return w.ResponseWriter.Write(b)
}

func (w *responseWriter) Status() int { return w.status }

// Unwrap is how http.ResponseController reaches the real writer, and it is not
// optional: httputil.ReverseProxy flushes streamed responses through a
// ResponseController and hijacks through it on a protocol upgrade. Drop this
// method and streaming responses silently start buffering, while upgrades fail
// outright with "can't switch protocols using non-Hijacker ResponseWriter type".
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
