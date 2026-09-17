package proxy

import (
	"encoding/json"
	"net/http"
)

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Sidecar-Error", code)
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"code": code, "message": msg})
}
