package e2e

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sidecar/internal/config"
)

func startControlPlane(t *testing.T, mesh *config.Source) *ControlPlane {
	t.Helper()
	cp, err := StartControlPlane(mesh, 0, quietLogger())
	if err != nil {
		t.Fatalf("start control plane: %v", err)
	}
	t.Cleanup(cp.Close)
	return cp
}

func startMeshSidecar(t *testing.T, cp *ControlPlane, service string, app *Echo) *MeshSidecar {
	t.Helper()
	sc, err := StartMeshSidecar(cp.Addr, service, app, quietLogger())
	if err != nil {
		t.Fatalf("start sidecar for %s: %v", service, err)
	}
	t.Cleanup(sc.Close)
	return sc
}

// eventually retries check until it passes or a few seconds have gone by.
// Discovery is asynchronous by nature; what these tests pin down is that it
// converges, and to what.
func eventually(t *testing.T, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// reaches is a check that a call to service through sc lands on want.
func reaches(sc *MeshSidecar, service, want string) func() error {
	return func() error {
		res, err := Call(sc.Addr, service, "/v1/hello")
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return fmt.Errorf("status %d, X-Sidecar-Error %q", res.StatusCode, res.Header.Get("X-Sidecar-Error"))
		}
		got, err := ReadReceived(res)
		if err != nil {
			return err
		}
		if got.Service != want {
			return fmt.Errorf("reached %q, want %q", got.Service, want)
		}
		return nil
	}
}

// failsWith is a check that a call to service through sc gets a sidecar error.
func failsWith(sc *MeshSidecar, service string, status int, code string) func() error {
	return func() error {
		res, err := Call(sc.Addr, service, "/v1/hello")
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != status || res.Header.Get("X-Sidecar-Error") != code {
			return fmt.Errorf("got %d %q, want %d %q", res.StatusCode, res.Header.Get("X-Sidecar-Error"), status, code)
		}
		return nil
	}
}

func adminStatus(sc *MeshSidecar, path string) int {
	res, err := http.Get("http://" + sc.Admin + path)
	if err != nil {
		return 0
	}
	res.Body.Close()
	return res.StatusCode
}

// Nothing lists service-b anywhere: its sidecar registers it, and A's sidecar
// learns of it from the control plane.
func TestDiscoveredRoute(t *testing.T) {
	cp := startControlPlane(t, config.Static(config.DefaultMesh()))

	b := startEcho(t, "service-b")
	startMeshSidecar(t, cp, "service-b", b)
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	eventually(t, "A reaches B by name", reaches(a, "service-b", "service-b"))
	if got := adminStatus(a, "/readyz"); got != http.StatusOK {
		t.Errorf("A /readyz = %d, want 200 once it routes", got)
	}
	if !a.Registrar.Registered() {
		t.Error("A's own instance is not registered")
	}
}

// Pods come and go; callers follow without anyone editing anything.
func TestScaleOutAndIn(t *testing.T) {
	cp := startControlPlane(t, config.Static(config.DefaultMesh()))
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	b1 := startEcho(t, "service-b#1")
	startMeshSidecar(t, cp, "service-b", b1)
	eventually(t, "A reaches b1", reaches(a, "service-b", "service-b#1"))

	b2 := startEcho(t, "service-b#2")
	b2Sidecar := startMeshSidecar(t, cp, "service-b", b2)
	eventually(t, "A reaches b2 after scale-out", reaches(a, "service-b", "service-b#2"))

	// Graceful shutdown deregisters, so b2 leaves every table at once rather
	// than when its lease runs out.
	b2Sidecar.Close()
	eventually(t, "b2 leaves A's table", func() error {
		for range 4 {
			if err := reaches(a, "service-b", "service-b#1")(); err != nil {
				return err
			}
		}
		return nil
	})
}

// An app that fails its health check stops receiving traffic, and comes back
// when it recovers.
func TestUnhealthyAppLeavesTheMesh(t *testing.T) {
	cp := startControlPlane(t, config.Static(config.DefaultMesh()))
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	b := startEcho(t, "service-b")
	bSidecar := startMeshSidecar(t, cp, "service-b", b)
	eventually(t, "A reaches B", reaches(a, "service-b", "service-b"))

	b.SetHealthy(false)
	// service-b is not in the mesh file, so with no instance it is unknown.
	eventually(t, "B leaves once unhealthy", failsWith(a, "service-b", http.StatusNotFound, "no_route"))
	if bSidecar.Registrar.Registered() {
		t.Error("B's registrar still claims a lease")
	}

	b.SetHealthy(true)
	eventually(t, "B returns once healthy", reaches(a, "service-b", "service-b"))
}

