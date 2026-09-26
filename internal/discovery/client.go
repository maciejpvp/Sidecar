// Package discovery is the sidecar's half of the control-plane protocol
// (DESIGN §13): a Registrar that keeps this instance registered while the
// local app is healthy, and a Watcher that long-polls for snapshots and hands
// each valid one to the routing table.
//
// Neither ever gives up. The control plane being unreachable is a normal
// state: the sidecar keeps routing with its last table and keeps trying.
package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"sidecar/internal/meshapi"
)

// ErrWarmingUp is the control plane refusing snapshots after a restart.
var ErrWarmingUp = errors.New("control plane is warming up")

// Client talks to one control plane.
type Client struct {
	base string // http://host:port
	http *http.Client
}

func NewClient(addr string) *Client {
	return &Client{
		base: "http://" + addr,
		// No Client.Timeout: a snapshot long-poll is meant to be held open.
		// Every call carries a context deadline instead.
		http: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()},
	}
}

func (c *Client) Heartbeat(ctx context.Context, reg meshapi.Registration) (meshapi.Lease, error) {
	var lease meshapi.Lease
	res, err := c.post(ctx, meshapi.PathHeartbeat, reg)
	if err != nil {
		return lease, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return lease, responseError(res)
	}
	if err := json.NewDecoder(res.Body).Decode(&lease); err != nil {
		return lease, fmt.Errorf("decode lease: %w", err)
	}
	if lease.Heartbeat <= 0 {
		return lease, fmt.Errorf("control plane sent heartbeat interval %v", lease.Heartbeat)
	}
	return lease, nil
}

func (c *Client) Deregister(ctx context.Context, reg meshapi.Registration) error {
	res, err := c.post(ctx, meshapi.PathDeregister, reg)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		return responseError(res)
	}
	return nil
}

// Snapshot returns the snapshot after version, waiting up to wait for one.
// It returns nil and no error when wait ran out with nothing new.
func (c *Client) Snapshot(ctx context.Context, version string, wait time.Duration) (*meshapi.Snapshot, error) {
	q := url.Values{"version": {version}, "wait": {wait.String()}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+meshapi.PathSnapshot+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusOK:
		var snap meshapi.Snapshot
		if err := json.NewDecoder(res.Body).Decode(&snap); err != nil {
			return nil, fmt.Errorf("decode snapshot: %w", err)
		}
		return &snap, nil
	case http.StatusNotModified:
		return nil, nil
	case http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: %v", ErrWarmingUp, responseError(res))
	default:
		return nil, responseError(res)
	}
}

func (c *Client) post(ctx context.Context, path string, body any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.http.Do(req)
}

func responseError(res *http.Response) error {
	var e meshapi.Error
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if json.Unmarshal(body, &e) == nil && e.Code != "" {
		return fmt.Errorf("control plane: %d %s: %s", res.StatusCode, e.Code, e.Message)
	}
	return fmt.Errorf("control plane: %d %s", res.StatusCode, bytes.TrimSpace(body))
}

// backoff doubles from min to max; reset starts it over.
type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur == 0 {
		b.cur = b.min
	} else {
		b.cur = min(b.cur*2, b.max)
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }

// sleep waits d or until ctx is done, and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
