# Sidecar — Decisions log & open questions

Decisions that [DESIGN.md](DESIGN.md) assumes but does not argue, recorded so they are not
re-litigated by accident — and the questions still open.

---

## Decisions

### D1 — Services are addressed by `Host`, not by a path prefix

*Status: decided · affects DESIGN §1, §3.1, §4, §10, §12 · code: `internal/proxy.serviceName`*

The first design draft had the app call `http://127.0.0.1:15001/<service>/<path>`, taking the
first path segment as the service name. The implementation reads the `Host` header instead (or
`URL.Host`, for the absolute-URI form an HTTP proxy receives).

**Why `Host` won:**

- **The sidecar claims no part of the app's URL space.** A prefix means the sidecar owns the
  first path segment forever: paths have to be rewritten on the way out, `Location` headers and
  cookie paths on the way back, and any upstream that generates absolute links is subtly wrong.
  With `Host`, the request is forwarded with its path byte-for-byte.
- **Unmodified HTTP clients work.** `HTTP_PROXY=http://127.0.0.1:15001` makes every standard
  library's client route through the sidecar with no code change, because naming a host in the
  request line is exactly what an HTTP proxy expects. A prefix needs the base URL threaded through
  the app.
- **It reads like DNS.** The app's code says `http://orders-svc/v1/orders/42` either way, which is
  what the same call would look like in a mesh with real service DNS. Swapping the sidecar out for
  DNS + a plain client later changes nothing in the app.

**Costs accepted:**

- A `curl` against the sidecar needs `-H 'Host: …'` or `HTTP_PROXY`, which is slightly less obvious
  than a path — hence the examples in DESIGN §4 and [../e2e/README.md](../e2e/README.md).
- The app cannot reach two services in one request path (never wanted).
- `Host` is also how virtual hosting works, so an upstream that inspects `Host` sees the instance
  address instead of the service name. The logical name survives as `X-Forwarded-Host`.

**Rejected alternative:** a custom header (`X-Target-Service`, which the spike used). It works, but
it is invisible to every off-the-shelf client and proxy, so nothing can be pointed at the sidecar
without being taught about the header first.

### D2 — Instance addresses are bare `host:port`, with no scheme

*Status: decided · affects CONFIG.md §services + §Validation 4, DESIGN §6.1, §11 · code: `internal/config.instanceAddr`*

The spike accepted full URLs (`http://10.0.0.7:15000`) because it handed them straight to
`url.Parse`. The configured form is now `host:port`: `config` validates it, and `routing` builds
the `http` URL from it without checking again.

**Why:**

- **The scheme is not a per-instance choice.** v1 speaks plain HTTP to every instance, and TLS is
  reserved for the `Transport` seam (DESIGN §1.2, §6) — a per-service or per-mesh decision, not
  something an operator should be able to set differently on instance #3 of the same pool.
- **One canonical string.** An instance address appears in the config file, in the access log's
  `instances` field (DESIGN §11) and in the outlier state key `service|addr` used for carry-over
  across reloads (DESIGN §8). If config says `http://10.0.0.7:15000` and the log says
  `10.0.0.7:15000`, then grep, and eventually state carry-over, quietly stop matching.
- **Stricter validation.** `net.SplitHostPort` + a port range rejects what `url.Parse` waves
  through: `url.Parse` accepts `ftp://host`, and accepts a path it will then silently drop —
  `https://www.example.com/` parsed fine in the spike and the trailing `/` simply vanished.
- **Instances are other sidecars' inbound ports** (DESIGN §2), which are addresses, not URLs.

**What this does *not* restrict.** The rule is about the *shape* of the address, not about which
hosts are allowed. The host may be a DNS name (single-label, dotted, fully qualified with a trailing
dot, any case, punycode), an IPv4 literal, or a bracketed IPv6 literal, because instances live
wherever the operator runs them. The sidecar never resolves a name itself — that stays with the
dialler, so an instance whose DNS record moves is followed without a config reload.
`e2e.TestInstanceAddressFamilies` proves a request reaches an instance named by IPv6 literal and by
DNS name; `config.TestInstanceAddressForm` pins the accepted forms.

Two forms are refused, both deliberately:

- **An unbracketed IPv6 address** (`2001:db8::7:15000`). Not a policy choice: the last group is
  ambiguously a port or part of the address, so `[2001:db8::7]:15000` is the only readable form
  (RFC 3986 §3.2.2, and what `net.SplitHostPort` / `net.Dial` require). The error says so.
- **A missing port** (`orders-svc`) — see Q5, which is a real choice and could go the other way.

