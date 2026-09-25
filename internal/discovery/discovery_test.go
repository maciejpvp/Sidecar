package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sidecar/internal/config"
	"sidecar/internal/meshapi"
)

// A snapshot that fails validation is not applied, and the table in effect
// stays; the next good one is applied as usual.
func TestWatcherRejectsBadSnapshot(t *testing.T) {
	good1 := meshapi.Snapshot{Version: "v1", Services: []config.Service{config.NewService("svc", "10.0.0.1:15000")}}
	bad := meshapi.Snapshot{Version: "v2", Services: []config.Service{config.NewService("svc", "http://10.0.0.1:15000")}}
	good3 := meshapi.Snapshot{Version: "v3", Services: []config.Service{config.NewService("svc", "10.0.0.3:15000")}}

	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := map[string]meshapi.Snapshot{"": good1, "v1": bad, "v2": good3}
		snap, ok := next[r.URL.Query().Get("version")]
		if !ok {
			<-r.Context().Done() // v3 is the last; hold the poll like the real thing
			return
		}
		json.NewEncoder(w).Encode(snap)
	}))
	defer cp.Close()

	var mu sync.Mutex
	var applied []string
	w := NewWatcher(NewClient(strings.TrimPrefix(cp.URL, "http://")), slog.New(slog.DiscardHandler), func(s []config.Service) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, s[0].Instances[0])
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for w.Current() == nil || w.Current().Version != "v3" {
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not reach v3; current %+v", w.Current())
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(applied) != 2 || applied[0] != "10.0.0.1:15000" || applied[1] != "10.0.0.3:15000" {
		t.Errorf("applied %v, want v1 then v3 and never the invalid v2", applied)
	}
	select {
	case <-w.Ready():
	default:
		t.Error("Ready not closed after the first snapshot")
	}
}

func TestHTTPHealth(t *testing.T) {
	status := http.StatusOK
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
	}))
	defer app.Close()
	addr := strings.TrimPrefix(app.URL, "http://")
	ctx := context.Background()

	for _, tc := range []struct {
		status  int
		healthy bool
	}{{200, true}, {204, true}, {302, true}, {404, false}, {503, false}} {
		status = tc.status
		if err := HTTPHealth(addr, "/healthz")(ctx); (err == nil) != tc.healthy {
			t.Errorf("status %d: healthy = %v, want %v (err %v)", tc.status, err == nil, tc.healthy, err)
		}
	}

	app.Close()
	if err := HTTPHealth(addr, "/healthz")(ctx); err == nil {
		t.Error("an app that is not listening is healthy")
	}
	if err := HTTPHealth(addr, "")(ctx); err != nil {
		t.Errorf("no health path should mean always healthy, got %v", err)
	}
}
