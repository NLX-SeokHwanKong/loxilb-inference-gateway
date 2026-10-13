# AI Gateway Controls — API Keys, Rate Limiting, Model Routing, SSE Quotas

> **Audience:** operators exposing OpenAI-compatible LLM endpoints to multiple teams, tenants,
> or customers, who need authentication, quota, and per-model traffic policy **at the gateway**
> instead of inside every serving engine.
> **Scope:** API-key management and enforcement, per-tenant rate limiting, model-name routing,
> and SSE stream quotas.
>
> **For exact rule fields, treat the matching `cicd/ai-*/config.sh` and `validation.sh` as the
> source of truth** — they are the gated, working configurations and track the current build.

---

## 1. The control surfaces at a glance

| Control | What it does | Config surface | CICD scenario |
|---|---|---|---|
| **API keys** | Issue/revoke `lxb_…` keys; clients authenticate with `X-Api-Key`; per-key model allow-list + rate/burst/token limits | `POST/GET/PATCH/DELETE /netlox/v1/config/ai/apikey` (`PATCH` is defined in `api/swagger-extras.yml`, not the main spec) | [`cicd/ai-apikey`](../../cicd/ai-apikey) |
| **Tenant rate limits** | Aggregate RPS / tokens-per-minute ceiling across all keys of a tenant | `POST/GET /netlox/v1/config/ai/tenant/ratelimit` | [`cicd/ai-apikey`](../../cicd/ai-apikey) |
| **Model-name routing** | Route by the request's model (`X-Model` header or JSON body `model`) to different endpoint pools | `model_name` + `path_prefix` + `path_match_mode` on the LB rule | [`cicd/ai-model-routing`](../../cicd/ai-model-routing) |
| **SSE stream quotas** | Keep streams alive past idle timeouts, cap runaway streams, backend keepalive | `sse_mode`, `max_stream_duration_sec`, `backend_keepalive_interval_sec` on the LB rule | [`cicd/ai-sse-quota`](../../cicd/ai-sse-quota) |

All of these run on **fullproxy** rules (`mode: 4`). Model routing and SSE quotas are pure
rule fields with no extra infrastructure; API keys and tenant limits additionally need the
user-service database (§2.1).

---

## 2. API-key management & enforcement

### 2.1 Prerequisites

API keys are persisted in the gateway's user-service database. Start loxilb with:

```bash
loxilb --userservice \
  --mgmt-db-host <postgres-ip> --mgmt-db-user aigw_mgmt_user --mgmt-db-name loxilb \
  --mgmt-db-password-file /etc/loxilb/mgmt_db_password \
  --aikey-db-host <postgres-ip> --aikey-db-user aigwuser --aikey-db-name loxilb \
  --aikey-db-password-file /etc/loxilb/aikey_password
```

backed by a PostgreSQL instance. Administrative calls authenticate with an
opaque session token, which the store holds only as a SHA-256 hash:

```bash
# one-time: create an admin user, then log in for a Bearer token
curl -s -X POST http://127.0.0.1:11111/netlox/v1/auth/users -d '{...}'
TOKEN=$(curl -s -X POST http://127.0.0.1:11111/netlox/v1/auth/login \
  -d '{"username":"...","password":"..."}' | jq -r .token)
```

### 2.2 Turning on data-plane enforcement

Enforcement (key validation + rate limiting) engages on any fullproxy rule with
`sse_mode: true`:

```json
{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "mode": 4, "sse_mode": true, "inactiveTimeOut": 60, "host": "10.10.10.254"
  },
  "endpoints": [ { "endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1 } ]
}
```

### 2.3 Key lifecycle (admin, Bearer-authenticated)

**Create** — returns the raw key exactly once:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/ai/apikey \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{
  "tenant_id": "team-a", "name": "prod-key",
  "allowed_models": ["llama-70b", "mistral-7b"],
  "rate_limit_rps": 50, "burst_size": 100, "tokens_per_min": 100000,
  "enabled": true }'
