package e2e

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"sidecar/internal/config"
	"sidecar/internal/reqctx"
)

// accessLines collects one sidecar's access lines, so a test can read what it
// said about a request it served.
type accessLines struct {
	mu    sync.Mutex
	lines []map[string]any
}

func (a *accessLines) logger() *slog.Logger { return slog.New(a) }

func (a *accessLines) Enabled(context.Context, slog.Level) bool { return true }

func (a *accessLines) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "access" {
		return nil
	}
	line := make(map[string]any, r.NumAttrs())
	r.Attrs(func(at slog.Attr) bool {
		line[at.Key] = at.Value.Any()
		return true
	})

	a.mu.Lock()
	a.lines = append(a.lines, line)
	a.mu.Unlock()
	return nil
}

func (a *accessLines) WithAttrs([]slog.Attr) slog.Handler { return a }
func (a *accessLines) WithGroup(string) slog.Handler      { return a }

func (a *accessLines) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lines = nil
}

// only returns the one line for dir, failing when there is not exactly one.
func (a *accessLines) only(t *testing.T, dir string) map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()

	var found []map[string]any
	for _, line := range a.lines {
		if line["dir"] == dir {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s access lines, want exactly 1", len(found), dir)
	}
	return found[0]
}

func startMeshSidecarLogging(t *testing.T, cp *ControlPlane, service string, app *Echo, log *accessLines) *MeshSidecar {
	t.Helper()
	sc, err := StartMeshSidecar(cp.Addr, service, app, log.logger())
	if err != nil {
		t.Fatalf("start sidecar for %s: %v", service, err)
	}
	t.Cleanup(sc.Close)
	return sc
}

func str(t *testing.T, line map[string]any, key string) string {
	t.Helper()
	s, ok := line[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want a string", key, line[key])
	}
	return s
}

// A request now crosses two sidecars: web's on the way out and orders-svc's on
// the way in. The app is reached only through the second one.
func TestRequestCrossesBothSidecars(t *testing.T) {
	cp := startControlPlane(t, config.Static(config.DefaultMesh()))

	orders, ordersLog := startEcho(t, "orders-svc"), &accessLines{}
	ordersSc := startMeshSidecarLogging(t, cp, "orders-svc", orders, ordersLog)

	web, webLog := startEcho(t, "web"), &accessLines{}
	webSc := startMeshSidecarLogging(t, cp, "web", web, webLog)

	eventually(t, "web can reach orders-svc", reaches(webSc, "orders-svc", "orders-svc"))

	t.Run("the mesh points at the sidecar, not the app", func(t *testing.T) {
		if got := ordersSc.Registrar.Registration().Address; got != ordersSc.Inbound {
			t.Errorf("registered %q, want its inbound listener %q", got, ordersSc.Inbound)
		}
		if ordersSc.Inbound == orders.Addr {
			t.Errorf("inbound listener is the app's own address %q", orders.Addr)
		}
	})

	t.Run("context an app forwards survives the hop and lands in both lines", func(t *testing.T) {
		webLog.reset()
		ordersLog.reset()

		const id = "7f3a2b1c"
		res, err := CallWith(webSc.Addr, "orders-svc", "/v1/orders/42",
			http.Header{reqctx.HeaderRequestID: {id}})
		if err != nil {
			t.Fatalf("call orders-svc: %v", err)
		}
		got, err := ReadReceived(res)
		if err != nil {
			t.Fatal(err)
		}

		if got.RequestID != id {
			t.Errorf("app saw request id %q, want %q", got.RequestID, id)
		}
		// Stamped by inbound for the app to forward on its own calls (§4).
		if got.Deadline == "" {
			t.Error("app saw no X-Sidecar-Deadline")
		}
		if reqctx.TraceID(got.Traceparent) == "" {
			t.Errorf("app saw traceparent %q, want a usable one", got.Traceparent)
		}

		// The payoff: one request, two lines, one id to join them on.
		if got := str(t, webLog.only(t, "outbound"), "requestId"); got != id {
			t.Errorf("web's outbound line says requestId %q, want %q", got, id)
		}
		if got := str(t, ordersLog.only(t, "inbound"), "requestId"); got != id {
			t.Errorf("orders-svc's inbound line says requestId %q, want %q", got, id)
		}
	})

	t.Run("a caller with no context gets one minted", func(t *testing.T) {
		webLog.reset()
		ordersLog.reset()

		res, err := Call(webSc.Addr, "orders-svc", "/v1/orders/42")
		if err != nil {
			t.Fatalf("call orders-svc: %v", err)
		}
		got, err := ReadReceived(res)
		if err != nil {
			t.Fatal(err)
		}

		// Outbound only passes context through; the first inbound in a chain is
		// what mints it, which is why web's line has nothing to report here.
		if got.RequestID == "" {
			t.Error("app saw no request id")
		}
		if line := str(t, ordersLog.only(t, "inbound"), "requestId"); line != got.RequestID {
			t.Errorf("inbound line says requestId %q, app saw %q", line, got.RequestID)
		}
	})
}
