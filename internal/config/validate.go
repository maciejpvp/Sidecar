package config

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Service names arrive in a Host header, so they must be DNS labels.
var serviceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedPrefix is kept for the sidecar's own endpoints.
const reservedPrefix = "_sidecar"

// validate applies CONFIG.md §Validation, reporting every problem at once so an
// operator does not restart once per mistake.
func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if err := listenAddr(c.Listeners.Inbound); err != nil {
		add("listeners.inbound: %w", err)
	}
	if err := listenAddr(c.Listeners.Outbound); err != nil {
		add("listeners.outbound: %w", err)
	} else if err := loopbackOnly(c.Listeners.Outbound); err != nil {
		add("listeners.outbound: %w", err)
	}
	if err := listenAddr(c.App.Address); err != nil {
		add("app.address: %w", err)
	}

	if c.Inbound.DefaultTimeout <= 0 {
		add("inbound.defaultTimeout: must be positive, got %v", c.Inbound.DefaultTimeout)
	}
	if c.Inbound.MaxTimeout <= 0 {
		add("inbound.maxTimeout: must be positive, got %v", c.Inbound.MaxTimeout)
	}
	if c.Inbound.DefaultTimeout > c.Inbound.MaxTimeout {
		add("inbound.defaultTimeout (%v) must not exceed inbound.maxTimeout (%v), or every request that omits a deadline is clamped below the default",
			c.Inbound.DefaultTimeout, c.Inbound.MaxTimeout)
	}

	if c.Limits.MaxBodyBytes <= 0 {
		add("limits.maxBodyBytes: must be positive, got %d", c.Limits.MaxBodyBytes)
	}
	if c.Limits.MaxHeaderBytes <= 0 {
		add("limits.maxHeaderBytes: must be positive, got %d", c.Limits.MaxHeaderBytes)
	}
	if c.Reload.Interval < 0 {
		add("reload.interval: must not be negative, got %v (0 disables hot reload)", c.Reload.Interval)
	}
	if c.Shutdown.DrainTimeout <= 0 {
		add("shutdown.drainTimeout: must be positive, got %v", c.Shutdown.DrainTimeout)
	}

	// Checked even when every service overrides it: a broken default is still wrong.
	errs = append(errs, c.Defaults.validate("defaults", c.Limits)...)

	seen := make(map[string]int, len(c.Services))
	for i, svc := range c.Services {
		at := fmt.Sprintf("services[%d]", i)
		if svc.Name != "" {
			at = fmt.Sprintf("services[%d] (%s)", i, svc.Name)
		}

		switch {
		case svc.Name == "":
			add("%s.name: required", at)
		case !serviceName.MatchString(svc.Name):
			add("%s.name: must be a DNS label, since the app names the service in Host", at)
		case strings.HasPrefix(svc.Name, reservedPrefix):
			add("%s.name: %q is reserved for the sidecar's own endpoints", at, reservedPrefix)
		}
		if first, dup := seen[svc.Name]; dup && svc.Name != "" {
			add("%s.name: %q is already used by services[%d]", at, svc.Name, first)
		} else if svc.Name != "" {
			seen[svc.Name] = i
		}

		if len(svc.Instances) == 0 {
			add("%s.instances: required, at least one", at)
		}
		listed := make(map[string]bool, len(svc.Instances))
		for _, inst := range svc.Instances {
			if err := instanceAddr(inst); err != nil {
				add("%s.instances: %q: %w", at, inst, err)
				continue
			}
			// A duplicate would get two round-robin slots and two sets of outlier state.
			if listed[inst] {
				add("%s.instances: %q is listed twice", at, inst)
			}
			listed[inst] = true
			if inst == c.Listeners.Inbound || sameLoopbackPort(inst, c.Listeners.Inbound) {
				add("%s.instances: %q is this sidecar's own inbound address, which would route a request back into itself", at, inst)
			}
		}

		errs = append(errs, svc.Policy.validate(at, c.Limits)...)
	}

	return errors.Join(errs...)
}