# → 201 { "key_id": "…", "raw_key": "lxb_…", … }   raw_key is shown only here
```

**List / get / update / delete:**

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:11111/netlox/v1/config/ai/apikey?tenant_id=team-a"      # list
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:11111/netlox/v1/config/ai/apikey/<key_id>               # get (never returns key material)
curl -s -X PATCH -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:11111/netlox/v1/config/ai/apikey/<key_id> \
  -d '{"allowed_models":["llama-70b"]}'                                     # update
curl -s -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:11111/netlox/v1/config/ai/apikey/<key_id>               # → 204
```

### 2.4 Client-side usage & enforcement results

Clients send the key (and optionally the model) as headers to the VIP:

```bash
curl -s http://10.10.10.254:2020/v1/chat/completions \
  -H 'X-Api-Key: lxb_…' -H 'X-Model: llama-70b' -d '{...}'
```

| Condition | Response |
|---|---|
| Valid key, allowed model, within limits | `200` (proxied) |
| Missing/unknown key | `401` `invalid_api_key` |
| Model not in the key's `allowed_models` | `403` `model_not_allowed` |
| Per-key or per-tenant rate/token limit exceeded | `429` |
| JSON body above the inspect cap whose top-level `model` is not within the first 64 KiB of the body | `413` `request_too_large_for_admission` |
| Chunked (or length-less) body larger than the gateway's 1 MiB request buffer | `413` `request_body_too_large` |

Enforcement runs before the first byte reaches a backend for every request
shape, including bodies the gateway streams instead of buffering (any non-JSON
body above 64 KiB, or a JSON body above 768 KiB). For a streamed JSON body the
model is resolved from the first 64 KiB, so put `model` early in the object;
the token reservation for such a request is sized from its `Content-Length`.
A chunked request body is never streamed and must fit the request buffer;
declare `Content-Length` for large uploads.

Backend connectivity failures surface through the same VIP and are easy to mistake for
quota errors: `502 backend_unreachable` / `503 no_healthy_backend` come from the proxy's
connect-failover path, not from key enforcement — see the
[troubleshooting guide](06-troubleshooting.md) §5.

## 3. Per-tenant rate limiting

Tenant-level ceilings apply across **all** keys of a tenant, on top of per-key limits:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/ai/tenant/ratelimit \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"tenant_id": "team-a", "rps": 100, "tokens_per_min": 500000}'

curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:11111/netlox/v1/config/ai/tenant/ratelimit/team-a       # read back
```

Both layers return `429` on breach; per-key limits trip first if they are tighter.

### Burst

`tokens_per_min` is a refill *rate*, not a hard per-minute cap. The tenant spends from a
bucket that refills continuously at that rate, so recovery from a breach is gradual rather
than a cliff at the top of the minute. `burst_pct` sets how large that bucket is, as a
percentage of `tokens_per_min` — that is, how much of a minute's budget a fully idle tenant
may spend at once:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/ai/tenant/ratelimit \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"tenant_id": "team-a", "rps": 100, "tokens_per_min": 500000, "burst_pct": 50}'
```

Omit it (or send `0`) to use the server default of `100` — one whole minute's budget, which
is how tenants behave with no override. Lower it to smooth a tenant that would otherwise
drain its budget in one burst and starve itself for the rest of the minute; raise it for
tenants with long idle gaps and large single requests. Accepted range is `1`–`1000`, and
values outside it are clamped rather than rejected. The server-wide default can be changed
with the `LLB_AI_QUOTA_BURST_PCT` environment variable.

Note that a single request larger than the bucket can never be admitted — the pre-admission
check refuses it outright rather than letting it queue — so `burst_pct` must leave room for
the largest prompt-plus-`max_tokens` a tenant legitimately sends.

### Requests without `max_tokens`: the default reservation

