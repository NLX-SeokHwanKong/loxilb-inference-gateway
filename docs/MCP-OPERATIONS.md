# loxilb-mcp operations guide

`loxilb-mcp` is the standalone MCP (Model Context Protocol) bridge for
loxilb-inference-gateway. It lets MCP clients (Claude Code, MCP Inspector,
custom agents) observe, manage, and diagnose loxilb instances through guarded
tools instead of raw REST.

- Install: `brew install --cask loxilb-io/tap/loxilb-mcp` (macOS), the
  `ghcr.io/loxilb-io/loxilb-mcp` container image, `go install`, or a tarball from
  the `mcp/vX.Y.Z` releases — per-OS steps and MCP-client wiring in
  [`../mcp/README.md`](../mcp/README.md). From source: `cd mcp && go build ./cmd/loxilb-mcp`
  (it is a standalone Go module, released independently of the datapath).
- Talks to loxilb REST (`:11111 /netlox/v1`), optionally Prometheus and
  Alertmanager
- No loxilb code changes; purely additive

## Quick start

```sh
# Local dev: stdio transport, single target, admin role
loxilb-mcp --target-url http://127.0.0.1:11111

# Claude Code
claude mcp add loxilb -- /path/to/loxilb-mcp --target-url http://127.0.0.1:11111

# Production shape: config file + streamable HTTP on loopback (SSH tunnel in)
loxilb-mcp --config /etc/loxilb-mcp.yaml --transport http --listen 127.0.0.1:8891
```

## Configuration file

```yaml
default_target: llb1
targets:
  llb1:
    url: http://172.17.0.2:11111
    # username/password_env or token_env when loxilb runs --userservice
    # tls_ca / insecure_skip_verify / timeout_sec as needed
clients:                       # HTTP-mode bearer tokens, one per client
  # Each client has a request budget: a token bucket of `burst` requests
  # refilled at `rps` per second (default 20 / 10). A request past it is
  # answered 429 with Retry-After and a JSON body; a budget cannot be
  # turned off (rps >= 1/60, burst >= 1).
  - { name: dashboard, role: viewer,   token_env: MCP_VIEWER_TOKEN, rps: 20, burst: 60 }
  - { name: oncall,    role: operator, token_env: MCP_OPERATOR_TOKEN }
  # mutations_per_minute gives one client its own mutation budget (below).
  - { name: sre,       role: admin,    token_env: MCP_ADMIN_TOKEN, mutations_per_minute: 12 }
# The mutation budget (§ the change gate): at most mutations_per_minute
# mutations sent per sliding minute per caller, and a stop after
# max_consecutive_failures refused mutations in a row until a read-only
# tool call succeeds. Defaults 6 and 3; neither can go below 1.
change_budget: { mutations_per_minute: 6, max_consecutive_failures: 3 }
audit_dir: /var/lib/loxilb-mcp          # JSONL audit log (0600)
secrets_dir: /var/lib/loxilb-mcp/secrets # ai_apikey_create key files (0600)
prometheus_url: http://127.0.0.1:9090    # enables promql_query/promql_range
alertmanager_url: ""                     # enables alerts_active when set
alert_rules_path: deploy/monitoring/prometheus/rules/loxilb-alerts.yml
openapi_spec_path: api/swagger.yml       # served as loxilb://spec/openapi
autopilot_tools: []                      # exact names of destructive tools allowed to skip confirm (§ Autopilot)
```

Tokens must be ≥16 bytes (32 random bytes recommended). Target names are the
only accepted `target` values in tool calls — URLs are rejected (anti-SSRF).

## Roles and tool tiers

| Role | May call | Tool count (base) |
|---|---|---|
| viewer | read-only tools only | 68 |
| operator | + object- and tenant-scope mutations (preview by default, `apply: true` as the shortcut) | 79 |
| admin | + admin-confirm tools (every delete and every global-, service- or instance-scope change, confirm-token gated) | 100 (101 with `--allow-import`) |

The tier of a tool is derived from the impact of its REST operation
(`mcp/contract/coverage.yml`, checked in CI): a read is viewer; a reversible
change to one object or one tenant is operator; a change to a service's
traffic, to every tenant/service (global) or to the instance, and every
delete, is admin-confirm. `lb_create` is listed for operators because a POST
on a new key creates a rule (operator); a POST on an existing key would
replace it, and that path is admin-confirm inside the tool (below).

"Base" is with all four domains on and no external monitoring backends
configured. A configured `prometheus_url` (`promql_query`, `promql_range`),
`alertmanager_url` (`alerts_active`), and `alert_rules_path` (`alerts_catalog`)
each register additional read-only tools that are visible to every role — up to
four more, so a fully-wired admin sees 104 (105 with `--allow-import`). `--read-only`,
`--enable-domains`, and `--allow-tools`/`--deny-tools` reduce the set further.