**Cost accepted:** when TLS arrives it needs a config knob of its own (`service.tls: true` or a
transport block) rather than coming free with a per-instance `https://`. That is the right shape
anyway — see Q1.

### D3 — The config files carry the whole schema, even the knobs nothing honours yet

*Status: decided · affects CONFIG.md, `internal/config` · code: `config.LoadMesh`, `config.LoadSidecar`*

`internal/config` parses, defaults and validates every field in CONFIG.md — including
`outlier.*`, `retry.budget.*`, `perTryTimeout` and `inbound.*`, none of which has a consumer yet.
The honoured set is listed at the top of CONFIG.md. (Since D6 the schema is split across
`sidecar.yaml` and `mesh.yaml`; the rule applies to both.)

**Why not a subset that grows with the features:** `KnownFields(true)` (validation rule 1, so a
misspelled `maxAttemps` cannot silently default) means any field the structs do not know is a
*startup error*. A subset would therefore make the documented example config fail to load, and
every feature landing later would be a breaking config change for anyone who had written the
documented form. Parsing the whole schema costs a struct field and a range check per knob.

**The cost, and how it is paid:** a knob that validates but does nothing is a trap — an operator
could set `outlier.consecutiveFailures: 3` and believe ejection is on. So the honoured set is listed
at the top of CONFIG.md and marked in the committed `mesh.yaml`, and each TODO item says which
knob it switches on. If that turns out to be too subtle, the next step is a startup warning naming
configured-but-inert fields.

**All validation lives in `config`.** Instance address syntax first stayed in `routing`, on the
theory that the rule belongs to the code that dials. Hot reload made that split expensive: config
needed a hook to ask routing before storing a file, the table was built twice per reload, and
`main` carried a panic for the case where the two disagreed — without the hook, one typo in an
instance would have crashed the running sidecar on reload. Now `validate.go` is the whole
rulebook and `routing.NewTable` cannot fail.

**One service type, one set of defaults.** `routing.NewTable` takes `[]config.Service` and
`routing.Service` embeds `config.Policy`, so there is no translation layer and no `0 → default`
rule outside `config`. Code that builds a service by hand (tests, the demo) starts from
`config.NewService`, which applies the same defaults a file gets.

**Not decided here:** the per-service knobs that *do* have consumers today but still live as
constants in `internal/proxy` (`backoff.base`/`max`, `minAttemptTime`). They already reach the
proxy through the embedded policy; switching the constants over is next. (`retry.maxBodyBytes`
made that switch first, after a review found the config value was silently ignored.)

### D4 — On reload, restart-only fields keep their running value

*Status: decided · affects DESIGN §8, CONFIG.md · code: `config.(*Mesh).keepStatic`*

> Since D6 only `mesh.yaml` is reloaded, and its one restart-only field is `reload.interval`. The
> sidecar's own file is read once, so listeners and the app address can no longer drift from what
> the process is bound to. The rule stands for any restart-only field the mesh file grows.

Some fields cannot change in a running process. When a reload changes one, the process applies the
rest of the file, puts those fields back to their running values, and logs
`config_restart_required` naming them.

**Why not store the file as written and just warn:** `Current()` would then say (in the original
single-file design) the outbound listener is `:25102` while the process is bound to `:25101`. Anything reading the config — a
future admin endpoint, a log line, the loop guard — would be told something false.

**Why not reject the whole reload:** an operator who changes a timeout and, in the same edit, a
listener would get *no* timeout change until they restart, which is a surprising way to find out
a field is restart-only.

(The original design re-validated after the restore, because the loop guard compared instances
with the restored inbound address. Neither exists in `mesh.yaml`, so that step went with them.)

### D5 — Instances register themselves; the control plane does not watch Kubernetes

*Status: decided · affects DESIGN §13 · code: `internal/controlplane`, `internal/discovery`*

Each sidecar registers its own instance (`service`, `address`, `id`) and renews it with heartbeats;
a lease that is not renewed expires. This is the Consul/Eureka model.

**The alternative, and it is a strong one:** the control plane watches EndpointSlices, which is
what Istio and Linkerd do. Kubernetes already knows every pod's IP and readiness, so heartbeats,
leases and warmup would all go away.

**Why self-registration anyway:**

- The control plane stays platform-independent: the e2e suite runs a real control plane and real
  sidecars in one process, and the docker-compose demo needs no cluster.
- Leases, expiry, warmup after restart and versioned snapshots are the distributed-systems part
  this project exists to build.
