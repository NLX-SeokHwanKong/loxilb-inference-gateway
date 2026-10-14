# Changelog

All notable changes to loxilb-inference-gateway are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).
Because this is a fork of [loxilb](https://github.com/loxilb-io/loxilb), releases
follow upstream's version scheme — `vMAJOR.MINOR.PATCH` with an optional fourth
build component (e.g. `v0.9.8.7`), plus `-rc.N` for a release candidate — so the
upstream baseline is readable at a glance. The reported version is derived from
the release tag at build time, never pinned in source. Changes inherited from an
upstream loxilb sync are summarized here by their upstream version; only the
inference-gateway delta is enumerated in detail. See
[docs/UPSTREAM-SYNC.md](docs/UPSTREAM-SYNC.md) for the sync and tagging model.

## [Unreleased]

First inference-gateway release, forked from upstream loxilb `0.9.8.6-beta`.
Every AI capability is opt-in per load-balancer rule; with none enabled the
binary behaves exactly like upstream loxilb.

### Added

- **KV-cache-aware routing** for LLM serving fleets: cache-locality-aware
  endpoint selection driven by the serving engines' native contracts
  (vLLM `--kv-events-config` ZMQ events; SGLang radix-cache semantics).
- **Prefill/decode (P·D) disaggregation** routing, including KV-exact mode over
  a prefill/decode topology (`kvExactMode:1` + endpoint role, as shipped for
  vLLM) and single-role KV-exact over a role-less pool (`kvExactMode:3`, as
  shipped for SGLang). `kvExactMode` selects the topology; the serving engine
  and its block-hash contract are selected independently by `kvEngineType`.
- **Consistent-hash cache routing** (CHWBL) for aggregated vLLM pools.
- **MCP gateway** support: session-sticky routing for Model Context Protocol
  server pools.
- **AI gateway controls**: per-rule API-key auth and rate limiting, armed via
  `sse_mode` on L7 fullproxy rules.
- **AI control plane**: `loxilb-ai-controller` (TTFT-adaptive controller) and
  `loxilb-kv-agent` (DOCA) components, plus AI metrics export.
- **eBPF data-path extensions** for inference routing, tracked in the
  `loxilb-ebpf-inference-gateway` fork and pinned via the `loxilb-ebpf`
  submodule.
- **REST API / swagger** extensions for all AI fields (`api/swagger-extras.yml`
  and generated models).
- **Documentation** under `docs/load-balancing/` covering AI gateway L7, REST
  API reference, KV-cache-aware routing (incl. hierarchical and P·D deep dives),
  MCP gateway, and AI gateway controls.
- **CI**: `ai-gateway-sanity`, `mcp-sanity`, `l7-proxy-sanity`, and
  `vllm-proxy-sanity` workflows, plus the automated upstream-sync chain
  (`upstream-sync.yml`, `ebpf-pin-bump.yml`).
- **Token quotas (tokens-per-minute)** for AI gateway rules: per-tenant and
  per-tenant-and-model budgets enforced from the tokens the serving engine
  reports. Usage is read from the engine's own `usage` block, with
  `stream_options.include_usage` injected on streaming requests so a client
  cannot opt out of accounting. Requests are checked **before** dispatch —
  the prompt plus any declared completion ceiling is reserved up front and
  reconciled at completion — so an over-budget request is refused with `429`
  instead of being detected after the GPU has already been spent. Quota is a
  smooth token bucket, so a tenant recovers continuously rather than at a
  fixed window boundary, and idle per-tenant state is evicted. Configured via
  `tokens_per_min` on the tenant rate-limit API and optional `model_limits`
  for per-model budgets. Bucket capacity — how much of a minute's budget a
  fully idle tenant may spend at once — defaults to one whole minute and can be
  narrowed or widened per tenant with `burst_pct` on the same API.
- **Traffic policing and shaping**, layered:
  - **Load-balancer-rule policers** (eBPF) — a bandwidth budget attached to a
    rule rather than a port, applied to both directions of the session.
  - **Egress-direction port policers** behind `--egr-hooks`, applied to
    host-originated egress and to transit traffic leaving through the port.
    Transit is policed after the forwarding decision, against the port the
    packet actually leaves by, so both classes meet the same budget.
  - **An L7 byte shaper** for full-proxy AI rules, which *paces* payload bytes
    to the configured rate in either direction instead of dropping them.
    Shaper-paused time is excluded from idle and stream-duration reaping, so
    pacing a long response or an SSE stream cannot manufacture a disconnect.
- **Observability for both of the above**: Prometheus series for token
  consumption (split by how the count was obtained), quota utilization and
  limits, quota denials, and per-service shaper counters — bytes passed and
  delayed, pause count and duration, currently-paused connections, and the
  configured rate and burst. The bundled AI dashboard gains a token-quota row
  and a byte-shaper row. `GET /metrics` is exempt from management-plane bearer
  auth so a scraper needs no credential.

### Changed

- Container image renamed to `ghcr.io/loxilb-io/loxilb-inference-gateway`.
- README rewritten around inference-gateway use cases; `loxilb-ebpf` submodule
  repointed to the `loxilb-ebpf-inference-gateway` fork.
- **Audit trail: a request authenticated by a bearer token (JWT) is recorded
  with `actor.auth: "bearer"`.** It used to be recorded as `"none"`, the
  value an unauthenticated request has, because the data-path actor only
  knew API keys. The new value is written on `data.ai.complete`,
  `data.ai.settle` and `sec.ai.deny` once the token has validated; a request
  whose token did not validate is still `"none"`, and API-key requests are
  still `"apikey"`. `schema_version` stays `1`. **SIEM impact:** a rule or
  dashboard that matches `actor.auth == "none"` no longer matches JWT
  traffic, and a consumer that validates `actor.auth` against a closed list
  must accept `"bearer"` (and `"network"`, written when a network-identity
  binding decided the tenant).

- **The default reservation is per rule.** Where several rules share a VIP
  and port (told apart by host, path or model), a request without
  `max_tokens` now reserves by `ai_default_reservation` /
  `ai_default_max_tokens` of the rule it routes to. Before, the rule
  installed first on the listener decided for all of them. With it go the
  reply text `Success; warning: ai_default_reservation: …` on a rule whose
  neighbours resolve differently, and the value `listener` of
  `fc_effective.source.default_reservation` / `.default_max_tokens`: a
  rule's read-back is its own. A client that matched either should stop.
  A request an L7 policy forwards to another pool reserves by the rule its
  host, path and model select.
- **Log calls no longer wait for the log's device.** The gateway's log and
  the data-plane log are written by one writer each, from an in-memory queue;
  a worker thread that logs copies its line and returns. When the device is
  slow or stalled, lines are dropped and counted in the new
  `loxilb_log_lines_dropped_total{level}` and the writer reports the drop in
  the log; requests are not held. Before, one stalled log write stopped every
  connection of the worker that made it. The data-plane log is now rotated by
  its own writer (rename and reopen; no line lost at a rotation). The stdout
  copy of the gateway's log is unchanged except that a message with no
  trailing newline now gets one. See `docs/LOG-ROTATION.md`.
- **Per-request log lines of the AI gateway moved from `info` / `warn` to
  `debug`**: key validated, empty or invalid key, expired key, model not
  allowed, rate-limited, token quota exceeded, pre-admission denied, capacity
  refused, request refused 413, stream start / end. Each of these is in the
  audit trail (`data.ai.complete`, `sec.ai.deny`) and the metrics. **Impact:**
  a log-based alert or a script that matches these lines sees them only at
  `--loglevel debug`; use the audit trail or `loxilb_ai_*` counters instead.
  Store-outage and fault lines are unchanged.

### Fixed

- **Bytes a client sends while a streamed answer is running are no longer
  relayed to the backend.** On a rule with `sse_mode`, anything a client sent
  on an HTTP/1.1 connection while its streamed answer was still in progress
  went to the backend behind the request being answered, without passing the
  admission gate: a second request sent early was served with no credential
  checked, no model allowlist, no rate limit and no charge. On a JWT service
  whose profile has `forward_identity` the gateway also wrote the first
  request's verified `X-Auth-Tenant` / `X-Auth-User` into those bytes, so the
  backend read them as an authenticated request. The gateway now closes the
  connection when a client sends anything before its streamed answer has
  ended; the unfinished stream is counted as one its client left, and each
  such close is counted in `loxilb_ai_stream_early_bytes_closed_total{service}`
  and logged once at debug. **Impact:**
  a client that pipelines a request behind a streamed answer on HTTP/1.1 has
  the connection closed and must send that request on a new one (or wait for
  the answer to end, or use HTTP/2). Clients that wait for their answer are
  not affected.
- **A request or response body is no longer edited as if it were a header
  block.** On the path that relays a connection's later reads, any read that
  held a blank line was taken for a header block: on a service that declares
  `api_key_auth`, lines beginning `X-Api-Key:` (and on a service with a
  forwarding network-identity binding, lines beginning `X-Auth-`) were
  removed from it, in a request body, in a backend's response headers and in
  a response body alike, leaving the message shorter than its
  `Content-Length`. Header lines are now removed only from the header block
  of a request, once, and a response body is never touched.
- **A backend's `X-Api-Key` response header is removed on purpose, where it
  was removed by accident.** On an HTTP/1.1 service that declares
  `api_key_auth` (any declared value) the gateway already removed a
  backend's `X-Api-Key` response header, as a side effect of the defect
  above. It still does, now as a rule: `X-Api-Key` is the gateway's
  credential header on such a service and a backend does not get to hand a
  key to a client under that name. The header is removed from the response
  head only, when the head arrives whole in one read. Unchanged from earlier
  releases: a backend's `X-Auth-*` response headers pass through on every
  service; on a service that makes no `api_key_auth` declaration, on HTTP/2,
  and on a rule whose response direction is carried by `sockMapMode`, a
  backend's `X-Api-Key` passes through as well.

### Notes

- This release preserves full upstream loxilb functionality; classic L4/L7
  load balancing is unchanged and documented upstream.

### Upgrade notes

- **`tenant_rate_limits` gains a `burst_pct` column.** The gateway adds it on
  start-up if it is missing, so an existing database needs no manual step and
  an already-migrated one is left alone. The column defaults to `0`, which
  means "use the server default", so tenants configured before this release
  keep exactly the capacity they had.

- **Token-quota state does not interoperate across this change in an HA pair.**
  Quota is synchronized between peers over an unchanged wire format, and this
  release reuses one of its existing integer fields to carry a bucket
  drain-time timestamp instead of a consumed-token count. The two are merged
  by taking the larger value, which makes the incompatibility **one-way**:

  - **New peer → old peer is harmful.** The old build reads a millisecond
    timestamp as a token count, concludes the tenant is enormously over
    budget, and denies its traffic with `429` until the entry ages out.
  - **Old peer → new peer is harmless.** A token count interpreted as a
    timestamp lands far in the past, so it loses the merge to the new peer's
    own value and cannot deny anyone.

  So a **rolling upgrade of an HA pair is not supported while token quotas are
  enabled.** Prefer upgrading both peers together.

  If the peers must be upgraded one at a time, set `tokens_per_min` to `0`
  **before** upgrading the first one. Quota entries are only created when a
  limit is configured, so a peer that restarts into the new build with quotas
  disabled never creates quota state and therefore never sends any. Ordering
  matters: disabling quotas does not retract entries that already exist — the
  upgraded peer starts with an empty map, but a peer left running keeps its
  entries until they idle out (about 10 minutes by default, `LLB_AI_QUOTA_EVICT_WINDOWS`).
  Re-enable quotas once both peers run the same build.

  Request-rate limits, API-key authorization and model authorization are
  unaffected and may stay enabled throughout. This applies only to peers
  exchanging quota state — a standalone gateway upgrades normally.

<!--
Template for future releases:

## [vX.Y.Z.W] - YYYY-MM-DD
### Added
### Changed
### Fixed
### Upstream
- Synced loxilb <upstream-version> / loxilb-ebpf <pin>.
-->
