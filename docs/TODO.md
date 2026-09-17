# Sidecar — TODO

Ordered by importance: each tier unblocks the next. Within a tier, top items first.
Design rationale for every item lives in [DESIGN.md](DESIGN.md) (section refs below).

Current state (spike): outbound-only proxy, routing by `X-Target-Service` header, hardcoded
service table, round-robin over all endpoints, JSON error model, JSON logger.

---

## P0 — Foundation (nothing else is buildable or testable without these)

- [ ] **Fix what's already wrong**
  - `main.go` listens on `:8080` — that's the *app's* port per DESIGN §2.1. Outbound belongs on
    `127.0.0.1:15001`, and it must bind loopback only or the sidecar is an open proxy into the mesh.
  - `http.ListenAndServe` error is discarded — a failed bind currently exits 0 silently.
  - Instances are full URLs (`https://www.youtube.com/`); CONFIG.md specifies `host:port`.
    Pick one now, because routing, outlier keys (`service|addr`) and logs all embed this format.
  - *Done when:* sidecar binds the documented ports and exits non-zero with a log line on bind failure.

- [ ] **Decide the addressing model: path prefix vs header** — DESIGN §3.1 says
  `/{service}/{path}`, the code says `X-Target-Service`. This is the app-facing contract; every
  later feature (routing, logs, redirect rewriting) assumes one. Path-prefix is the design choice
  and matches how Envoy-style meshes are addressed.
  *Done when:* `curl 127.0.0.1:15001/echo/hello` reaches an echo server, unknown prefix → 404 `no_route`.

- [ ] **YAML config: parse → defaults → validate** (CONFIG.md, DESIGN §8) — the hardcoded map
  blocks literally every feature below, since retries, timeouts and ejection are all per-service
  knobs. Include `KnownFields(true)` so typos fail loudly, and the validation rules in CONFIG.md §Validation.
  *Done when:* startup loads `sidecar.yaml`; an invalid file exits 1 with a precise error.

- [ ] **Package layout + lifecycle** (DESIGN §6.1, §9) — move `main.go` → `cmd/sidecar/`, add
  `-config` flag, signal handling, `http.Server.Shutdown` with a drain timeout.
  *Done when:* Ctrl+C drains in-flight requests instead of cutting them.

- [ ] **Per-request timeout** — the single biggest correctness gap right now: a hung upstream
  hangs the caller forever. Start with the simple form (`service.timeout` → `context.WithDeadline`
  → 504 `deadline_exceeded`); full two-header propagation comes in P1.
  *Done when:* a deliberately slow upstream returns 504 at the configured budget.

- [ ] **Access log, one line per request** (DESIGN §11) — you cannot debug retries or ejection
  by reading code. Build this *before* the resilience work, not after.
  *Done when:* every request logs method/service/path/status/attempts/durationMs/sidecarError.

---

## P1 — The features that make it a sidecar (rather than a reverse proxy)

- [ ] **Test harness first: injectable `Clock` + `httptest` upstreams** (DESIGN §12) — retry
  backoff, budget windows and ejection timers are all time-driven. Without a fake clock you're
  testing with `time.Sleep`, which is slow and flaky. This item pays for itself immediately.

- [ ] **Attempt classification + retries** (DESIGN §5.1, §5.2) — the heart of the project.
  Eligibility rules (idempotent methods, `Idempotency-Key`, replayable body, time remaining),
  full-jitter backoff select-ed against the request context.
  *Done when:* table-driven tests cover every row of §5.1.

- [ ] **Outlier ejection + healthy-only balancing** (DESIGN §5.4, §5.5) — today the balancer
  cheerfully round-robins into a dead instance forever. Needs `maxEjectionPercent` so you never
  eject the whole pool.
  *Done when:* killing 1 of 3 upstreams → ejected after N failures, traffic continues, log shows
  `instance_ejected` / `instance_restored`.

- [ ] **Retry budget** (DESIGN §5.2.2) — sliding window per service. Underrated: naive retries
  turn a struggling upstream into a dead one. Ejection without a budget is a retry storm amplifier.

- [ ] **Deadline propagation, both header forms** (DESIGN §5.3) — `X-Request-Timeout-Ms` on the
  wire, `X-Sidecar-Deadline` inside the host. This is what stops service C working on a request
  A abandoned. Use monotonic time internally.
  *Done when:* the A→B→C worked example in §5.3 is reproducible and the budget visibly shrinks per hop.

- [ ] **Inbound listener + context stamping** (DESIGN §3.2) — `:15000`, request-id and
  `traceparent` generation/validation, deadline clamp, forward to the app. No retries, no LB.
  Unlocks true sidecar→sidecar chains; until this exists, propagation is untestable end to end.

- [ ] **Hot reload: mtime poll + atomic table swap + state carry-over** (DESIGN §8) — the part
  people get subtly wrong. Invalid config must keep serving the old table; outlier state and
  budgets carry over by key, or every reload silently un-ejects every broken instance.
  *Done when:* editing YAML changes routing with no restart and no dropped request.

---

## P2 — Hardening

- [ ] Body buffering up to `retry.maxBodyBytes`, hard cap at `limits.maxBodyBytes` → 413.
- [ ] Hop-by-hop header stripping, `Connection`-listed headers, correct `X-Forwarded-For`.
- [ ] Loop guard: reject a config listing this sidecar's own inbound address (CONFIG §Validation 7).
- [ ] Transport tuning: connection pool sizes, `CloseIdleConnections()` after a table swap.
- [ ] `-race` on the whole suite + one end-to-end test with two in-process sidecars.
- [ ] Error-model audit: sidecar errors only when there's no real upstream response to return (§7).

---

## P3 — Demo & polish

- [ ] `docker-compose` demo: A→B→C, each with a sidecar, chaos flags on C (latency, error rate).
      This is what makes the project *showable* — the milestone that turns it into a portfolio piece.
- [ ] README walkthrough reproducing retries, ejection, deadline propagation.
- [ ] `Location` header rewriting so upstream redirects don't leak instance addresses (§10).

---

## P4 — Beyond v1 (explicit non-goals today; listed so the seams stay honest)

- [ ] Prometheus metrics + `/stats` admin endpoint (the logs-only decision will start to hurt around P1).
- [ ] TLS / mTLS behind the `Transport` seam.
- [ ] Active health checks, half-open probing, slow-start after un-ejection.
- [ ] Alternative balancers (least-request, EWMA) behind the `Balancer` interface.
- [ ] gRPC / HTTP2 as a second `Protocol` implementation.
- [ ] Circuit breaking (concurrent-request limits), dynamic discovery / control plane, iptables interception.