Before a request is sent to a backend the gateway reserves its worst case against every
token quota it counts on (tenant, tenant and model, user, user and model, key, and the
service's shared bucket): the prompt estimate plus the completion ceiling the request
declares in `max_tokens` or `max_completion_tokens`. The reservation is released when the
response is settled and replaced by what was really used.

A request that declares no ceiling — the field is absent, `-1` or any negative number, `0`,
a string, a float, an object or an array — reserves its prompt estimate alone. That makes
leaving the field out the cheaper choice at admission: the same request with a ceiling can
be refused before dispatch, and without one it cannot, so a tenant's `tokens_per_min`
bounds only its prompts until the usage is counted afterwards.

The default reservation closes most of that gap. It is off unless set, per service:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"serviceArguments": {"externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
        "mode": 4, "sse_mode": true, "host": "10.10.10.254",
        "ai_default_reservation": "on", "ai_default_max_tokens": 1024},
       "endpoints": [{"endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1}]}'
```

| Field | Values | Meaning |
|---|---|---|
| `ai_default_reservation` | `on`, `off`, `inherit` | `on`: a request without a ceiling also reserves the default. `inherit` or omitted: the process default, `LLB_AI_DEFAULT_RESERVATION` (`on`/`off`), else off |
| `ai_default_max_tokens` | `0`–`100000000` | the default in tokens. `0` or omitted: `LLB_AI_DEFAULT_MAX_TOKENS`, else the built-in `512` |

How it behaves:

- **It never refuses a request.** The default is reserved beside the prompt estimate, and
  each quota bucket cuts it to what it can hold: to the bucket's capacity less the prompt
  estimate, and to what earlier reservations have left, down to nothing. Only the prompt
  estimate — and a ceiling the request declares — can be refused, exactly as before.
- **A request admitted with less than the default is counted**, per service and by
  reason, in `loxilb_ai_reservation_clamped_total{service, reason}` and on the rule's
  `GET` (`fc_effective.reservation_clamped_burst`, `reservation_clamped_budget`, and
  their sum `reservation_clamped`):
  - `burst`: the default beside the prompt estimate is more than a quota bucket can ever
    hold, so the request is clamped even on an idle bucket. This does not pass by itself.
    Lower `ai_default_max_tokens`, or raise the tenant's `tokens_per_min` or its burst.
    Alert on this one.
  - `budget`: the bucket's budget was already taken by other reservations or by spend.
    It passes as the minute refills; a count that keeps rising means the budgets are
    tight for the traffic.

  A request both apply to is counted as `burst`.
- **It sizes the reservation only.** Nothing is written into the request, a declared
  ceiling is never replaced, and the response is charged for its real usage as before.
- **It narrows the gap, it does not close it.** When a bucket is nearly empty the default
  is cut to nothing and that request is prompt-only again; if its response then overruns
  the budget, the tenant's next requests are refused `token_quota_exceeded` until the
  bucket refills.
- The built-in `512` is an estimate of a typical completion, not a measured one. Set
  `ai_default_max_tokens` to what your clients' completions look like.

**Per listener until per-rule lands.** The gateway reserves before it selects a rule by
host, path or model, so one default reservation is in force per listener (VIP and port):
where several rules share one (model-name routing, for example), the value of the rule
installed first on it applies to all of them, as with `api_key_auth`. Declare the same
value on each rule of such a service. Two things keep a difference from going unnoticed:

- **A warning when the rule is applied.** If the rules on the VIP and port resolve to
  different default reservations, the rule is still accepted, and the reply reads
  `Success; warning: ai_default_reservation: …` with the values the rules resolve to and
  the one in effect. The same text is logged at warning level.
- **The read-back shows what is in effect.** `fc_effective.default_reservation` and
  `default_max_tokens` are the listener's value on every rule of the VIP and port, and
  their `source` entries read `listener` on a rule that resolves to something else
  (otherwise `rule`, `env` or `default`, as that rule resolves it). The rule's own
  `ai_default_reservation` and `ai_default_max_tokens` keep showing what it declares.

Both fields follow the replace rules of the
`fc_*` fields: omitted on a replace keeps the stored value, `inherit` and `0` return to the
process default, `null` is refused, and a replace that changes nothing else applies in
place, to requests admitted from then on. A streamed request body whose `max_tokens` the
gateway has not read by the time it admits the request is treated as declaring none.

---

## 4. Model-name routing

Route requests to different endpoint pools based on the model they ask for — the gateway reads
the `X-Model` header or the JSON body's `model` field. One rule per model on the same VIP/port
family; an empty `model_name` is the wildcard catch-all.

```bash
# llama-70b pool
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer -d '{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "sel": 0, "mode": 4, "host": "10.10.10.254",
    "path_prefix": "/", "path_match_mode": "prefix",
    "model_name": "llama-70b", "inactiveTimeOut": 30 },
  "endpoints": [ { "endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1 } ]}'

# mistral-7b pool → port 2021 with "model_name": "mistral-7b"
# catch-all      → port 2022 with "model_name": ""
```

Five fields are required per model-routing rule: `mode: 4`, `host`, `path_prefix`,
`path_match_mode`, `model_name`. No database or user-service is needed.

Combined with per-key `allowed_models` (§2), this gives tenant-scoped model access **and**
model-scoped backend pools from one gateway.

---

## 5. SSE stream quotas

LLM responses stream over SSE and can outlive any sane idle timeout — or run away entirely.
Three rule fields govern streaming behavior:

```json
{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "sel": 0, "mode": 4, "host": "10.10.10.254",
    "path_prefix": "/", "path_match_mode": "prefix", "model_name": "sse-test",
    "sse_mode": true,
    "max_stream_duration_sec": 120,
    "backend_keepalive_interval_sec": 30,
    "inactiveTimeOut": 60
  },
  "endpoints": [ { "endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1 } ]
}
```

| Field | Meaning |
|---|---|
| `sse_mode: true` | Detect SSE responses; suppress `inactiveTimeOut` while a stream is active; recognize the `data: [DONE]` terminator |
| `max_stream_duration_sec` | Hard wall-clock cap per stream — terminates runaway streams |
| `backend_keepalive_interval_sec` | TCP keepalive interval toward the backend during long streams |

This is a **stream-duration** quota; token-count budgets are enforced per key/tenant via
`tokens_per_min` (§2–§3).

---

## 6. Try it

```bash
cd cicd/ai-model-routing   # no DB needed — model routing only
./config.sh && ./validation.sh && ./rmconfig.sh

cd ../ai-sse-quota         # SSE quota behaviors incl. the 10s runaway-cap probe
./config.sh && ./validation.sh && ./rmconfig.sh

cd ../ai-apikey            # full key lifecycle + 401/403/429 enforcement (brings up PostgreSQL)
./config.sh && ./validation.sh && ./rmconfig.sh
```

---

## 7. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `POST /config/ai/apikey` fails with `503 ai_key_store_unconfigured` | loxilb started without `--aikey-db-host` | Restart with the `--aikey-db-*` family and a reachable PostgreSQL provisioned by `scripts/aigw-db-bootstrap.sql` |
| `POST /config/ai/apikey` fails with `401` | loxilb started with `--userservice` and no credential was presented | Authenticate first; see the bootstrap note above |
| Mutating calls return `503` + `Retry-After: 5` right after a gateway restart | Boot-config gate: writes are held until the boot snapshot replay settles | Expected — retry after the `Retry-After` interval; not an outage |
| Keys accepted but not enforced at the VIP | Rule lacks `sse_mode: true` / not `mode: 4` | Enforcement runs in the fullproxy AI path only |
| Requests hit the wrong model pool | Field casing | `model_name`, `path_prefix`, `path_match_mode` are snake_case — a mis-cased field is silently dropped |
| Streams cut at ~`inactiveTimeOut` | `sse_mode` not set | Set `sse_mode: true` on streaming rules |
| Long streams die at exactly N seconds | `max_stream_duration_sec` cap | Raise the cap; it is the intended runaway guard |
