package accesslog

import "net/http"

// responseWriter records the status that reached the client. It stays 0 when
// nothing went through this writer: the client hung up, or the connection was
// hijacked for an upgrade, whose 101 goes straight to the socket.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	// 1xx is informational: net/http keeps the header open for the real status,
	// and ReverseProxy replays an upstream's 1xx through here.
	if w.status == 0 && status >= 200 {
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
// optional: ReverseProxy flushes streamed responses and hijacks upgrades through
// one. Without it, streaming silently buffers and upgrades fail outright.
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