func (p Policy) validate(at string, limits Limits) []error {
	var errs []error
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if p.Timeout <= 0 {
		add("%s.timeout: must be positive, got %v", at, p.Timeout)
	}
	if p.PerTryTimeout < 0 {
		add("%s.perTryTimeout: must not be negative, got %v (0 disables it)", at, p.PerTryTimeout)
	}
	if p.PerTryTimeout > 0 && p.PerTryTimeout > p.Timeout {
		add("%s.perTryTimeout (%v) must not exceed timeout (%v)", at, p.PerTryTimeout, p.Timeout)
	}

	if p.Retry.MaxAttempts < 1 || p.Retry.MaxAttempts > 10 {
		add("%s.retry.maxAttempts: must be 1..10, got %d (1 disables retries)", at, p.Retry.MaxAttempts)
	}
	if p.Retry.MaxBodyBytes < 0 {
		add("%s.retry.maxBodyBytes: must not be negative, got %d", at, p.Retry.MaxBodyBytes)
	}
	if p.Retry.MaxBodyBytes > limits.MaxBodyBytes {
		// Buffering past the hard cap would hold a body about to be rejected with 413.
		add("%s.retry.maxBodyBytes (%d) must not exceed limits.maxBodyBytes (%d)", at, p.Retry.MaxBodyBytes, limits.MaxBodyBytes)
	}
	if p.Retry.MinAttemptTime < 0 {
		add("%s.retry.minAttemptTime: must not be negative, got %v", at, p.Retry.MinAttemptTime)
	}
	if p.Retry.Backoff.Base < 0 {
		add("%s.retry.backoff.base: must not be negative, got %v", at, p.Retry.Backoff.Base)
	}
	if p.Retry.Backoff.Max < p.Retry.Backoff.Base {
		add("%s.retry.backoff.max (%v) must be at least backoff.base (%v)", at, p.Retry.Backoff.Max, p.Retry.Backoff.Base)
	}

	if p.Retry.Budget.Ratio < 0 || p.Retry.Budget.Ratio > 1 {
		add("%s.retry.budget.ratio: must be 0..1, got %v", at, p.Retry.Budget.Ratio)
	}
	if p.Retry.Budget.MinPerSecond < 0 {
		add("%s.retry.budget.minPerSecond: must not be negative, got %d", at, p.Retry.Budget.MinPerSecond)
	}
	switch w := p.Retry.Budget.Window; {
	case w < time.Second || w > time.Minute:
		add("%s.retry.budget.window: must be 1s..60s, got %v", at, w)
	case w%time.Second != 0:
		// The window is one-second buckets (DESIGN §5.2.2).
		add("%s.retry.budget.window: must be whole seconds, got %v", at, w)
	}

	if p.Outlier.ConsecutiveFailures < 1 {
		add("%s.outlier.consecutiveFailures: must be at least 1, got %d", at, p.Outlier.ConsecutiveFailures)
	}
	if p.Outlier.BaseEjection <= 0 {
		add("%s.outlier.baseEjection: must be positive, got %v", at, p.Outlier.BaseEjection)
	}
	if p.Outlier.MaxEjection < p.Outlier.BaseEjection {
		add("%s.outlier.maxEjection (%v) must be at least outlier.baseEjection (%v)", at, p.Outlier.MaxEjection, p.Outlier.BaseEjection)
	}
	if p.Outlier.MaxEjectionPercent < 0 || p.Outlier.MaxEjectionPercent > 100 {
		add("%s.outlier.maxEjectionPercent: must be 0..100, got %d", at, p.Outlier.MaxEjectionPercent)
	}
	if p.Outlier.DecayAfter <= 0 {
		add("%s.outlier.decayAfter: must be positive, got %v", at, p.Outlier.DecayAfter)
	}

	return errs
}

// instanceAddr checks an upstream address: bare host:port, since v1 dials plain
// HTTP and TLS belongs to the Transport. The host is resolved at connect time.
func instanceAddr(addr string) error {
	if strings.Contains(addr, "/") {
		return errors.New("want host:port without a scheme")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// "2001:db8::7:15000" cannot say where the address ends and the port begins.
		if strings.Count(addr, ":") > 1 && !strings.Contains(addr, "[") {
			return errors.New("want host:port, with an IPv6 address in brackets: [2001:db8::7]:15000")
		}
		return fmt.Errorf("want host:port: %w", err)
	}
	if host == "" {
		return errors.New("want host:port: host is empty")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("want host:port: port %q is not 1..65535", port)
	}
	return nil
}

// listenAddr checks an address this process binds. Unlike an instance, an empty
// host is legal: it means every interface.
func listenAddr(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("want host:port, got %q", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not 1..65535", port)
	}
	return nil
}

func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if isLoopback(host) {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("must be a loopback address, and %q is a name that could resolve anywhere", host)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("must bind loopback only (127.0.0.1 or ::1), got %q — anywhere else is an open proxy into the mesh", host)
	}
	return nil
}

// sameLoopbackPort reports whether an instance is our own inbound listener
// spelled differently: 0.0.0.0:15000, 127.0.0.1:15000 and localhost:15000 are
// one socket from here.
func sameLoopbackPort(instance, inbound string) bool {
	iHost, iPort, err := net.SplitHostPort(instance)
	if err != nil {
		return false
	}
	lHost, lPort, err := net.SplitHostPort(inbound)
	if err != nil || iPort != lPort || !isLoopback(iHost) {
		return false
	}
	// Only a listener on every interface or on loopback is reachable that way.
	if lHost == "" || isLoopback(lHost) {
		return true
	}
	lIP := net.ParseIP(lHost)
	return lIP != nil && lIP.IsUnspecified()
}

// isLoopback is true for a loopback IP literal or the name localhost.
func isLoopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
