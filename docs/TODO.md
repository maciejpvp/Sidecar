# Sidecar — TODO

Ordered by importance: each tier unblocks the next. Within a tier, top items first.
Design rationale for every item lives in [DESIGN.md](DESIGN.md) (section refs below); decisions
already settled are in [QUESTIONS.md](QUESTIONS.md).

Current state: outbound-only proxy on `127.0.0.1:15001`, services addressed by `Host`
(QUESTIONS D1), service table loaded from `sidecar.yaml` with `host:port` instances (QUESTIONS D2,
D3), round-robin with retries on a fresh instance, per-service request deadline, JSON error model,
JSON logger, hot reload of the config file (QUESTIONS D4). End-to-end demo and tests in
[../e2e](../e2e). No inbound listener, no access log, no graceful shutdown.

---

## P0 — Foundation (nothing else is buildable or testable without these)

- [x] **Fix what's already wrong**
  - Outbound now binds `127.0.0.1:15001` (loopback only, or the sidecar is an open proxy into the
    mesh) and exits non-zero with a log line when the bind fails.
  - Instance addresses are `host:port`, parsed with `net.SplitHostPort` and rejected with a precise
    error otherwise — the form CONFIG.md specifies, and the one outlier keys (`service|addr`) and
    the access log will embed. Rationale: QUESTIONS D2. Duplicates within a pool are rejected too.
  - Still hardcoded in `main.go` until the config item below lands.

- [x] **Addressing model decided: `Host` header** (QUESTIONS D1) — not the path prefix the first
  draft assumed. The sidecar claims no part of the app's URL space, so paths and redirects pass
  through unrewritten, and `HTTP_PROXY=127.0.0.1:15001` works with an unmodified HTTP client.
  DESIGN §1, §3.1, §4, §7, §10 and §12 M1 now describe this.
  *Done:* `curl -H 'Host: echo' 127.0.0.1:15001/hello` reaches an echo server, unknown name → 404
  `no_route`; both addressing forms are covered in `e2e.TestRequestFromAToB`.

- [x] **YAML config: parse → defaults → validate** (CONFIG.md, DESIGN §8) — `internal/config`,
  loaded from `-config` (default `sidecar.yaml`). `KnownFields(true)` so a typo fails loudly;
  field-by-field inheritance from `defaults`; every CONFIG.md §Validation rule, instance addresses
  included, lives in `config/validate.go` (QUESTIONS D3). All problems are reported in one go, so
  an operator does not restart once per mistake.
  *Done:* startup loads `sidecar.yaml`; an invalid file exits 1 with a precise error naming the
  field. The whole schema is parsed and validated, but only `listeners.outbound`,
  `limits.maxHeaderBytes`, `log.level`, `instances`, `timeout`, `retry.maxAttempts` and
  `retry.maxBodyBytes` are honoured yet — see the note at the top of CONFIG.md.
  - Next: `transport.go` still holds `backoff.base`/`max` and `minAttemptTime` as constants.
    `routing.Service` embeds `config.Policy`, so wiring them is a matter of reading
    `svc.Retry.Backoff.Base` and friends in place of the constants.

- [ ] **Package layout + lifecycle** (DESIGN §6.1, §9) — move `main.go` → `cmd/sidecar/` (`-config`
  already exists), signal handling — and pass that context to `cfg.Watch` instead of `Background` — `http.Server.Shutdown` with a drain timeout.
  *Done when:* Ctrl+C drains in-flight requests instead of cutting them.

- [x] **Per-request timeout** — `service.timeout` → `context.WithTimeout` → 504
  `deadline_exceeded`, covering the response body as well as time to first byte. Full two-header
  propagation is still P1, and `perTryTimeout` is still unimplemented (QUESTIONS Q2 — which is why
  a *hanging* upstream gets one attempt and no retry).
  *Done:* `e2e.TestDeadlineExceeded` and `proxy.TestRetriesStopAtDeadline`.

- [ ] **Access log, one line per request** (DESIGN §11) — you cannot debug retries or ejection
  by reading code. Build this *before* the resilience work, not after.
  *Done when:* every request logs method/service/path/status/attempts/durationMs/sidecarError.

---

## P1 — The features that make it a sidecar (rather than a reverse proxy)

- [ ] **Test harness first: injectable `Clock` + `httptest` upstreams** (DESIGN §12) — retry
  backoff, budget windows and ejection timers are all time-driven. Without a fake clock you're
  testing with `time.Sleep`, which is slow and flaky. This item pays for itself immediately.

- [~] **Attempt classification + retries** (DESIGN §5.1, §5.2) — landed ahead of the items above:
  classification, retry onto a fresh instance, full-jitter backoff select-ed against the request
  context, body buffering and replay, laps so a single-instance service still retries.
  Still missing: `Idempotency-Key` for POST/PATCH (§5.2.1 rule 2), the retry budget (below), and
  the per-attempt timeout that makes §5.1's timeout row reachable at all (QUESTIONS Q2).
  *Done when:* table-driven tests cover every row of §5.1 — currently every row except that one.

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

- [~] **Hot reload: mtime poll + atomic table swap + state carry-over** (DESIGN §8) — `config.Source`
  polls, validates and stores; `routing.Store` swaps the table; idle
  connections are closed after a swap. Invalid files are rejected once and the old table keeps
  serving; restart-only fields keep their running value (QUESTIONS D4).
  *Done:* editing YAML changes routing with no restart (`e2e.TestHotReloadReroutes`).
  Still open: outlier state and budgets carrying over by key — nothing to carry until those
  features exist, but it must land *with* them, or every reload silently un-ejects every broken
  instance.

---

## P2 — Hardening

- [~] Body buffering up to `retry.maxBodyBytes` (done, per service), hard cap at `limits.maxBodyBytes` → 413 (not yet).
- [ ] Hop-by-hop header stripping, `Connection`-listed headers, correct `X-Forwarded-For`.
- [x] Loop guard: reject a config listing this sidecar's own inbound address (CONFIG §Validation 7) —
      landed with config validation, including `0.0.0.0:15000` vs `127.0.0.1:15000` spellings.
- [~] Transport tuning: connection pool sizes. (`CloseIdleConnections()` after a table swap is done.)
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