Stdio sessions take the role from `--role` (default admin — stdio inherits
the local user's authority). HTTP sessions take it from the bearer token.
Every management call the bridge makes names who it acts for in
`X-Loxilb-Originator`: `mcp:<client name>` for an HTTP session, and for a
stdio session `mcp-stdio:<os user>@<host>:<pid>` — there is no
authenticated client over stdio, so the OS account that launched the bridge
is the finest attribution the gateway's audit trail can carry. The gateway
records the value beside the bridge's own account and trusts it only when
that account carries `delegation_allowed` (docs/MANAGEMENT-API-EXPOSURE.md).
`tools/list` reflects exactly what the caller may do — treat it, not this
table, as authoritative for a given configuration.

Additional gates, all composable: `--read-only`, `--allow-tools` /
`--deny-tools` globs (deny wins), `--enable-domains mgmt,analysis,monitoring,ai`.

## The change gate: preview, confirm, read-back, budget

Every admin-confirm tool is two-step:

1. Call without `confirm_token` → nothing changes. The tool reads the object
   it would change and answers `action:"preview"` with `current` (as read
   from the API), `proposed` (the state the call would produce), `diff`
   (the keys that change, each as `{from, to}`), `rollback` (`{tool, args}`
   that restores `current`, or a `note` saying why no single call can),
   `impact` (the manifest's blast-radius class) and a `confirm_token`.
2. Repeat the call with **identical arguments** plus the token within 120 s.
   The executed result carries `read_back`: the object re-read from the API
   after the change, so the effect is visible before the next step.

Tokens are single-use and SHA-256-bound to (tool, target, arguments); any
argument change or replay burns them. `--no-confirm` (CI only) skips the
flow. `config_import` additionally requires `--allow-import`.

**Operator writes preview too.** The object- and tenant-scope writes
(`ai_apikey_create`, `ai_apikey_update`, `ai_ratelimit_set`,
`ai_user_ratelimit_set`, `llamafw_health_check`) answer the same preview
when called without a token, and apply with it; at operator tier they
also accept `apply: true` to skip the preview (§3.6 item 1 of the
campaign guide). `ai_user_ratelimit_delete` is operator too (the row is
re-creatable and the preview carries it as the arguments of an
`ai_user_ratelimit_set` rollback) but has no `apply` shortcut: a removal
always previews.

**The mutation budget** (change-gate requirement 4, D12) stops a runaway
agent before the gateway does. Per caller — the authenticated HTTP client,
or the stdio session — the bridge sends at most `mutations_per_minute`
mutations (default 6) in any sliding minute; the next one is refused with
the time until a slot frees. After `max_consecutive_failures` (default 3)
mutations in a row were refused by the gateway, no further mutation is
sent until a read-only tool call succeeds (the agent has to look at the
state the failures left before it changes it again). A refusal is an
error the agent can read (`mutation budget: 4 of 4 mutations per minute
already sent for mcp:oncall; nothing was sent. Retry in 37 s, ...`);
nothing queues, and a confirm token whose apply was refused by the budget
stays usable until it expires. Previews and reads are never counted. The
knobs are `change_budget:` in the config file and `mutations_per_minute`
on a `clients:` entry; zero keeps the default and nothing goes below 1
per minute, so the budget is a brake, never a switch. A harness that
replays a session's worth of changes in a minute sizes its client up
(`cicd/mcp/validation.sh` gives its CI clients 120) rather than pacing.

**`lb_update`** (admin-confirm, impact service) changes a rule in place
through `PATCH /config/loadbalancer/externalipaddress/{ip}/port/{port}/protocol/{proto}`.
It accepts only `admin_state_up`, the weight of existing endpoints
(`endpoint_weights`; the gateway replaces the endpoint list when the key
is present, so the bridge sends the full current list with the weights
swapped) and `fc_mode`, `fc_max_outstanding`, `fc_max_queue_depth`,
`fc_max_queue_wait_ms`; the schema refuses any other argument and the
description names `lb_create` (replace) and `lb_delete`. The capacity gate
runs only on AI-gateway rules, and every one is fullproxy, which the
gateway does not PATCH: `fc_*` on a fullproxy (mode 4) rule is refused by
the bridge from its pre-read, before any request, naming the `lb_create`
replace (read the gate in force back with `lb_get`, `fc_effective`); `fc_*`
on any other mode is sent, and the preview's rollback note and the
read-back's `fc_note` say that no gate runs there. The call
pre-reads the rule (a missing key is refused naming `lb_create`), previews
`current`, `proposed`, `diff` and an `lb_update` rollback with the current
values (a note names fields the rule had not declared, which the PATCH
cannot un-declare), and the executed result re-reads the rule and its
`GET .../status`. The gateway serialises `adminStateUp` with omitempty: an
enabled rule's GET carries an explicit `true`, a paused rule's GET (and its
status) carries no flag. `lb_update` reports the flag as read — `true`,
`false` or `"absent"` — in `current`; for an absent flag the rollback states
no `admin_state_up` and its note says why. Only the read-back after the call
sent `admin_state_up: false` resolves an absent flag to `false`, with
`admin_state_up_note` saying so. A PATCH the gateway refuses — a fullproxy (`mode 4`)
rule, an immutable field — is passed on as is with one hint naming the
replace path (D8).

**`secrate_disable`** (admin-confirm, global, never autopilot; D18) is
`DELETE /config/securityrate`: the only way back to the unconfigured
state, because `POST /config/securityrate` refuses a body that keeps every
protection off. Its preview carries the current configuration as the
arguments of a `secrate_set` rollback, and `secrate_set`'s own rollback on
a target that had no protection on names it.

**`config_persist`** (admin-confirm, instance, never autopilot; D22) is
`POST /config/persist`. The preview's `current` is the running snapshot's
identity (schema, checksum, generation); the write overwrites
`<config-path>/snapshot.json`, which cannot be read back, so the rollback
is a note. While the gateway's auto-persist holds the snapshot gate (about
3 s after a config write) the read or the write answers 409; the tool
returns `snapshot_gate_busy` with `retry_after_sec: 3` and does not retry.

The deletes (`lb_delete`, `fw_delete`, `net_route_delete`,
`ai_apikey_delete`, `config_import`) preview the object(s) they would
remove under `preview`. The global- and instance-scope changes raised to
this tier in PR-A2 — `secrate_set`, `secrate_reset`, `bgp_neigh_set`,
`bgp_global_set`, `bgp_policy_apply`, `config_params_set`, `gpu_mode_set`,
`gpu_conversations_cleanup`, `llamafw_enable_set`, `llamafw_configure`,
`llamafw_scanners_set`, `pii_enable_set`, `pii_configure`,
`pii_url_patterns_set` — preview `current`/`proposed`/`diff`/`rollback`.
Where the REST contract has no read of the object (`bgp_global_set`,
`bgp_policy_apply`) the preview shows the proposed values only and the
rollback note says so; where the change is not reversible (`secrate_reset`,
`gpu_conversations_cleanup`) the note says that. `secrate_set` merges the
supplied fields into the configuration it read and sends the full result,
because the REST body requires every threshold; a key the GET omits is
read as its zero value. A proposed configuration that enables no
per-source protection and sets no aggregate budget is refused before any
token is issued, because `POST /config/securityrate` refuses such a body;
on a target that had no protection enabled the preview's rollback is
`secrate_disable` instead of a `secrate_set` call into that refused state.

`lb_create` pre-reads `GET /config/loadbalancer/externalipaddress/{ip}/port/{port}/protocol/{proto}`
(and, on a shared VIP:port, the full list for the host/path/model key)
before it sends anything. No rule at the key: the POST creates one and
executes directly at operator tier. A rule at the key: the POST would
replace it, so the call previews the current rule against the proposed
body with a rollback that re-creates the current rule, and applies only
with the token; an operator session is refused on that path with a message
naming `lb_delete` and the admin-confirm tier, before any request is built.
`llamafw_health_check` (a trigger, operator tier) previews the status it
probes and accepts `apply: true` to run without the token.

### Autopilot (default off)

`--autopilot-tools lb_delete,...` (or `autopilot_tools:` in the config) names
destructive tools that execute **without** the preview→confirm step. Names are exact; globs are
rejected. Role tiers still apply (destructive tools remain admin-only), the
bypass writes an `autopilot_exec` audit event, and startup logs a warning
listing the tools. `diagnose_*` suggested actions carry `autopilot:
true|false` so an agent knows which follow-ups it may run directly. Leave
empty until the team trusts the audit trail for the specific action.

Some tools can never be put on autopilot, whatever the configuration says:
`guard.NeverAutopilot` holds every tool whose change is global (every tenant
or service: BGP, security rate including `secrate_disable`, GPU mode,
LlamaFirewall, PII) or instance-wide (`config_import`, `config_params_set`,
`config_persist`, and the planned
`maintenance_set`, objective / network-identity / JWT-profile / rate-limit
defaults writers, half-close release, sockmap reset, OTLP and OPA
destinations). Listing one of them in `autopilot_tools` is ignored and logged.
The contract gate (below) keeps the set a superset of every manifest entry
whose impact is `global` or `instance`.

## Secrets handling

`ai_apikey_create` never returns raw key material by default: the key is
written to `secrets_dir` as `apikey-<key_id>.key` (file 0600, dir 0700) and
only the path is returned (`result.key_file` of the executed call).
`reveal:true` returns it inline instead (`result.raw_key`) — the key then
enters model/client context; use only when the caller is the end user.
`config_export` masks secret-shaped fields. Audit lines redact secret-shaped
arguments.

## Tool catalog

**Seed / health** — `version_get`, `health_overview`, `lb_list`, `ct_list`,
`metrics_snapshot`. `version_get` returns `product` and `features[]` from
`GET /version` when the build reports them (`loxilb-inference-gateway`;
tokens such as `epp`, `api_key_auth`, `jwt_auth_profile`), so an agent can
tell a gateway-only or EPP-capable build from upstream loxilb before it sends
the matching rule fields. `health_overview` carries the same two fields plus
`ready` / `ready_reasons` from `GET /status/ready` (a 503 not-ready answer is
reported as a verdict, not an error; a build without the endpoint shows
`errors.ready`).

**Fleet (read)** — `targets_list` (configured targets + default),
`fleet_overview` (concurrent health probe of every target with the same
`product`/`features`/`ready` fields; unreachable targets degrade into
per-target error sections).

**Analysis (read)** — `meta_get`, `cluster_state_get`, `trace_status_get`,
`l4trace_status_get`, `trace_catalog_list`, `nodegraph_get` (backend 501 in
this fork), `status_get`, `logs_tail`, `log_archives_list`, `log_archive_get`,
`ipsec_status_get`.

**Monitoring (read)** — `metrics_config_get`, `metrics_legacy_get`,
`promql_query` / `promql_range` (needs `prometheus_url`), `alerts_active`
(needs `alertmanager_url`), `alerts_catalog` (needs `alert_rules_path`).

**Management** — read: `lb_get` (one rule by composite key or opaque id, `include: [status, stats]`), `lb_kvexact_status` (the resolved KV-exact composition status of a key, the one read that says whether KV-aware routing is in force), `status_capabilities`, `maintenance_get`, `halfclose_get`, `audit_status`, `audit_policy_get`, `audit_sink_get` (primary sink, or a secondary by name), `config_snapshot_get` (size-capped, secret-shaped fields masked; the gateway's 409 while its auto-persist holds the snapshot gate, about 3 s after a config write, comes back as the retryable error `{error: snapshot_gate_busy, retry_after_sec: 3, detail}` and is not retried by the tool), `endpoint_list`, `fw_list`, `ipfilter_list`,
`secrate_get`, `net_route_list`, `net_vlan_list`, `net_vxlan_list`,
`net_neighbor_list`, `net_ip_list`, `net_port_list`, `bgp_neigh_list`,
`bgp_policy_list`, `session_list`, `session_ulcl_list`, `config_params_get`,
`config_export`. Operator+: `lb_create` (create path; the replace path is
admin-confirm), `endpoint_host_state_set`, `fw_create`, `ipfilter_set`,
`net_route_create`. Admin+confirm: `lb_update` (phase-1 in-place change:
`admin_state_up`, endpoint weights, the four `fc_*` fields), `lb_delete`,
`fw_delete`, `net_route_delete`, `config_import`, `config_persist`,
`secrate_set`, `secrate_disable`, `secrate_reset`, `bgp_neigh_set`,
`bgp_global_set`, `bgp_policy_apply`, `config_params_set`.

`lb_create` exposes typed AI routing arguments instead of requiring callers to
guess Gateway JSON casing: `host`, `path_prefix`, `model_name`, `sse_mode`,
`pd_disagg_mode`, `pd_cache_aware_mode`, `api_key_auth`, `jwt_auth_profile`,
`kv_exact_mode`, `kv_engine_type`, `kv_zmq_port`, `kv_block_size`, the
flow-control fields `fc_mode`, `fc_max_outstanding`, `fc_max_queue_depth`,
`fc_max_queue_wait_ms`, `fc_ep_max_inflight`, `fc_prefill_max_inflight`,
`fc_decode_max_inflight`, `fc_tenant_max_share_pct`, the endpoint-picker
fields `epp_endpoint`, `epp_failure_mode`, `epp_timeout_ms`, `epp_plaintext`,
the admission fields `ai_default_reservation`, `ai_default_max_tokens`,
`ai_objective_strict`, and `security`, `admin_state_up`, `id`, plus
endpoint `ep_role` and `nixl_port`. The bridge maps those names to the REST
contract. The permitted values and descriptions of `sel`, `mode`,
`api_key_auth`, `fc_mode`, `epp_failure_mode`, `ai_default_reservation` and
`security` are generated from swagger (`zz_swagger_enums_gen.go`) and
validated before any request is built, so the schema names every selector
(CHWBL, GPU-aware, WRR-hash, ...) and every credential policy the API knows.
Omit `api_key_auth` to preserve a backend-owned `X-Api-Key`; `disabled` and
`required` both reserve and strip that header, while only `required` enforces
an API key; `jwt` validates a Bearer JWT against `jwt_auth_profile`, and
`apikey-or-jwt` accepts either credential. Both JWT modes require
`jwt_auth_profile` to name a configured profile, and the bridge refuses the
call without it. `lb_list` projects the same routing fields and endpoint
roles, plus `id`, `admin_state_up`, `security`, `jwt_auth_profile`,
`epp_endpoint`, `kv_exact_api_mode`, `kv_model_profile` and the three
data-plane read-back values `fc_effective`, `backend_tls_effective`,
`half_close_effective`, so an agent sees what was configured and what is in
force. `admin_state_up` is tri-state in `lb_list` and `lb_get`: `true`,
`false`, or `"absent"` when the rule's GET carries no `adminStateUp` — on this
gateway the form a paused rule reads as (omitempty), while an enabled rule
always carries `true`. `service_extra` remains for
untyped advanced fields and rejects collisions with supplied typed arguments.
Its keys are validated before any request is built: each must be a
`serviceArguments` property of the REST contract (the list is generated from
`api/swagger.yml`), read-back values (`fc_effective`,
`backend_tls_effective`, `half_close_effective`) are refused, and so is any
property `mcp/contract/fields.yml` marks `excluded-field`.

**AI-gateway ops** — read: `ai_apikey_list`, `ai_apikey_get`,
`ai_ratelimit_get`, `ai_objective_list` (with the source the gateway applies them from), `ai_netidentity_list`, `ai_jwtprofile_list` (issuer, JWKS URL, claims; no secrets), `ai_user_ratelimit_list`, `ai_user_ratelimit_get`, `ai_ratelimit_defaults_get`, `ai_model_profile_list`, `ai_model_profile_get`, `ai_kv_inventory_get`, `gpu_status`,
`gpu_worker_metrics_get`, `llamafw_status`, `llamafw_stats`, `pii_status`,
`pii_stats`, `ai_traffic_report`. Operator+ (preview by default, `apply:
true` to skip it): `ai_apikey_create` (typed `enabled`, `client_op_id`,
`priority_class`, `default_objective`, `allowed_objectives`; no key import),
`ai_apikey_update` (the full PATCH surface), `ai_ratelimit_set`
(`model_limits`, `priority_class`, `default_objective`,
`allowed_objectives`; no delete endpoint — set quotas 0 to lift),
`ai_user_ratelimit_set`, `ai_user_ratelimit_delete` (preview only),
`llamafw_health_check`. Admin+confirm: `ai_apikey_delete`,
`gpu_mode_set`, `gpu_conversations_cleanup`, `llamafw_enable_set`,
`llamafw_configure`, `llamafw_scanners_set`, `pii_enable_set`,
`pii_configure`, `pii_url_patterns_set`.

> **Prerequisite:** the API-key and tenant-rate-limit REST endpoints are only
> wired when the target loxilb runs with `--userservice` (plus a reachable
> `--mgmt-db-host`). On a non-userservice target these tools return the
> target's HTTP 501 verbatim. Note also: with `--userservice` enabled,
> `/netlox/v1/metrics` requires a JWT, so a Prometheus instance scraping that
> target needs a bearer-token scrape config.

**Diagnostics / RCA (read)** — `diagnose_l4_errors`, `diagnose_ai_latency`,
`diagnose_endpoint`, `capacity_report`. Each returns a correlated evidence
bundle (sections degrade independently into `errors`) plus machine-readable
`suggested_actions[] {tool, args, rationale, risk}`. Every bundle starts with
`evidence.diagnostics`, a sanitized projection of `GET /diagnostics` (build
identity, uptime, `ready` and its reasons, maintenance state, eBPF
attachments, map utilization, dependency checks), so a finding is read
against the gateway's own verdict; a build without the endpoint shows
`errors.diagnostics`. `diagnose_ai_latency` also reads, from the same
scrape: `admission` per pool (mode, the `role="service"` units held, the
ceiling in force, `queue_wait`, `at_ceiling` under enforce),
`pd_admission_sheds`, `epp` (decision latency, rejected candidates by
reason, pending decisions), `kv_freshness` per endpoint (connected,
inventory fresh, the last event's age against the bridge's clock; a
`problem` of `disconnected` or `inventory_not_fresh`) and `quota_denied`
per tenant (the token-quota refusals, split into `exhausted` and
`would_exceed` by the reason on `loxilb_ai_rate_limit_hits_total`). A
section the gateway emits nothing for is left out. The KV series' `service`
label is the gateway's numeric service id on current builds (the id
`ai_kv_inventory_get` takes; no REST read maps it to a rule), and a
connected subscriber with no last-event series reads `no_event_accepted`.
It suggests `lb_get` for a pool at its ceiling, `ai_kv_inventory_get` (or
`lb_kvexact_status` where the label is a VIP:port) for a cold or
disconnected subscriber, and `ai_ratelimit_get` for a tenant refused by a
token quota.
`ai_traffic_report` adds `tokens_consumed` (model, tenant, kind) with
prompt and completion totals, `quota_denied` per tenant and the non-zero
`fc_band` rows (service, band, outcome; `queued` counts parks). The tool gathers, the
model concludes, a human approves mutations — the confirm-token flow is the
approval gate; nothing in `suggested_actions` auto-executes.

## Prompts

Guided playbooks:
`triage-alert(alert[, target])`, `rca-l4-errors`, `rca-ai-latency`,
`capacity-report`, `safe-lb-change(change[, target])` — the last walks a
baseline → preflight → apply → verify → rollback LB change. `triage-alert`
maps `LoxilbAIAdmission*` to `diagnose_ai_latency`, `lb_get` and, for a
ceiling or queue change, an `lb_create` replace preview; `LoxilbHalfCloseHold*`
to `halfclose_get` and `lb_get` (the release endpoint is not a bridge tool and
is only proposed to the operator); `KvExact*` to `lb_kvexact_status`.

## Resources

`loxilb://docs/alerts` (rendered alert-rules catalog),
`loxilb://docs/metrics` (metric family reference: curated key families and
caveats, then one row per `loxilb_*` family generated from
`tools/metric-manifest` by `go generate` in `mcp/`),
`loxilb://spec/openapi` (swagger spec).

## Audit log

`<audit_dir>/audit.jsonl`, one JSON object per event: `tool_call` (with
redacted args for mutations), `auth_reject`, `origin_reject`, `rate_limit`.
Mutating calls log both failures and successes; confirm-token rejections
appear as `confirm: ...` errors.

## Security posture

- HTTP mode refuses to start without client tokens, and refuses plaintext on
  non-loopback binds unless `--insecure-http` (lab only). TLS + optional mTLS
  via `--tls-cert/--tls-key/--tls-client-ca`.
- Browser cross-origin requests are rejected (Go 1.25 cross-origin
  protection + SDK localhost check) — DNS-rebinding defense.
- Per-client token-bucket rate limiting (T8): 20 requests in a burst, refilled
  at 10 per second, per client name, unless the client's `clients:` entry sets
  its own `rps` / `burst` (neither can be set to zero). A request past the
  budget is answered `429` with `Retry-After: <seconds>` and the JSON body
  `{error: rate_limited, client, retry_after_sec, limit: {rps, burst}, detail}`
  and is recorded as a `rate_limit` audit event. A list-then-get pass over
  twenty objects, or ten preview/confirm pairs, fits one burst; size the
  budget of a busy client up rather than pacing it. Constant-time token
  verification.
- The mutation budget (change-gate requirement 4): per caller, at most
  `mutations_per_minute` mutations sent per sliding minute (default 6) and
  a stop after `max_consecutive_failures` refused mutations in a row
  (default 3) until a read-only tool call succeeds; refusals are readable
  tool errors, nothing queues, previews and reads are never counted.
- Log/archive content is returned under `untrusted_data` keys — treat as
  data, never instructions (prompt-injection defense).
- Known caveats: with `--userservice` on a target, configure bridge target
  credentials (`token_env` or `username`/`password_env`) — otherwise metrics
  scraping returns 401. And `ai_traffic_report` restates an accounting caveat
  in every result: `loxilb_ai_requests_total` splits on an `outcome` label
  (`completed` = answered whole, `aborted` = the client did not get the whole
  answer, with a `cause` of `client`, `engine` or `gateway`, `denied` =
  refused by the policy gate or by the MCP framing checks — 400
  `mcp_invalid_request`, 405 `mcp_method_not_allowed`, 413
  `mcp_request_too_large` — on an AI-gateway rule; `error_ratio` is
  completed 5xx plus engine and gateway aborts over completed + aborted),
  `loxilb_ai_rate_limit_hits_total` carries the *reason* a request was denied
  rather than a second count of it, and a gateway old enough to emit no
  `outcome` label excludes denials from the total entirely — cross-check those
  against `loxilb_proxy_http_responses_total`.

## CI / E2E

`cicd/mcp/` is the self-contained scenario: it builds the
bridge, drives it with curl JSON-RPC over streamable HTTP against a docker
testbed — observe checks, an MCP-only L4 create→traffic→confirm-delete
round-trip, a typed-AI create→observe→full-key-delete round-trip with audit
verification, viewer-role guardrails, the API-key
lifecycle with secrets-to-file, and the diagnose/report tools. The `stage1`
job of `.github/workflows/mcp-ci.yml` runs it on every PR that touches
`mcp/**`, the swagger files, the metric manifest or `cicd/mcp/**`, against the
published gateway image on an ubuntu-22.04 runner; the scenario log is the
`mcp-stage1-log` artifact and the per-check verdicts are in the job summary.
Run it locally with:

```sh
cd cicd/mcp && ./config.sh && ./validation.sh; ./rmconfig.sh
```

The AI 429-under-load drill and the 12 h soak need an SSE
backend and wall-clock time, so they run separately on a dedicated testbed.

### On-demand E2E against a live target

`cicd/mcp/live-e2e.sh` validates the bridge against an already-running loxilb
deployment — no docker testbed, no LLM. It builds the bridge, opens a single
persistent stdio JSON-RPC session, and asserts the observe/guardrail surface:

```sh
cd cicd/mcp
./live-e2e.sh                                   # default target, read-only
TARGET=http://<host>:11111 ./live-e2e.sh        # any live target
./live-e2e.sh --mutate                          # + control-plane round-trip
```

Read-only mode (default) changes **nothing** on the target: it checks
`version_get`/`health_overview` reachability, cross-checks `lb_list` against the
target's REST `/config/loadbalancer/all`, verifies `metrics_snapshot`,
`targets_list`, `fleet_overview`, `diagnose_l4_errors`, and that
`ai_traffic_report` surfaces its accounting caveat, then exercises the
guardrails (URL-as-target rejected, unknown target
rejected, unknown tool → JSON-RPC error, viewer role sees no `lb_create`).

`--mutate` additionally runs an isolated `lb_create` → `lb_list` → confirm-token
`lb_delete` round-trip on a TEST-NET-2 VIP (`198.51.100.7:19999`, override with
`VIP`/`VPORT`/`VEP`) that will not collide with real services, checks the audit
trail, and cleans up — including a `--no-confirm` safety-net delete if any step
fails, so a run never leaves state on the target. The two-step confirm-token
flow is why the harness holds one session open: the token is in-memory,
single-use, and per process. Exit code 0 means every check passed.

### Release notes

#### mcp/v0.2.0 (unreleased)

**Added** — the AI control-plane writes of tenant and object scope (PR-B2): `lb_update` (admin-confirm, impact service: `admin_state_up`, endpoint weights, `fc_mode`, `fc_max_outstanding`, `fc_max_queue_depth`, `fc_max_queue_wait_ms`; everything else is `lb_create`/`lb_delete`; a PATCH the gateway refuses is passed on with the replace hint), `ai_user_ratelimit_set` and `ai_user_ratelimit_delete` (operator; an all-zero entry is refused before any request, the delete's preview carries the row as the rollback), `secrate_disable` (admin-confirm, global, never autopilot: `DELETE /config/securityrate`, the preview carries the configuration as a `secrate_set` rollback), `config_persist` (admin-confirm, instance, never autopilot: the preview's `current` is the snapshot identity, 409 on the gate is `snapshot_gate_busy`). The mutation budget: `change_budget: {mutations_per_minute, max_consecutive_failures}` and a per-client `mutations_per_minute`, defaults 6 and 3, floors of 1, refusals as readable tool errors, the stop lifted by a successful read-only call. Tool counts: viewer 68, operator 77 → 79, admin 95 → 100 (101 with `--allow-import`).

**Added** — diagnostics modernisation (PR-D, read-only): `diagnose_ai_latency` sections `admission`, `pd_admission_sheds`, `epp`, `kv_freshness`, `quota_denied` with read-only suggestions (`lb_get`, `ai_kv_inventory_get` or `lb_kvexact_status`, `ai_ratelimit_get`); `ai_traffic_report` fields `tokens_consumed`, `tokens_consumed_prompt`, `tokens_consumed_completion`, `quota_denied`, `quota_denied_total`, `fc_band`; the generated family table of `loxilb://docs/metrics`; `triage-alert` mappings for `LoxilbAIAdmission*`, `LoxilbHalfCloseHold*`, `KvExact*`.

**Changed** — `lb_update` refuses `fc_*` on a fullproxy rule before any request and names the `lb_create` replace; on any other mode the preview and the read-back (`fc_note`) say that no capacity gate runs there (PR-D, D26).

**Changed** — the operator writes `ai_apikey_create`, `ai_apikey_update` and `ai_ratelimit_set` preview by default (`current`, `proposed`, `diff`, `rollback`, `impact`, `confirm_token`) and apply with the token or with `apply: true`; executed results carry `read_back`. `ai_apikey_create` answers the common mutation shape: `key_id`, `key_file` (or `raw_key` on reveal) and `note` moved under `result`; it gains `enabled`, `client_op_id`, `priority_class` (generated enum), `default_objective`, `allowed_objectives`. `ai_apikey_update` covers the full PATCH surface (`rate_limit_rps`, `burst_size`, `tokens_per_min`, `priority_class`, `default_objective`, `allowed_objectives` beside `allowed_models` and `enabled`; an empty string or list clears). `ai_ratelimit_set` gains `model_limits`, `priority_class`, `default_objective`, `allowed_objectives`. `lb_create` types `fc_mode`, `fc_max_outstanding`, `fc_max_queue_depth`, `fc_max_queue_wait_ms`, `fc_ep_max_inflight`, `fc_prefill_max_inflight`, `fc_decode_max_inflight`, `fc_tenant_max_share_pct`, `epp_endpoint`, `epp_failure_mode`, `epp_timeout_ms`, `epp_plaintext`, `ai_default_reservation`, `ai_default_max_tokens`, `ai_objective_strict`, `security`, `admin_state_up`, `id` (the enumerated ones validated against the generated swagger tables); the replace preview's rollback carries them. `pii_url_patterns_set` sends `is_exclude`, the key swagger spells (the bridge sent `exclude`, which the gateway ignored), and its argument is renamed the same way. `secrate_set`'s rollback on a target with no protection on is `secrate_disable`. `lb_list` and `lb_get` report `admin_state_up` as `true`, `false` or `"absent"` (it was a boolean omitted when the GET carried no flag); `lb_update`'s preview reports an absent flag as `"absent"` and only its read-back after sending `false` resolves absent to `false`, with `admin_state_up_note`.

**Added** — 17 control-plane reads, all viewer tier: `lb_get` (composite key or opaque id; `include: [status, stats]`), `lb_kvexact_status`, `ai_objective_list`, `ai_netidentity_list`, `ai_jwtprofile_list`, `ai_user_ratelimit_list`, `ai_user_ratelimit_get`, `ai_ratelimit_defaults_get`, `ai_model_profile_list`, `ai_model_profile_get`, `status_capabilities`, `maintenance_get`, `halfclose_get`, `audit_status`, `audit_policy_get`, `audit_sink_get`, `config_snapshot_get` (size-capped). Tool counts: viewer 51 → 68, operator 60 → 77, admin 78 → 95 (96 with `--allow-import`).

**Changed** — the per-client request budget (20 burst, 10/s) can be set per client in the config file (`rps`, `burst` on a `clients:` entry; defaults unchanged, neither field can be zero), and a request past it is answered `429` with `Retry-After` and the JSON body `{error: rate_limited, client, retry_after_sec, limit, detail}` instead of a line of text.

**Changed** — `config_snapshot_get` returns the gateway's 409 (its auto-persist holds the snapshot gate for about 3 s after a successful config write) as the structured tool error `{error: snapshot_gate_busy, retry_after_sec: 3, detail}`, text and structured content alike, and does not retry on the caller's behalf: wait `retry_after_sec` and call again.

**Changed** — operator sessions no longer see the 14 global- and
instance-scope tools (`secrate_set`, `secrate_reset`, `bgp_neigh_set`,
`bgp_global_set`, `bgp_policy_apply`, `config_params_set`, `gpu_mode_set`,
`gpu_conversations_cleanup`, `llamafw_enable_set`, `llamafw_configure`,
`llamafw_scanners_set`, `pii_enable_set`, `pii_configure`,
`pii_url_patterns_set`); an admin session with a confirm token does, after a
preview with `current`, `proposed`, `diff` and `rollback`. `lb_create` on an
existing rule key is a replace and follows the same flow (admin-confirm); on
a new key it is unchanged. `llamafw_health_check` previews unless called
with `apply: true`. `secrate_set` sends the full merged configuration (the
REST body requires every threshold); its integer arguments accept an
explicit 0, a change that would leave every protection off is refused
before a token is issued, and on a target that had none on the rollback is
a note (`DELETE /config/securityrate`; `secrate_disable` since PR-B2). Executed changes carry
`read_back`. Tool counts: operator 74 → 60.

### Contract gate

`mcp/contract/` keeps the bridge in step with the REST API. The `contract`
job of `mcp-ci.yml` runs on every change to `mcp/**`, `api/swagger.yml`,
`api/swagger-extras.yml` or `tools/metric-manifest/**` and fails when:

- `coverage.yml` and swagger disagree: an operation of `api/swagger*.yml` has
  no entry (`tools:`, `pending: P1a|P1b|P2|P3|P4`, or `excluded: "reason"`),
  or an entry names an operation that no longer exists.
- a `tools:` entry names a tool the bridge does not register, or the tier
  the manifest states is not the one the code enforces (read-only → viewer,
  destructive → admin-confirm, otherwise operator).
- a mutation's tier is weaker than its blast radius allows. The minimum tier
  is **derived** from `impact` and `reversible`: `none` → viewer; `object` or
  `tenant` and reversible → operator, irreversible → admin-confirm; `service`,
  `global`, `instance` → admin-confirm, and `global`/`instance` are never
  autopilot-eligible. A weaker tier passes only with a `tier_gap:` reason,
  counted against `budget.tier_gap_max`.
- a REST path literal in `internal/mcp/tools` or `internal/mcp/client`
  matches no swagger operation, or the manifest does not credit the tool
  whose handler contains it.
- `pending` entries exceed `budget.pending_max`, or a `fields:` allowlist
  names a property swagger does not have. A raised budget is a warning.
- `fields.yml` leaves a property of `serviceArguments`, `endpoints[]`,
  `ApiKeyCreateRequest` or `TenantRateLimitMod` unlabelled, or a `typed`
  property has no matching tool argument. A changed swagger description is a
  warning naming the field (`desc_sha256`).
- a tool argument or result field is named like a secret (`pem`,
  `private_key`, `secret`, `password`, `token` other than `confirm_token`,
  `api_key` on an input).
- a `loxilb_*` name under `mcp/internal` or `mcp/deploy` is not a family
  `tools/metric-manifest` finds in the gateway (`metrics-exceptions.yml`
  carries the known defects until their fix lands).
- `go generate ./...` changes `internal/mcp/tools/zz_swagger_enums_gen.go`:
  the swagger property lists, enum values and enum descriptions the tools
  package compiles in are generated, never copied.

The job's step summary lists the pending operations by phase, the mapped
mutations by impact and tier, the recorded tier gaps, and the gateway metric
families the bridge never references. The tests skip when `api/` is not
beside the module, so `go install` of the bridge keeps working.
