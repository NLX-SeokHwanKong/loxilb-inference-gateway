# MCP Gateway — Load-Balancing Model Context Protocol Servers

> **Audience:** operators putting a fleet of MCP (Model Context Protocol) servers behind one
> stable, TLS-terminating, session-sticky endpoint.
> **Scope:** the three MCP proxy deployment shapes (HTTP, TLS-terminating, end-to-end HTTPS),
> MCP session stickiness, and the runnable CICD scenarios that validate each shape.
>
> **For exact rule fields, treat the matching `cicd/mcp-*/config.sh` as the source of truth** —
> they are the gated, working configurations and track the current build.

---

## Protocol versions

The session examples below cover the legacy Streamable HTTP transport (2025-11-25). MCP 2026-07-28 uses independent requests and removes protocol sessions and initialization. Create a separate TCP FullProxy rule with `mcp_protocol_version: "2026-07-28"`, `sel: 0` or `sel: 2`, and `sockMapMode: "off"`; omit `session_header_name`. Existing rules retain their mode; changing it requires a new rule. All aliases on a shared listener must agree on the mode.

The modern rule validates required request headers against JSON-RPC metadata before backend selection. Supported L7 header routes use `Mcp-Method` and the decoded `Mcp-Name`; policies cannot modify validated transport headers or add session persistence. JSON and finite SSE responses are supported. Validation buffers bodies up to 768 KiB and rejects larger requests with HTTP 413. HTTP/1.1 supports one outstanding request per connection; pipelining is rejected. With L7 policies, operands must fit the existing routing field bounds. These are admission limits, not streaming validation fallbacks.

The modern frontend can negotiate HTTP/2 independently of an HTTP/1.1 backend. The H2-to-H1 bridge allocates one backend connection per live stream, bounded by the per-frontend-connection bridge-slot limit (32). It preserves incremental SSE and isolates cancellation to that stream. RR/WRR selection is shared across frontend protocols; unequal L7 backendRef weights select within the permitted route pool rather than multiplying the base endpoint weights. Oversized or malformed backend response framing terminates the affected stream.

Modern MCP requires `backend_protocol: "http1"` or omission and `egress: false` or omission. Admission refuses backend `http2`/`both` and egress rules, including replacements that omit the MCP declaration. Frontend HTTP/2 remains supported through the H2-to-H1 bridge. Endpoint weight `0` retains the existing default-to-`1` behavior; it is not a drain switch. Backend connections are request-owned; connection pooling is deferred to a separate design.

Configured FC admission applies to modern POST requests, including per-endpoint saturation and endpoint-return warmup. Read `fc_effective` to verify installed mode, limits and warming state; configuration values alone do not establish enforcement. Capacity fallback and connection retries remain inside the selected L7 route and require a permit for the target endpoint.

Only the explicitly implemented `2026-07-28` version is accepted. Future dates are not negotiated automatically. Validation denials carry a JSON-RPC error with a safely recovered request ID, or `null` when no valid ID is available. Unknown versions use `-32022` with the supported version list; malformed requests and mirrored-header failures remain distinct. Early framing/size failures may precede body parsing. `null` configuration is rejected. Omitting the version on a replacement preserves the existing mode; an explicit empty value selects legacy behavior only when creating a new rule.

Delete/recreate affects new connections. Existing connections can retain the old transport policy until they close; drain or close clients before moving a listener between modes. The modern implementation uses a new backend connection per H1 request or H2 stream. It does not pool backend legs, so connection churn and TIME-WAIT are deployment capacity constraints. Admission limits and workload-specific connection measurements are needed before sizing a fleet.

Unrelated Authorization/Cookie/custom headers retain their wire bytes and are not subject to the small L7 operand limit. Only headers referenced by policy must fit the routing fields. Encoded Unicode/control characters in `Mcp-Name` follow the MCP Base64 contract; a name that cannot be represented safely by a configured L7 operand is refused before routing.

See the [modern regression fixture and commands](../../cicd/mcp-stateless/README.md). Its portable unit tests do not establish an installed runtime qualification. MRTR, subscriptions, arbitrary tool parameter schema reflection and OAuth authorization are outside this implementation's scope.

## 1. Why put a gateway in front of MCP servers

Legacy MCP servers speak the session-oriented Streamable HTTP transport: a client opens a session, receives an
`mcp-session-id`, and every subsequent JSON-RPC call must land on the server that owns that
session. Load-balancing this naively breaks sessions. The inference gateway solves it at L7:

- **Session stickiness** — the sockproxy keys routing on the `mcp-session-id` response/request
  header, so all calls of one MCP session stay on one backend.
- **TLS termination or end-to-end HTTPS** — expose `https://` to clients regardless of whether
  the MCP servers themselves run TLS.
- **Health failover & scaling** — standard endpoint liveness and weighted selection across a
  pool of identical MCP servers.
- **MCP-aware tracing** — `trace_type: "mcp"` tags the proxy's trace records for MCP traffic.

```
MCP client ──► VIP :2020 ──► [sockproxy: mcp-session-id stickiness] ──► mcp-server pool :8080
```

All MCP rules are **fullproxy** rules (`mode: 4`) — the L7 sockproxy must terminate the
connection to observe the session header.

---

## 2. Deployment shapes