// A service the mesh file names exists even with nothing registered: callers
// are told it is down (503), not that it does not exist (404).
func TestDeclaredServiceWithNoInstances(t *testing.T) {
	mesh := config.DefaultMesh()
	mesh.Services = []config.ServicePolicy{{Name: "service-b", Policy: mesh.Defaults}}
	cp := startControlPlane(t, config.Static(mesh))
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	eventually(t, "declared service with no pods", failsWith(a, "service-b", http.StatusServiceUnavailable, "no_healthy_upstream"))
}

// Per-service policy lives in the control plane's file, and an edit reaches
// sidecars without restarting anything.
func TestPolicyFromMeshFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.yaml")
	write := func(timeout string) {
		t.Helper()
		body := "services:\n  - name: service-b\n    timeout: " + timeout + "\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("50ms")
	mesh, err := config.New(path, config.WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	cp := startControlPlane(t, mesh)

	b := StartSlowEcho("service-b", 300*time.Millisecond)
	t.Cleanup(b.Close)
	startMeshSidecar(t, cp, "service-b", b)
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	eventually(t, "50ms timeout from mesh.yaml", failsWith(a, "service-b", http.StatusGatewayTimeout, "deadline_exceeded"))

	write("2s")
	if err := mesh.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	eventually(t, "2s timeout after the edit", reaches(a, "service-b", "service-b"))
}

// Before its first snapshot a sidecar says it is not ready — to the app with
// 503 rather than a misleading 404, and to the kubelet on /readyz.
func TestNotReadyBeforeFirstSnapshot(t *testing.T) {
	cp, err := StartControlPlane(config.Static(config.DefaultMesh()), time.Hour, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cp.Close)
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))

	eventually(t, "call before any snapshot", failsWith(a, "service-b", http.StatusServiceUnavailable, "mesh_not_ready"))
	if got := adminStatus(a, "/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 before the first snapshot", got)
	}
	if got := adminStatus(a, "/healthz"); got != http.StatusOK {
		t.Errorf("/healthz = %d, want 200: not ready is not dead", got)
	}
	// Registration does not wait for snapshots: the control plane is
	// collecting instances while it warms up.
	eventually(t, "registered during warmup", func() error {
		if !a.Registrar.Registered() {
			return fmt.Errorf("not registered")
		}
		return nil
	})
}

// The control plane going away is survivable: sidecars keep routing on their
// last table, and a restarted control plane waits for re-registration before
// it serves a snapshot, instead of handing out an empty mesh.
func TestControlPlaneRestart(t *testing.T) {
	mesh := config.DefaultMesh()
	mesh.Registry.LeaseTTL = 3 * time.Second // heartbeat every second
	cp := startControlPlane(t, config.Static(mesh))

	b := startEcho(t, "service-b")
	startMeshSidecar(t, cp, "service-b", b)
	a := startMeshSidecar(t, cp, "service-a", startEcho(t, "service-a"))
	eventually(t, "A reaches B", reaches(a, "service-b", "service-b"))

	cp.Close()
	if err := reaches(a, "service-b", "service-b")(); err != nil {
		t.Fatalf("with the control plane down: %v", err)
	}

	restarted, err := StartControlPlaneOn(cp.Addr, config.Static(mesh), mesh.Registry.LeaseTTL, quietLogger())
	if err != nil {
		t.Fatalf("restart control plane on %s: %v", cp.Addr, err)
	}
	t.Cleanup(restarted.Close)

	// Throughout warmup, and after it, B stays routable: it re-registered
	// with the new control plane before that one served anything.
	deadline := time.Now().Add(mesh.Registry.LeaseTTL + time.Second)
	for time.Now().Before(deadline) {
		if err := reaches(a, "service-b", "service-b")(); err != nil {
			t.Fatalf("across the restart: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	eventually(t, "A picks up a snapshot from the new control plane", func() error {
		if v, want := a.Watcher.Current().Version, restarted.Server.Snapshot().Version; v != want {
			return fmt.Errorf("A is on %s, the new control plane on %s", v, want)
		}
		return nil
	})
}
