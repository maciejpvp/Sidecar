package proxy

import (
	"encoding/json"
	"net/http"

	"sidecar/internal/accesslog"
)

// writeError is the only place this sidecar answers for itself, so it is where
// the access line learns the code — the header also carries upstream codes (§7).
func writeError(w http.ResponseWriter, rec *accesslog.Record, status int, code, msg string) {
	rec.Fail(code)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Sidecar-Error", code)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}
