package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// events captures what a Source logs.
type events struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (e *events) Enabled(context.Context, slog.Level) bool { return true }
func (e *events) WithAttrs([]slog.Attr) slog.Handler       { return e }
func (e *events) WithGroup(string) slog.Handler            { return e }

func (e *events) Handle(_ context.Context, r slog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recs = append(e.recs, r.Clone())
	return nil
}

func (e *events) count(msg string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, r := range e.recs {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// attr returns the value of key on the last record with this message.
func (e *events) attr(msg, key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var val string
	for _, r := range e.recs {
		if r.Message == msg {
			r.Attrs(func(a slog.Attr) bool {
				if a.Key == key {
					val = a.Value.String()
				}
				return true
			})
		}
	}
	return val
}

// testFile sets mtimes explicitly, so even a fast rewrite counts as a change.
type testFile struct {
	t    *testing.T
	path string
	gen  int
}

func newTestFile(t *testing.T, body string) *testFile {
	f := &testFile{t: t, path: filepath.Join(t.TempDir(), "sidecar.yaml")}
	f.write(body)
	return f
}

// write replaces the file atomically. Writing in place would let a concurrent
// stat see new content before its mtime is set, counting one edit as two.
func (f *testFile) write(body string) {
	f.t.Helper()
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		f.t.Fatalf("write config: %v", err)
	}
	f.gen++
	mod := time.Date(2026, 1, 1, 0, 0, f.gen, 0, time.UTC)
	if err := os.Chtimes(tmp, mod, mod); err != nil {
		f.t.Fatalf("set mtime: %v", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		f.t.Fatalf("replace config: %v", err)
	}
}

func withTimeout(d string) string {
	return fmt.Sprintf("services:\n  - name: orders-svc\n    instances: [\"10.0.0.7:15000\"]\n    timeout: %s\n", d)
}

func newSource(t *testing.T, f *testFile, opts ...Option) (*Source, *events) {
	t.Helper()
	ev := &events{}
	s, err := New(f.path, append([]Option{WithLogger(slog.New(ev))}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, ev
}

func changes(s *Source) func() []*Config {
	var mu sync.Mutex
	var got []*Config
	s.OnChange(func(_, next *Config) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, next)
	})
	return func() []*Config {
		mu.Lock()
		defer mu.Unlock()
		return append([]*Config(nil), got...)
	}
}

func TestNewLoadsAndValidates(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, ev := newSource(t, f)

	if got := s.Current().Services[0].Timeout; got != 2*time.Second {
		t.Errorf("timeout = %v, want 2s", got)
	}
	if ev.count("config_loaded") != 1 {
		t.Errorf("config_loaded logged %d times, want 1", ev.count("config_loaded"))
	}
}

func TestNewRejectsInvalidFile(t *testing.T) {
	f := newTestFile(t, "services:\n  - name: BAD\n")
	if _, err := New(f.path, WithLogger(slog.New(&events{}))); err == nil {
		t.Fatal("New succeeded on an invalid file")
	}
}

func TestReloadSwapsAndNotifies(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, _ := newSource(t, f)
	before := s.Current()

	var gotOld, gotNext *Config
	s.OnChange(func(old, next *Config) { gotOld, gotNext = old, next })

	f.write(withTimeout("3s"))
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if got := s.Current().Services[0].Timeout; got != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", got)
	}
	if gotOld != before || gotNext != s.Current() {
		t.Error("OnChange did not receive the previous and the new config")
	}
	// A snapshot taken before the reload is untouched by it.
	if got := before.Services[0].Timeout; got != 2*time.Second {
		t.Errorf("old snapshot timeout = %v, want 2s", got)
	}
}

func TestReloadKeepsPreviousOnError(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid file", body: "services:\n  - name: BAD\n"},
		{name: "not yaml", body: "services: [oops\n"},
		{name: "instance with a scheme", body: "services:\n  - name: orders-svc\n    instances: [\"http://10.0.0.7:15000\"]\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFile(t, withTimeout("2s"))
			s, ev := newSource(t, f)
			before := s.Current()
			got := changes(s)

			f.write(tc.body)
			if err := s.Reload(); err == nil {
				t.Fatal("Reload succeeded, want an error")
			}

			if s.Current() != before {
				t.Error("Current changed after a rejected reload")
			}
			if len(got()) != 0 {
				t.Error("OnChange ran for a rejected reload")
			}
			if ev.count("config_rejected") != 1 {
				t.Errorf("config_rejected logged %d times, want 1", ev.count("config_rejected"))
			}
		})
	}
}

