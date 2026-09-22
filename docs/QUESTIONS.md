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

*Status: decided · affects CONFIG.md §services + §Validation 4, DESIGN §6.1, §11 · code: `internal/routing.parseInstance`*

The spike accepted full URLs (`http://10.0.0.7:15000`) because it handed them straight to
`url.Parse`. The configured form is now `host:port`, and `routing` builds the `http` URL itself.

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
DNS name; `routing.TestInstanceAddressForm` pins the accepted forms.

Two forms are refused, both deliberately:

- **An unbracketed IPv6 address** (`2001:db8::7:15000`). Not a policy choice: the last group is
  ambiguously a port or part of the address, so `[2001:db8::7]:15000` is the only readable form
  (RFC 3986 §3.2.2, and what `net.SplitHostPort` / `net.Dial` require). The error says so.
- **A missing port** (`orders-svc`) — see Q5, which is a real choice and could go the other way.

**Cost accepted:** when TLS arrives it needs a config knob of its own (`service.tls: true` or a
transport block) rather than coming free with a per-instance `https://`. That is the right shape
anyway — see Q1.

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

Today `orders-svc` is rejected and `orders-svc:15000` is required. Since every instance is another
sidecar's inbound listener, and that defaults to `:15000`, the port is the same in nearly every
entry — so it could default, leaving `instances: ["10.0.0.7", "10.0.0.8"]`.

*For requiring it:* the demo, `docker-compose` and any host running two sidecars all use
non-default ports, and a silently defaulted port turns a typo into a connect failure at 3am rather
than a config error at startup. It also keeps one address string (D2) instead of a configured form
and an expanded form.

*For defaulting it:* less repetition, and the default is genuinely the common case.

### Q4 — Does `Location` rewriting (DESIGN §10) belong in v1?

Under D1 the rewrite is authority-only (`http://10.0.0.7:15000/v1/x` → `http://orders-svc/v1/x`),
which is much smaller than the path rewrite the prefix design needed. It may be cheap enough to
pull forward from P3.
