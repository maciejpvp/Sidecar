# Sidecar — Configuration Reference

There are two files, owned by two processes ([DESIGN §8](DESIGN.md#8-configuration--hot-reload)):

| File | Read by | Holds | When |
|---|---|---|---|
| [`sidecar.yaml`](../sidecar.yaml) | each sidecar (`-config`, optional) | listeners, the local app, the control plane's address, this pod's identity | once, at startup |
| [`mesh.yaml`](../mesh.yaml) | the control plane (`-mesh`) | per-service policy, lease TTL | hot-reloaded, delivered to every sidecar |

**Neither file lists instances.** Every sidecar registers its own instance with the control plane
([DESIGN §13](DESIGN.md#13-control-plane)), and every sidecar learns the others from the
control plane's snapshots. A service `mesh.yaml` does not name still routes, with `defaults`.

Both files are **parsed, defaulted and validated** with `KnownFields(true)`: a typo or an
out-of-range value stops startup with exit 1 and a message naming the field, and every problem is
reported at once. Not every field is **honoured** yet, because the features they configure are still
to come ([TODO.md](TODO.md), decision D3 in [QUESTIONS.md](QUESTIONS.md)). Honoured today:

- sidecar: `listeners.outbound`, `listeners.admin`, `app.*`, `controlPlane.address`, `service.*`,
  `limits.maxHeaderBytes`, `shutdown.drainTimeout`, `log.level`;
- mesh: `registry.leaseTTL`, `reload.interval`, `log.level`, and per service `timeout`,
  `retry.maxAttempts`, `retry.maxBodyBytes`.

---

## `sidecar.yaml` — one sidecar

Optional. In Kubernetes there is usually no file at all: the per-pod values come from environment
variables filled by the Downward API ([deploy/k8s/example.yaml](../deploy/k8s/example.yaml)).

```yaml
# ---------------------------------------------------------------- identity (required)
controlPlane:
  address: "sidecar-controlplane.mesh.svc.cluster.local:15100"   # host:port

service:
  name: orders-svc               # what this pod registers as; callers put it in Host
  advertise: "10.1.2.3:15000"    # where other sidecars reach this instance: a routable
                                 # host:port, never 0.0.0.0. In Kubernetes $(POD_IP):15000.

# ---------------------------------------------------------------- local app
app:
  address: "127.0.0.1:8080"      # where inbound forwards to; also what gets health-checked
  healthPath: /healthz           # 2xx/3xx = healthy = registered. "" registers unconditionally.

# ---------------------------------------------------------------- listeners
listeners:
  inbound:  "0.0.0.0:15000"      # public entry, other sidecars connect here (not built yet)
  outbound: "127.0.0.1:15001"    # MUST be loopback, used by the local app
  admin:    "0.0.0.0:15020"      # /healthz (liveness), /readyz (has a snapshot), /snapshot

inbound:
  defaultTimeout: 3s             # deadline when caller sent no X-Request-Timeout-Ms
  maxTimeout: 30s                # caller-provided deadlines are clamped to this

limits:
  maxHeaderBytes: 65536

shutdown:
  drainTimeout: 10s              # after deregistering, how long in-flight requests get

log:
  level: info                    # debug | info | warn | error
```

### Environment overrides

A set, non-empty variable wins over the file. Empty counts as unset, because a templated manifest
produces `""` for a value it did not have.

| Variable | Field | Typical Kubernetes source |
|---|---|---|
| `SIDECAR_SERVICE` | `service.name` | `fieldRef: metadata.labels['app']` |
| `SIDECAR_ADVERTISE` | `service.advertise` | `"$(POD_IP):15000"` with `POD_IP` from `fieldRef: status.podIP` |
| `SIDECAR_CONTROL_PLANE` | `controlPlane.address` | constant per cluster |
| `SIDECAR_APP_ADDRESS` | `app.address` | per app |
| `SIDECAR_APP_HEALTH_PATH` | `app.healthPath` | per app |
| `SIDECAR_LOG_LEVEL` | `log.level` | — |

### Field reference

| Path | Type | Default | Notes |
|---|---|---|---|
| `controlPlane.address` | host:port | — | **required** |
| `service.name` | string | — | **required**, `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`, not `_sidecar*` |
| `service.advertise` | host:port | — | **required**; instance form (rule 4 below), not unspecified, not our own outbound listener |
| `app.address` | host:port | `127.0.0.1:8080` | |
| `app.healthPath` | path | `/healthz` | must start with `/`; empty disables the check |
| `listeners.inbound` | host:port | `0.0.0.0:15000` | |
| `listeners.outbound` | host:port | `127.0.0.1:15001` | must resolve to loopback |
| `listeners.admin` | host:port | `0.0.0.0:15020` | the kubelet probes it, so not loopback |
| `inbound.defaultTimeout` | duration | `3s` | ≤ `maxTimeout` |
| `inbound.maxTimeout` | duration | `30s` | |
| `limits.maxHeaderBytes` | int | `65536` | applied to the outbound `http.Server` |
| `shutdown.drainTimeout` | duration | `10s` | |
| `log.level` | enum | `info` | |

---

## `mesh.yaml` — the whole mesh (control plane)

Hot-reloaded: polled every `reload.interval`; a valid change is stored, bumps the snapshot version,
and reaches every sidecar through its pending long-poll. An invalid change is logged as
`config_rejected` once and the previous policy stays in effect. Only `reload.interval` is
restart-only (decision D4); a reload that changes it applies the rest and logs
`config_restart_required`. With no `-mesh` flag the control plane runs every service on the
built-in defaults.

```yaml
# ---------------------------------------------------------------- registry
registry:
  leaseTTL: 15s                  # dropped if not renewed for this long; sidecars heartbeat
                                 # every leaseTTL/3; a restarted control plane waits this long
                                 # before serving snapshots

# ---------------------------------------------------------------- global defaults
# Every service inherits these, and can override any of them. A service not
# listed under `services` gets exactly these.
defaults:
  timeout: 5s                    # overall request budget (all attempts + backoff)
  perTryTimeout: 0s              # 0 = no per-attempt limit (only overall deadline)

  retry:
    maxAttempts: 3               # total attempts including the first; 1 = no retries
    maxBodyBytes: 1048576        # bodies above this are streamed and never retried
    minAttemptTime: 20ms         # don't start an attempt with less time left than this
    backoff:
      base: 25ms
      max: 250ms
    budget:
      ratio: 0.2                 # retries may add at most 20% of request volume
      minPerSecond: 3            # floor so low-traffic services can still retry
      window: 10s

  outlier:
    consecutiveFailures: 5
    baseEjection: 30s            # ejection time = base * ejectionCount
    maxEjection: 5m
    maxEjectionPercent: 50       # never eject more than this share of a pool
    decayAfter: 5m               # healthy this long -> ejectionCount decremented

# ---------------------------------------------------------------- per-service overrides
# Only services that need something other than `defaults`. Naming a service
# here also makes it exist with zero instances: callers get 503 (down), not
# 404 (unknown), while none of its pods is registered.
services:
  - name: orders-svc
    timeout: 2s

  - name: payments-svc
    retry:
      maxAttempts: 1             # payments: never retry from the sidecar
    outlier:
      consecutiveFailures: 3

# ---------------------------------------------------------------- misc
limits:
  maxBodyBytes: 10485760         # hard cap on request bodies (413 request_too_large)

reload:
  interval: 2s                   # mtime poll interval; 0 disables hot reload

log:
  level: info                    # the control plane's own log level
```

### Field reference

| Path | Type | Default | Reloadable | Notes |
|---|---|---|---|---|
| `registry.leaseTTL` | duration | `15s` | yes | 3s..5m |
| `defaults.timeout` | duration | `5s` | yes | |
| `defaults.perTryTimeout` | duration | `0s` | yes | 0 = disabled |
| `defaults.retry.maxAttempts` | int | `3` | yes | 1..10 |
| `defaults.retry.maxBodyBytes` | int | `1048576` | yes | ≤ `limits.maxBodyBytes` |
| `defaults.retry.minAttemptTime` | duration | `20ms` | yes | |
| `defaults.retry.backoff.base` | duration | `25ms` | yes | |
| `defaults.retry.backoff.max` | duration | `250ms` | yes | ≥ base |
| `defaults.retry.budget.ratio` | float | `0.2` | yes | 0..1 |
| `defaults.retry.budget.minPerSecond` | int | `3` | yes | ≥ 0 |
| `defaults.retry.budget.window` | duration | `10s` | yes | whole seconds, 1s..60s |
| `defaults.outlier.consecutiveFailures` | int | `5` | yes | ≥ 1 |
| `defaults.outlier.baseEjection` | duration | `30s` | yes | |
| `defaults.outlier.maxEjection` | duration | `5m` | yes | ≥ baseEjection |
| `defaults.outlier.maxEjectionPercent` | int | `50` | yes | 0..100 |
| `defaults.outlier.decayAfter` | duration | `5m` | yes | |
| `services[].name` | string | — | yes | required, unique, DNS label, not `_sidecar*` |
| `services[].timeout` / `perTryTimeout` / `retry.*` / `outlier.*` | — | inherits `defaults` | yes | field-by-field override |
| `limits.maxBodyBytes` | int | `10485760` | yes | |
| `reload.interval` | duration | `2s` | no | |
| `log.level` | enum | `info` | yes | |

---

## Validation rules

A file is rejected as a whole if any rule fails: at startup that means exit code 1; for a
`mesh.yaml` reload the old policy is kept and `config_rejected` is logged.

1. YAML must parse, and **unknown fields are errors** (`yaml.Decoder.KnownFields(true)`), so typos
   never get silently ignored. That includes the old static-file fields: `services[].instances` in
   `mesh.yaml`, or `services:` in `sidecar.yaml`, fail with "field … not found" rather than being
   ignored.
2. All required fields present, all values in the ranges above.
3. Service names are DNS labels, unique, and don't use the reserved prefix `_sidecar`.
4. Every instance address — a registration, a snapshot entry, `service.advertise`,
   `controlPlane.address` — parses with `net.SplitHostPort`, and the port is 1..65535. The host
   may be a DNS name, an IPv4 literal or a bracketed IPv6 literal (`[2001:db8::7]:15000` — brackets
   are required, since an unbracketed IPv6 address is ambiguous about where the port starts); names
   are resolved by the dialler at connect time. A scheme (`http://…`) or a path is an error, not
   something to strip: decision D2 in [QUESTIONS.md](QUESTIONS.md).
5. `perTryTimeout` (if > 0) ≤ `timeout`.
6. `listeners.outbound` host is a loopback address.
7. `service.advertise` is not this sidecar's own outbound listener (loop guard): every caller of the
   service would be sent into our outbound port, which would pick this instance again.

The control plane applies rules 3 and 4 to every registration (`400 bad_registration`), and each
sidecar applies 2–5 again to every snapshot (`config.Snapshot`) before using it.
