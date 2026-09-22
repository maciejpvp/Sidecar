# Sidecar — Configuration Reference

The sidecar reads one YAML file, passed with `-config` (default `./sidecar.yaml`).
Design rationale for every field lives in [DESIGN.md](DESIGN.md).

## Full annotated example

```yaml
# ---------------------------------------------------------------- listeners
# Not hot-reloadable. Changing these requires a restart.
listeners:
  inbound:  "0.0.0.0:15000"      # public entry, other sidecars connect here
  outbound: "127.0.0.1:15001"    # MUST be loopback, used by the local app

# ---------------------------------------------------------------- local app
app:
  address: "127.0.0.1:8080"      # where inbound forwards to (not hot-reloadable)

inbound:
  defaultTimeout: 3s             # deadline when caller sent no X-Request-Timeout-Ms
  maxTimeout: 30s                # caller-provided deadlines are clamped to this

# ---------------------------------------------------------------- global defaults
# Every service inherits these, and can override any of them.
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

# ---------------------------------------------------------------- services
services:
  - name: orders-svc             # the app names it in Host (DESIGN §3.1)
    instances:                   # host:port only — no scheme, no path (see below)
      - "10.0.0.7:15000"         # other sidecars' INBOUND ports
      - "orders-svc-2.internal:15000"   # a DNS name works too, resolved at connect time
      - "[2001:db8::9]:15000"    # IPv6 needs its brackets
    timeout: 2s                  # override

  - name: payments-svc
    instances:
      - "10.0.1.3:15000"
    retry:
      maxAttempts: 1             # payments: never retry from the sidecar
    outlier:
      consecutiveFailures: 3

# ---------------------------------------------------------------- misc
limits:
  maxBodyBytes: 10485760         # hard cap on request bodies (413 request_too_large)
  maxHeaderBytes: 65536

reload:
  interval: 2s                   # mtime poll interval; 0 disables hot reload

shutdown:
  drainTimeout: 10s

log:
  level: info                    # debug | info | warn | error
```

## Field reference

| Path | Type | Default | Reloadable | Notes |
|---|---|---|---|---|
| `listeners.inbound` | host:port | `0.0.0.0:15000` | no | |
| `listeners.outbound` | host:port | `127.0.0.1:15001` | no | must resolve to loopback |
| `app.address` | host:port | `127.0.0.1:8080` | no | |
| `inbound.defaultTimeout` | duration | `3s` | yes | |
| `inbound.maxTimeout` | duration | `30s` | yes | |
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
| `services[].name` | string | — | yes | required, unique, `^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$` |
| `services[].instances` | []host:port | — | yes | required, ≥ 1, unique within service; no scheme |
| `services[].timeout` / `perTryTimeout` / `retry.*` / `outlier.*` | — | inherits `defaults` | yes | field-by-field override |
| `limits.maxBodyBytes` | int | `10485760` | yes | |
| `limits.maxHeaderBytes` | int | `65536` | no | applied to `http.Server` |
| `reload.interval` | duration | `2s` | no | |
| `shutdown.drainTimeout` | duration | `10s` | yes | |
| `log.level` | enum | `info` | yes | |

## Validation rules

Config is rejected as a whole if any rule fails. At startup that means exit code 1. During a reload the old config is kept and `config_rejected` is logged.

1. YAML must parse, and **unknown fields are errors** (`yaml.Decoder.KnownFields(true)`), so typos never get silently ignored.
2. All required fields present, all values in the ranges above.
3. Service names are unique and don't collide with reserved prefixes (`_sidecar`).
4. Every instance address parses with `net.SplitHostPort`, and the port is 1..65535. The host may be
   a DNS name, an IPv4 literal or a bracketed IPv6 literal (`[2001:db8::7]:15000` — brackets are
   required, since an unbracketed IPv6 address is ambiguous about where the port starts); names are
   resolved by the dialler at connect time, not at config load. A scheme
   (`http://…`) or a path is an error, not something to strip: v1 dials plain HTTP and TLS lives
   behind the `Transport` seam (DESIGN §6), so the scheme is not a per-instance choice. Keeping the
   address scheme-less also makes the config string, the access log's `instances` field and the
   outlier key (`service|addr`) literally the same text. Decision D2 in [QUESTIONS.md](QUESTIONS.md).
5. `perTryTimeout` (if > 0) ≤ `timeout`.
6. `listeners.outbound` host is a loopback address.
7. A service must not list this sidecar's own inbound address as an instance (loop guard).