| Shape | Frontend → backend | `security` | CICD scenario |
|---|---|---|---|
| Plain HTTP proxy | HTTP → HTTP | absent (0) | [`cicd/mcp-httpproxy`](../../cicd/mcp-httpproxy) |
| TLS-terminating proxy | **HTTPS** → HTTP | `1` | [`cicd/mcp-fullproxy`](../../cicd/mcp-fullproxy) |
| End-to-end HTTPS | **HTTPS** → **HTTPS** | `2` | [`cicd/mcp-e2ehttps`](../../cicd/mcp-e2ehttps) |

Two more scenarios — [`cicd/mcp-direct-test`](../../cicd/mcp-direct-test) and
[`cicd/mcp-direct-test-https`](../../cicd/mcp-direct-test-https) — validate raw client↔server
MCP connectivity without any LB rule, and are useful for isolating backend problems.

### 2.1 Plain HTTP MCP proxy

Round-robin across two MCP servers with MCP session stickiness:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer \
  -H 'Content-Type: application/json' -d '{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "sel": 0, "mode": 4, "host": "10.10.10.254",
    "session_header_name": "mcp-session-id"
  },
  "endpoints": [
    { "endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1 },
    { "endpointIP": "32.32.32.1", "targetPort": 8080, "weight": 1 }
  ]}'
```

For hash-persistent (source-sticky) selection instead of round-robin, use `"sel": 3`.

### 2.2 TLS-terminating MCP proxy (HTTPS in, HTTP out)

Adds `security: 1` (frontend TLS, plain HTTP to the backends) and MCP trace tagging:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer \
  -H 'Content-Type: application/json' -d '{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "sel": 0, "mode": 4, "security": 1,
    "session_header_name": "mcp-session-id",
    "host": "10.10.10.254", "trace_type": "mcp"
  },
  "endpoints": [
    { "endpointIP": "31.31.31.1", "targetPort": 8080, "weight": 1 },
    { "endpointIP": "32.32.32.1", "targetPort": 8080, "weight": 1 }
  ]}'
```

The equivalent `loxicmd` form (loxicmd — separate repository):

```bash
loxicmd create lb 10.10.10.254 --tcp=2020:8080 --select=rr --mode=fullproxy \
  --security=https --session-header-name=mcp-session-id --host=10.10.10.254 \
  --endpoints=31.31.31.1:1,32.32.32.1:1
```

The frontend certificate is served from the gateway's certificate store. There is **no per-rule
certificate field**: uploaded certs are registered under hostnames auto-derived from the leaf
cert's SAN/CN, and the listener selects the matching certificate at handshake time by SNI. See
[L7 TLS](03-l7-tls.md) for certificate upload/rotation (`certId` management).

### 2.3 End-to-end HTTPS (HTTPS in, HTTPS out)

When the MCP servers themselves terminate TLS (e.g. launched with
`--ssl-certfile/--ssl-keyfile`), set `security: 2` — the gateway re-encrypts toward the
backend:

```bash
curl -s -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer \
  -H 'Content-Type: application/json' -d '{
  "serviceArguments": {
    "externalIP": "10.10.10.254", "port": 2020, "protocol": "tcp",
    "sel": 0, "mode": 4, "security": 2,
    "session_header_name": "mcp-session-id", "host": "10.10.10.254"
  },
  "endpoints": [
    { "endpointIP": "31.31.31.1", "targetPort": 8443, "weight": 1 },
    { "endpointIP": "32.32.32.1", "targetPort": 8443, "weight": 1 },
    { "endpointIP": "33.33.33.1", "targetPort": 8443, "weight": 1 }
  ]}'
```

---

## 3. Field reference (MCP-relevant)

| Field | Values | Meaning |
|---|---|---|
| `mode` | `4` | **Required.** Fullproxy — the L7 sockproxy handles the connection |
| `sel` | `0` rr · `3` persist | Endpoint selection for *new* sessions (existing sessions follow the header) |
| `session_header_name` | `"mcp-session-id"` | Header that keys session stickiness; all requests carrying the same value route to the same endpoint |
| `security` | absent/`0` · `1` · `2` | Plain HTTP · TLS-terminate · end-to-end HTTPS |
| `trace_type` | `"mcp"` | Tags proxy trace records as MCP traffic |
| `host` | VIP address | Must be local to the gateway node (sockproxy binds the VIP) |

Streaming (SSE/streamable-HTTP responses) is proxied natively by the fullproxy path — no
MCP-specific streaming flag is needed.

---

## 4. Try it

```bash
cd cicd/mcp-fullproxy
./config.sh        # gateway + 2 fastmcp servers, TLS-terminating rule
./validation.sh    # MCP session open + tool calls through the VIP, stickiness asserted
./rmconfig.sh
```

Each `validation.sh` drives a real MCP client through the VIP: it initializes a session,
verifies the `mcp-session-id` round-trip, issues JSON-RPC tool calls, and asserts every call of
a session landed on the same backend.

---

## 5. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Session breaks mid-conversation | `session_header_name` missing on the rule | Add `"session_header_name": "mcp-session-id"` |
| Rule created but nothing listens | VIP not local | `host`/`externalIP` must be an address on the gateway node |
| TLS handshake fails at client | `security` mismatch | `1` for HTTPS-in/HTTP-out, `2` only when backends serve TLS |
| Backend works direct, fails via VIP | Isolate with the no-LB scenarios | Run `cicd/mcp-direct-test(-https)` against the backend first |

See [Troubleshooting](06-troubleshooting.md) for general L7/fullproxy diagnostics.
