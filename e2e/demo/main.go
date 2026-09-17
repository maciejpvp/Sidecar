// Command demo runs a two-service example end to end: service B listens on its
// own port, service A calls it by name through the sidecar, and the round trip
// is printed alongside the sidecar's real JSON logs.
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

	"sidecar/e2e"
	"sidecar/internal/logging"
)

// The outbound listener's documented address (DESIGN §2.1): loopback only.
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

	// Service B: the callee. In a real deployment this is another host's app,
	// reached through that host's inbound sidecar; there is no inbound
	// listener yet, so A's sidecar talks to B directly.
	serviceB := e2e.StartEcho("service-b")
	defer serviceB.Close()
	step(1, "service-b listening on %s", serviceB.URL)

	// Service A's sidecar. Its routing table is what turns the name
	// "service-b" into that address.
	sidecar, err := e2e.StartSidecar(outboundAddr, map[string][]string{
		"service-b": {serviceB.URL},
	}, logger)
	if err != nil {
		return err
	}
	defer sidecar.Close()
	step(2, "sidecar (outbound) listening on %s, routing service-b -> %s", sidecar.Addr, serviceB.URL)

	// Service A: the caller. It knows the name of the service it wants and the
	// address of its own sidecar, and nothing about where service-b runs.
	step(3, "service-a calls GET /v1/hello with Host: service-b")
	res, err := e2e.Call(sidecar.Addr, "service-b", "/v1/hello")
	if err != nil {
		return fmt.Errorf("service-a could not reach service-b: %w", err)
	}

	got, err := e2e.ReadReceived(res)
	if err != nil {
		return err
	}
	step(4, "service-a got %s", res.Status)
	fmt.Printf("    service-b received:  %s %s\n", got.Method, got.Path)
	fmt.Printf("    upstream Host:       %s   (rewritten by the sidecar from %q)\n", got.Host, "service-b")
	fmt.Printf("    X-Forwarded-For:     %s   (added by the sidecar)\n", got.XForwardedFor)
	fmt.Printf("    X-Forwarded-Proto:   %s\n", got.XForwardedProto)

	if res.StatusCode != 200 || got.Service != "service-b" {
		return fmt.Errorf("round trip did not reach service-b: status %s, service %q", res.Status, got.Service)
	}

	fmt.Printf("\n  Same request by hand:\n    curl -is -H 'Host: service-b' http://%s/v1/hello\n", sidecar.Addr)
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