- The local app's own health decides registration (DESIGN §13.4), which is the same signal a
  readiness probe would give Kubernetes.

**Cost accepted:** a dead pod is routed to until its lease expires (≤ 15 s by default), where
Kubernetes would drop it from endpoints on the readiness failure. Retries on connection failure,
and outlier ejection once it lands, cover the gap. A Kubernetes-backed `Registry` remains possible
behind the same versioned view.

### D6 — Per-service policy lives in the control plane

*Status: decided · affects CONFIG.md, DESIGN §8, §13 · code: `config.Mesh`, `config.Sidecar`*

Timeouts, retries and outlier settings are in `mesh.yaml`, read and hot-reloaded by the control
plane, and delivered inside every snapshot. The sidecar's own config shrank to what is per-pod
(identity, listeners, app, control-plane address), which in Kubernetes is environment variables.

**Why:** a policy is a property of the *callee*. With per-sidecar files, two callers of
`payments-svc` could disagree about whether it may be retried, and changing a timeout meant editing
every caller's file. It also keeps "zero manual work" honest: a new service needs no file edit at
all, because an unnamed service gets `defaults`.

**Consequence:** the old static `services:` list is gone rather than kept as a fallback, so there is
one code path. Tests that want a fixed table build one with `routing.NewTable` directly, as the
proxy tests and `e2e.StartSidecar` already did.

### D7 — Snapshots are delivered by versioned long-poll

*Status: decided · affects DESIGN §13.3 · code: `controlplane.(*Server).snapshot`, `discovery.Watcher`*

`GET /v1/snapshot?version=V&wait=30s` answers at once when `V` is stale and otherwise waits for a
change. Chosen over:

- **Piggybacking on the heartbeat** (simplest): changes would take up to a heartbeat interval (5 s)
  to arrive, which is exactly when a pod has just died.
- **Server-sent events / a stream** (closest to xDS): a persistent stream needs its own resume
  protocol and reconnect logic, and gains nothing over long-poll at this scale.

Snapshots are complete, not deltas, and the version carries a per-process epoch so a restarted
control plane never reuses a version string (DESIGN §13.3).

### D8 — A sidecar with no snapshot runs, but is not ready

*Status: decided · affects DESIGN §9, §13.5 · code: `routing.Store.Ready`, `proxy` (`mesh_not_ready`)*

A sidecar that cannot reach the control plane at startup starts anyway, fails `/readyz`, answers
outbound calls with `503 mesh_not_ready`, and keeps retrying. It registers its app as soon as it can,
independently of snapshots.

**Why not exit and let Kubernetes restart it:** crash-looping couples every pod's startup to the
control plane's, and the backoff Kubernetes applies to a crash loop makes recovery *slower* once the
control plane is back. **Why not a local fallback file:** it would be a second source of routes that
drifts from the real one, and a second code path.

**Why 503 and not 404:** before the first snapshot every name is unknown, and `404 no_route` would
tell the app "this service does not exist". It is the sidecar that is not ready, and a retry soon
will work.

### D9 — A heartbeat is a full registration, keyed by address, guarded by a process id

*Status: decided · affects DESIGN §13.2 · code: `controlplane.Registry`, `meshapi.Registration`*

- **Idempotent heartbeat = registration**: there is no separate renew call, so a control plane that
  lost its state is refilled by the heartbeats it was going to receive anyway. No "unknown lease,
  re-register" handshake to get wrong.
- **Keyed by address**: an address is one pod. The newest registration of an address wins.
- **Random id per sidecar process**: pod IPs are reused, and without an id the delayed deregister
  of a dead pod could remove the live pod that now has its IP.
- **Warmup = one lease TTL** after a control-plane start: by then every live instance has
  heartbeated at least twice (every TTL/3). `e2e.TestControlPlaneRestart` fails with warmup 0.

### D10 — Two images from one Dockerfile

*Status: decided · affects `deploy/` · code: `Dockerfile` (targets `sidecar`, `controlplane`)*

The sidecar and the control plane ship as separate images (`sidecar`, `sidecar-controlplane`),
built as two final stages over one shared build stage, each with its own `ENTRYPOINT`.

**Why not one image with both binaries:** the two roll out on very different schedules. The
control plane is one Deployment, restarted freely; a new sidecar reaches production only by
restarting every app pod. A shared tag makes a control-plane-only fix look like a sidecar release,
and makes it easy to roll both by accident. Separate entrypoints also mean a manifest cannot start
the wrong binary, and app pods no longer carry a control-plane binary they never run.

