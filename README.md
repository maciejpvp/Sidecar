# Sidecar

Small Go service-mesh prototype for service-to-service HTTP communication.

- Applications send outbound requests through a local sidecar.
- Sidecars register healthy instances with a control plane.
- The control plane distributes service instances and routing policy.
- The sidecar selects an instance, proxies the request, and handles retries.

The project uses the Go standard library and `gopkg.in/yaml.v3`.

## Architecture

```text
                                      control plane
                              +-------------------------+
                              | registry                 |
                              | mesh policy + hot reload |
                              | HTTP/JSON API :15100     |
                              +------------+------------+
                                           ^
                         heartbeat         |        long-poll snapshots
                                           |
  application instance                    |
  +----------------------------+           |
  |  application               |           |
  |  127.0.0.1:8080            |           |
  |            |               |           |
  |  +---------v------------+  |           |
  |  | sidecar              |  |           |
  |  | outbound :15001      +--+-----------> other instance
  |  | routing table         |  |           |    :15000* / app
  |  | round robin           |  |           |
  |  | retries + deadlines  |  |           |
  |  | admin :15020         |  |           |
  |  +----------------------+  |           |
  +----------------------------+

  * The inbound listener is planned/configured but is not currently wired into
    cmd/sidecar. Current examples may advertise the application directly.
```

| Component | Responsibility |
| --- | --- |
| `cmd/sidecar` | Outbound proxy, routing table, discovery, admin endpoints |
| `cmd/controlplane` | In-memory registry and snapshot API |
| Application | Sends requests through `127.0.0.1:15001` |

## Request flow

1. The application sends a request to the local outbound listener.
2. The service name is read from the absolute URL host or `Host` header.
3. The sidecar checks its latest routing snapshot.
4. A healthy instance is selected using round-robin.
5. The request is proxied with `X-Forwarded-*` headers.
6. Eligible failures may be retried against another instance.
7. The upstream response or a structured sidecar error is returned.

Routing snapshots are swapped atomically. In-flight requests keep their resolved route; later requests use the new snapshot.

Current sidecar error codes:

- `mesh_not_ready`
- `no_route`
- `no_healthy_upstream`
- `deadline_exceeded`
- `upstream_connect_failed`

Errors are returned as JSON with an `X-Sidecar-Error` header.

## Discovery

### Sidecar

- Probes the local application health endpoint.
- Registers the instance only while the application is healthy.
- Renews the registration lease with heartbeats.
- Deregisters on application failure or shutdown.
- Long-polls for control-plane snapshots.
- Keeps the last valid routing table during control-plane outages.

### Control plane

- Stores registrations in memory.
- Expires registrations when heartbeats stop.
- Combines instances with policy from `mesh.yaml`.
- Publishes versioned snapshots.
- Waits for instances to re-register after a restart before serving snapshots.

## Control-plane API

| Method | Endpoint | Purpose |
| --- | --- | --- |
| `POST` | `/v1/heartbeat` | Register or renew an instance lease |
| `POST` | `/v1/deregister` | Remove an instance |
| `GET` | `/v1/snapshot?version=...&wait=...` | Fetch or long-poll a snapshot |
| `GET` | `/healthz` | Liveness |
| `GET` | `/readyz` | Snapshot readiness |

## Configuration

| File | Loaded by | Contains |
| --- | --- | --- |
| [`sidecar.yaml`](sidecar.yaml) | Each sidecar | Local app, listeners, identity, control plane, limits |
| [`mesh.yaml`](mesh.yaml) | Control plane | Lease settings, defaults, service policy, reload settings |

Configuration behavior:

- YAML fields are strictly validated.
- Sidecar configuration is loaded at startup.
- `mesh.yaml` is hot-reloaded by the control plane.
- Sidecar environment variables override `sidecar.yaml`.
- Instances are discovered dynamically; they are not listed in either file.

See [docs/CONFIG.md](docs/CONFIG.md) for the full reference.

## Run locally

Requirements: Go 1.26 or newer.

Start the control plane:

```bash
go run ./cmd/controlplane -mesh mesh.yaml
```

Start a sidecar in another terminal:

```bash
go run ./cmd/sidecar -config sidecar.yaml
```

The local application must listen on the configured address and pass its health check before registration.

Send a request through the sidecar:

```bash
curl -H 'Host: orders-svc' \
  http://127.0.0.1:15001/v1/orders/42
```

Or configure an HTTP proxy:

```bash
HTTP_PROXY=http://127.0.0.1:15001 \
  curl http://orders-svc/v1/orders/42
```

Inspect the sidecar:

```bash
curl http://127.0.0.1:15020/healthz
curl http://127.0.0.1:15020/readyz
curl http://127.0.0.1:15020/snapshot
```

## Kubernetes

Example manifests are in [`deploy/k8s`](deploy/k8s).

- `deploy/k8s/controlplane.yaml` creates the control plane and mesh ConfigMap.
- `deploy/k8s/example.yaml` demonstrates application Deployments with native sidecars.
- Per-pod identity is provided through environment variables and the Downward API.
- The current example uses transitional application-address advertisements until the inbound listener is implemented.

## Repository layout

| Directory | Purpose |
| --- | --- |
| `cmd/sidecar` | Sidecar executable |
| `cmd/controlplane` | Control-plane executable |
| `internal/config` | Parsing, defaults, validation, hot reload |
| `internal/controlplane` | Registry and snapshot API |
| `internal/discovery` | Registration and snapshot watcher |
| `internal/proxy` | HTTP proxy, retries, errors, transport |
| `internal/routing` | Routing tables and round-robin selection |
| `internal/meshapi` | Control-plane wire types |
| `deploy/k8s` | Kubernetes examples |
| `docs` | Design and configuration documentation |
| `e2e` | End-to-end tests and demo application |

## Testing

```bash
go test ./...
```

## Current scope

Implemented focus:

- HTTP/1.1 outbound proxying
- Dynamic registration and discovery
- Round-robin routing
- Request timeouts and eligible retries
- Structured access logging
- Kubernetes examples

Not currently implemented as a production feature set:

- Inbound sidecar listener and inbound context stamping
- TLS or mutual TLS
- Transparent interception
- gRPC/HTTP2 or raw TCP
- Persistent/high-availability control plane
- Authentication, metrics, and tracing export
- Rate limiting and request transformation

See [docs/DESIGN.md](docs/DESIGN.md), [docs/QUESTIONS.md](docs/QUESTIONS.md), and [docs/TODO.md](docs/TODO.md) for design details and planned work.
