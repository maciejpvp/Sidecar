// Command demo runs the two-service example end to end: service A calls B
// through the sidecar, then calls a service too slow to answer in time.
//
//	go run ./e2e/demo
//	go run ./e2e/demo -hold    # leave the sidecar up so you can curl it
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sidecar/e2e"
	"sidecar/internal/logging"
	"sidecar/internal/routing"
)

// Loopback only (DESIGN §2.1).
const outboundAddr = "127.0.0.1:15001"

func main() {
	hold := flag.Bool("hold", false, "keep the sidecar running until Ctrl+C so you can send your own requests")
	flag.Parse()

	if err := run(*hold); err != nil {
		slog.Error("demo failed", "error", err)
		os.Exit(1)
	}
}

func run(hold bool) error {
	var level slog.LevelVar
	level.Set(slog.LevelDebug)
	logger := logging.New(&level)
	slog.SetDefault(logger)

	serviceB := e2e.StartEcho("service-b")
	defer serviceB.Close()
	step(1, "service-b listening on %s", serviceB.Addr)

	slowService := e2e.StartSlowEcho("slow-service", 10*time.Second)
	defer slowService.Close()
	step(2, "slow-service listening on %s, and always takes 10s to answer", slowService.Addr)

	sidecar, err := e2e.StartSidecar(outboundAddr, map[string]routing.ServiceConfig{
		"service-b":    {Instances: []string{serviceB.Addr}},
		"slow-service": {Instances: []string{slowService.Addr}, Timeout: 200 * time.Millisecond},
	}, logger)
	if err != nil {
		return err
	}
	defer sidecar.Close()
	step(3, "sidecar (outbound) listening on %s, routing service-b and slow-service", sidecar.Addr)

	step(4, "service-a calls GET /v1/hello with Host: service-b")
	res, err := e2e.Call(sidecar.Addr, "service-b", "/v1/hello")
	if err != nil {
		return fmt.Errorf("service-a could not reach service-b: %w", err)
	}

	got, err := e2e.ReadReceived(res)
	if err != nil {
		return err
	}
	step(5, "service-a got %s", res.Status)
	fmt.Printf("    service-b received:  %s %s\n", got.Method, got.Path)
	fmt.Printf("    upstream Host:       %s   (rewritten by the sidecar from %q)\n", got.Host, "service-b")
	fmt.Printf("    X-Forwarded-For:     %s   (added by the sidecar)\n", got.XForwardedFor)
	fmt.Printf("    X-Forwarded-Proto:   %s\n", got.XForwardedProto)

	if res.StatusCode != 200 || got.Service != "service-b" {
		return fmt.Errorf("round trip did not reach service-b: status %s, service %q", res.Status, got.Service)
	}

	step(6, "service-a calls slow-service, whose timeout is 200ms")
	start := time.Now()
	res, err = e2e.Call(sidecar.Addr, "slow-service", "/v1/hello")
	if err != nil {
		return fmt.Errorf("service-a could not reach slow-service: %w", err)
	}
	slow, err := e2e.ReadSidecarError(res)
	if err != nil {
		return err
	}
	step(7, "service-a got %s after %v", res.Status, time.Since(start).Round(time.Millisecond))
	fmt.Printf("    X-Sidecar-Error:     %s\n", res.Header.Get("X-Sidecar-Error"))
	fmt.Printf("    body:                %s\n", slow.Message)
	fmt.Printf("    (slow-service is still working on it; nobody is waiting)\n")

	if res.StatusCode != 504 || slow.Code != "deadline_exceeded" {
		return fmt.Errorf("slow-service should have timed out: status %s, code %q", res.Status, slow.Code)
	}

	fmt.Printf("\n  The same requests by hand:\n    curl -is -H 'Host: service-b' http://%s/v1/hello\n", sidecar.Addr)
	fmt.Printf("  A service that answers too late (504 deadline_exceeded):\n    curl -is -H 'Host: slow-service' http://%s/v1/hello\n", sidecar.Addr)
	fmt.Printf("  An unknown service (404 no_route):\n    curl -is -H 'Host: nope' http://%s/v1/hello\n\n", sidecar.Addr)

	if hold {
		fmt.Println("  Holding. Ctrl+C to stop.")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		fmt.Println()
	}
	return nil
}

func step(n int, format string, args ...any) {
	fmt.Printf("\n  [%d] %s\n", n, fmt.Sprintf(format, args...))
}