**What we give up:** a single tag that guarantees both sides were built from the same commit.
During a rollout, sidecars and the control plane will run different versions, so any change to
the `cpapi` protocol has to stay compatible with the previous release on both sides.

---

## Open questions

### Q1 — Where does TLS configuration hang?

D2 keeps the scheme out of the instance address, so enabling TLS needs a knob. Candidates: a
per-service `tls:` block, a mesh-wide transport setting, or "always TLS between sidecars, plain
only to the local app". Not urgent (v1 is plain HTTP by §1.2) but it decides whether `Transport`
is per-service or global.

### Q2 — Is a per-attempt timeout needed before retries are useful?

DESIGN §5.1 lists "per-attempt timeout (no response headers in time)" as retriable, but
`perTryTimeout` defaults to `0s` (disabled) and is unimplemented, so today a slow instance consumes
the entire request budget on its first attempt and no retry is ever started. Fast failures (refused
dial, 502/503/504) retry correctly. So the most common real-world failure — an instance that is up
but hanging — currently gets one attempt. Either `perTryTimeout` gets a non-zero default, or §5.1's
timeout row should say so explicitly.

### Q3 — Should the outbound listener reject requests that name it by address?

A request arriving with `Host: 127.0.0.1:15001` (i.e. an app that forgot to name a service) gets
`404 no_route` with "unknown service", which is correct but unhelpful — the operator's mistake is
the missing name, not a missing route. Worth a distinct message, perhaps a distinct code.

### Q5 — Should the port be optional, defaulting to the inbound port?

> Mostly moot since D5: nobody types instance addresses any more, and the sidecar builds its
> advertised address from `$(POD_IP)` and a port in the manifest.


Today `orders-svc` is rejected and `orders-svc:15000` is required. Since every instance is another
sidecar's inbound listener, and that defaults to `:15000`, the port is the same in nearly every
entry — so it could default, leaving `instances: ["10.0.0.7", "10.0.0.8"]`.

*For requiring it:* the demo, `docker-compose` and any host running two sidecars all use
non-default ports, and a silently defaulted port turns a typo into a connect failure at 3am rather
than a config error at startup. It also keeps one address string (D2) instead of a configured form
and an expanded form.

*For defaulting it:* less repetition, and the default is genuinely the common case.

### Q6 — How should registration be authenticated?

Today anything that reaches `:15100` can register as any service and receive its traffic. The
natural fix in Kubernetes: the sidecar sends its projected ServiceAccount token, the control plane
validates it with TokenReview and checks the pod's labels match the claimed service. That adds
client-go (or a hand-rolled API call) and RBAC. Outside Kubernetes it needs a different identity
source, so it should hang off an interface. Same trust boundary as the plain-HTTP data plane
(DESIGN §10), which is why it waited.

### Q7 — What would a highly available control plane look like?

One replica with in-memory state is a single point of failure for *changes* (existing routes keep
working, DESIGN §13.5). Options, cheapest first:

1. **Sidecars heartbeat to every replica** (headless Service, resolve all IPs). Each replica then
   holds the full registry independently, with no replication protocol. Snapshots from different
   replicas have different epochs, so a sidecar switching replicas re-fetches once — harmless.
2. **Shared store** (etcd, Redis) with leases in the store.
3. **Watch Kubernetes instead** (D5's alternative), where the API server is the HA store.

Until then the Deployment uses `strategy: Recreate`, because a rolling update would briefly split
registrations between two replicas.

### Q8 — Should a sidecar be able to run without registering?

Every sidecar registers, so a pure client (a batch job, a cron) must still serve a health endpoint
and advertise an address nobody should call. A `service.register: false` switch is small; the open
part is whether such a pod should still need a service name (for logs, and eventually for Q6's
authorization).

### Q9 — Should the sidecar notice pod termination directly?

Deregistration on pod shutdown currently hangs on the app's health check failing (DESIGN §13.4),
because a native sidecar is signalled only *after* the app exits. Apps that fail health on SIGTERM
close the window; apps that just exit leave up to ~1 s during which callers hit refused connections
and retry. Alternatives: a `preStop` hook on the app container that calls the sidecar's admin port
(per-app manifest work, against the zero-manual goal), or watching the pod's own deletion timestamp
(needs the Kubernetes API).

### Q4 — Does `Location` rewriting (DESIGN §10) belong in v1?

Under D1 the rewrite is authority-only (`http://10.0.0.7:15000/v1/x` → `http://orders-svc/v1/x`),
which is much smaller than the path rewrite the prefix design needed. It may be cheap enough to
pull forward from P3.