// Restart-only fields keep their running values; the rest of the edit applies.
func TestReloadKeepsRestartOnlyFields(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, ev := newSource(t, f)

	f.write("listeners:\n  outbound: \"127.0.0.1:25001\"\nreload:\n  interval: 1s\n" + withTimeout("3s"))
	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	c := s.Current()
	if got := c.Services[0].Timeout; got != 3*time.Second {
		t.Errorf("timeout = %v, want 3s (reloadable, should apply)", got)
	}
	if got := c.Listeners.Outbound; got != "127.0.0.1:15001" {
		t.Errorf("listeners.outbound = %q, want the running 127.0.0.1:15001", got)
	}
	if got := c.Reload.Interval; got != 2*time.Second {
		t.Errorf("reload.interval = %v, want the running 2s", got)
	}

	fields := ev.attr("config_restart_required", "fields")
	for _, want := range []string{"listeners.outbound", "reload.interval"} {
		if !strings.Contains(fields, want) {
			t.Errorf("config_restart_required fields = %s, want it to name %s", fields, want)
		}
	}
}

// The loop guard must check the inbound address in effect, not the one requested.
func TestReloadRevalidatesAfterKeepingRestartOnlyFields(t *testing.T) {
	f := newTestFile(t, "listeners:\n  inbound: \"127.0.0.1:15000\"\n"+withTimeout("2s"))
	s, _ := newSource(t, f)

	f.write(`listeners:
  inbound: "127.0.0.1:16000"
services:
  - name: orders-svc
    instances: ["127.0.0.1:15000"]
`)
	if err := s.Reload(); err == nil || !strings.Contains(err.Error(), "own inbound address") {
		t.Fatalf("Reload error = %v, want the loop guard against the running inbound address", err)
	}
}

// manualTicks drives Watch by hand. The second send proves the first tick was
// handled; the second itself may still be in flight when tick returns.
func manualTicks(s *Source) func() {
	ch := make(chan time.Time)
	s.tick = func(time.Duration) (<-chan time.Time, func()) { return ch, func() {} }
	return func() {
		ch <- time.Time{}
		ch <- time.Time{}
	}
}

func watch(t *testing.T, s *Source) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Watch(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestWatchReloadsOnlyOnChange(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, ev := newSource(t, f)
	tick := manualTicks(s)
	got := changes(s)
	watch(t, s)

	tick()
	if len(got()) != 0 || ev.count("config_loaded") != 1 {
		t.Fatal("Watch reloaded a file that had not changed")
	}

	f.write(withTimeout("3s"))
	tick()
	if len(got()) != 1 {
		t.Fatalf("OnChange ran %d times after one change, want 1", len(got()))
	}
	if timeout := s.Current().Services[0].Timeout; timeout != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", timeout)
	}
}

func TestWatchRejectsBrokenFileOnce(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, ev := newSource(t, f)
	tick := manualTicks(s)
	watch(t, s)

	f.write("services: [oops\n")
	tick()
	tick()
	tick()
	if n := ev.count("config_rejected"); n != 1 {
		t.Errorf("config_rejected logged %d times over three ticks, want 1", n)
	}

	f.write(withTimeout("3s"))
	tick()
	if timeout := s.Current().Services[0].Timeout; timeout != 3*time.Second {
		t.Errorf("timeout = %v after the fix, want 3s", timeout)
	}
}

// Editors that save by rename leave the path briefly missing.
func TestWatchSurvivesMissingFile(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, ev := newSource(t, f)
	tick := manualTicks(s)
	watch(t, s)

	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	tick()
	tick()
	if n := ev.count("config_rejected"); n != 1 {
		t.Errorf("config_rejected logged %d times while missing, want 1", n)
	}
	if s.Current().Services[0].Timeout != 2*time.Second {
		t.Error("a missing file replaced the running config")
	}

	f.write(withTimeout("3s"))
	tick()
	if timeout := s.Current().Services[0].Timeout; timeout != 3*time.Second {
		t.Errorf("timeout = %v after the file returned, want 3s", timeout)
	}
}

func TestWatchDisabled(t *testing.T) {
	f := newTestFile(t, "reload:\n  interval: 0s\n"+withTimeout("2s"))
	s, _ := newSource(t, f)

	done := make(chan struct{})
	go func() {
		s.Watch(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch kept running with reload.interval: 0")
	}
}

// Readers never block or tear while reloads land; run with -race.
func TestCurrentDuringReloads(t *testing.T) {
	f := newTestFile(t, withTimeout("2s"))
	s, _ := newSource(t, f)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				c := s.Current()
				if d := c.Services[0].Timeout; d != 2*time.Second && d != 3*time.Second {
					t.Errorf("read timeout %v, want 2s or 3s", d)
					return
				}
			}
		}()
	}

	for i := range 50 {
		f.write(withTimeout([]string{"2s", "3s"}[i%2]))
		if err := s.Reload(); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
