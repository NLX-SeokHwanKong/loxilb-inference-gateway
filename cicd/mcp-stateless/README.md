# MCP 2026-07-28 stateless regression fixture

For a container-only Gateway/backend/client lab on one CPU core, see
[the Docker provisioning guide](CONTAINER-LAB.md). Full regression automation
also accepts `--cpu` to restrict all participants to one available core.

This CPU-only Python standard-library fixture exercises the modern MCP wire contract independently of the legacy FastMCP examples under `cicd/common/mcp`. It is a bounded test backend, **not a complete MCP SDK or production server**. It supports discovery, tool/resource/prompt listing, `tools/call` (`echo`), `resources/read` (`fixture://status`) and `prompts/get` (`welcome`). MRTR, subscriptions, OAuth, schema cache validation, production concurrency limits and arbitrary tool-schema reflection are outside this fixture's scope.

The fixture accepts independent POST requests without `initialize`, carries capabilities/version on every request, emits `resultType: complete`, ignores legacy session/resumption headers and rejects GET/DELETE with 405. HTTP headers and their body mirrors are checked before execution. Content-Length and chunked request bodies are accepted within a 1 MiB fixture bound; ambiguous Content-Length/Transfer-Encoding and all request trailers are rejected. The Gateway independently enforces its smaller 768 KiB capture bound. The echo tool annotates its optional string `region` argument with `x-mcp-header: Region`; the server validates its mirrored value after Base64 sentinel decoding. Unknown custom headers are ignored. Invalid origin is 403; invalid mirrored headers are 400/-32020; unsupported version is 400/-32022; missing required per-request metadata is 400/-32602.

The server writes two distinct journal events: `received` after HTTP body framing and before protocol validation, and `executed` after successful validation and dispatch. Response receipt IDs correlate with `executed` journal entries. Gateway negative cases require **zero total journal delta**, proving the request did not arrive at the backend. A zero execution delta alone would also permit backend rejection, so it does not establish Gateway enforcement.

## Local fixture checks

From the repository root:

```sh
python3 -B -m unittest discover -s cicd/mcp-stateless -v
```

These are fixture wire tests and do not qualify installed Gateway behavior. The tests cover repeated requests on the same TCP connection, fresh connections, required metadata, missing/mismatched/duplicated headers, Unicode/Base64/whitespace/literal sentinel cases, strict JSON, invalid arguments, bounded chunked request decoding, Origin validation, legacy HTTP/session behavior and flushed SSE progress followed by one final response.

## Start independent backends

Run each command in its own terminal. Journal files are append-only; choose new campaign-owned paths for each replay. Binding defaults to loopback. Change `--bind` only to the explicit testbed address reachable by Gateway.

```sh
python3 -B cicd/mcp-stateless/fixture.py --port 8081 --backend-id backend-a --journal /tmp/uc8-backend-a.jsonl --response-mode json
```

```sh
python3 -B cicd/mcp-stateless/fixture.py --port 8082 --backend-id backend-b --journal /tmp/uc8-backend-b.jsonl --response-mode sse
```

`--progress-delay` sets the delay in seconds between progress events and final delivery (default 0.05, finite and nonnegative). Dedicated saturation fixtures can use 1.5 without slowing normal fixtures. The `sse` mode returns a real chunked `text/event-stream`, flushes a comment, optionally emits two progress events when `progressToken` is supplied, then flushes the final matching response and ends the HTTP response. This finite response permits another independent request on the same TCP connection.

Backend TLS is optional: pair `--cert /absolute/server.crt --key /absolute/server.key`. Use a certificate whose SAN matches the backend address configured in Gateway. Client HTTPS verifies system trust, hostname and certificate chain; `--ca /absolute/test-ca.pem` adds a campaign CA. There is no insecure verification flag. `--allow-origin https://client.example` explicitly allows a browser origin; an absent Origin is accepted.

## Gateway rule configuration example

Apply through the configured Gateway management endpoint, after reserving the example VIP/ports and replacing backend addresses with the actual testbed addresses. This command is an example interface, not evidence of an executed deployment.

```sh
curl --fail-with-body -X POST http://127.0.0.1:11111/netlox/v1/config/loadbalancer -H 'Content-Type: application/json' -d '{"serviceArguments":{"externalIP":"20.20.20.8","port":2028,"protocol":"tcp","sel":0,"mode":4,"mcp_protocol_version":"2026-07-28"},"endpoints":[{"endpointIP":"31.31.31.1","targetPort":8081,"weight":1},{"endpointIP":"32.32.32.1","targetPort":8082,"weight":1}]}'
```

This dedicated rule uses TCP FullProxy (`mode: 4`) and round robin (`sel: 0`) with explicit `mcp_protocol_version`. Start Gateway with sockmap disabled. Do not configure session affinity, model/P-D/KV routing on this rule. The modern mode is immutable after rule creation; delete and recreate an owned test rule to change it. Read back the rule before traffic and retain the exact source/image identity and effective runtime options with the replay report.

## Persistent request smoke client

The client prints the full result/events, backend receipt and request ID per request. Ten requests share one TCP connection by default; `--new-connection` gives each request a new connection. The client never issues initialize or supplies a session ID.

```sh
python3 -B cicd/mcp-stateless/client.py --url http://20.20.20.8:2028/mcp --requests 12 --progress
```

```sh
python3 -B cicd/mcp-stateless/client.py --url http://20.20.20.8:2028/mcp --requests 12 --new-connection --progress
```

## Gateway verification with independent journal evidence

Run this on the CPU testbed where both backend journal paths are directly readable. Copying journals after replay cannot verify negative-case arrival deltas. Avoid unrelated traffic to these dedicated backends during the replay. The verifier requires at least two expected backend IDs, validates identity/echo/receipt matches, confirms every expected backend appears while the client retains one TCP connection, validates unbuffered SSE progress, repeats on fresh connections checks six additional independent methods without clientInfo, and checks fifteen Gateway header/version/metadata negative cases. It requires each negative response to be HTTP 400 with zero total backend journal delta.

```sh
python3 -B cicd/mcp-stateless/verify_gateway.py --url http://20.20.20.8:2028/mcp --backend-id backend-a --backend-id backend-b --journal /tmp/uc8-backend-a.jsonl --journal /tmp/uc8-backend-b.jsonl --requests 12
```

For HTTPS add `--ca /absolute/test-ca.pem` to either client command. Preserve stdout as the replay report. The client and traffic verifiers do not create rules or change host networking. The explicitly invoked CI runner below creates and cleans only its own isolated resources; TLS verification remains enabled. A successful verifier establishes only its named routing/header/receipt checks; TLS failures, failover, drain/reload, cancellation, authorization and UC-8 manual clean replay need their own evidence.

## Adversarial HTTP/1.1 wire verification

The raw socket verifier covers fragmented headers/body, valid chunked requests, request trailers, media mismatch, Host/path bounds, Gateway body limits, GET/DELETE, header-only Expect 100-continue, buffered valid+malformed pipelines, and late malformed requests on an existing connection. A malformed already-buffered pipeline is rejected before either request reaches a backend; sequential keep-alive is tested separately. Bounds may return 413 while malformed mirrors return 400.

```sh
python3 -B cicd/mcp-stateless/verify_wire.py --url http://20.20.20.8:2028/mcp --journal /tmp/uc8-backend-a.jsonl --journal /tmp/uc8-backend-b.jsonl
```

## Actual HTTP/2 over TLS verification

The curl-based verifier requires HTTP/2 ALPN negotiation and checks it explicitly; a successful HTTP/1.1 fallback does not pass. It sends multiple independent requests over one TLS connection, checks JSON/SSE and early progress delivery, correlates independent journals, and exercises malformed/duplicate headers and authority/path bounds. The host curl must support HTTP/2. Use a trusted test CA and matching SAN; verification remains enabled.

```sh
python3 -B cicd/mcp-stateless/verify_h2.py --url https://gateway.example:2029/mcp --ca /absolute/test-ca.pem --backend-id backend-a --backend-id backend-b --journal /tmp/uc8-backend-a.jsonl --journal /tmp/uc8-backend-b.jsonl --requests 8
```

On an explicit MCP 2026-07-28 rule, frontend HTTP/2 can bridge to an HTTP/1 backend while `backend_protocol` remains `http1`. The verifier checks the actual frontend protocol and independent backend receipts; it does not claim the backend speaks HTTP/2. Other transport modes require their own qualification. Cleartext prior knowledge uses the same verifier with an `http://` URL and no CA argument.

## Isolated candidate CI runner

This regression automation is separate from the operator manual's individual commands. On a Linux Docker host, supply the exact ID of an image built from the candidate source and objects. The runner requires Python 3, Docker, OpenSSL and `ip`; `--require-h2` also requires curl with HTTP2 support and the Python `h2` client package for concurrent streams/cancellation. It refuses overlapping Docker subnets or host routes and occupied fixture ports 8181/8182 (plus fault-backend port 8191 for HTTP/2 and dedicated SSE ports 8201/8202 for FC checks, plus 8211 for optional CB checks). It creates unique owner-labeled container/network resources and stops only its own Python child processes. Cleanup never uses prune or broad deletion. Use a fresh evidence directory; a nonempty directory is refused to preserve earlier evidence. Exact fixture SHA256 values, journals, image identity, rules, responses and Gateway logs are retained in a private evidence directory; cleanup has its own result. Do not use an old published image to qualify candidate changes.

```sh
python3 -B cicd/mcp-stateless/run_gateway.py --image sha256:REPLACE_WITH_BUILT_CANDIDATE_IMAGE_ID --subnet 172.30.99.0/24 --evidence-dir /absolute/uc8-ci-evidence --require-h2
```

An absent `--require-h2` runs HTTP/1.1, verified TLS, wire, L7, API guards and scoped backend outage/recovery only. It makes no HTTP/2 claim. A failed campaign still retains `report.json`, backend journals and Gateway logs, and attempts scoped cleanup. The CI runner is qualified only after its own actual runtime replay; local fixture/cleanup tests alone do not qualify Gateway behavior.

```sh
python3 -B -m unittest discover -s cicd/mcp-stateless -p 'test_*.py'
```

Monitor-disabled connection refusal is expected to return HTTP 502 with zero backend arrivals; HTTP 503 means no eligible endpoint. The outage verifier confirms the single-backend route never escapes to another backend, restores the owned fixture, and checks a fresh receipt after recovery. Surviving requests on an unmonitored RR pool are recorded without claiming health-based failover.

Add `--require-weighted` to qualify explicit `sel: 2` WRR. The runner checks 3:1 endpoint weights on both persistent/fresh clients and a separate L7 rule whose backendRef weights reverse that ratio, proving the route weights are independently applied. Each phase uses 24 requests and requires exact 18:6 journal-correlated receipts. With `--require-h2`, the same checks run over real HTTP/2.

Add `--require-fc` to require installed `fc_effective` before testing enforce mode, per-endpoint capacity 1 and queue depth 0. A held SSE request on the explicit A route must block another A request without a backend arrival while the B route completes; A capacity must recover on the same client connection. Installed inflight counters and paired backend receipts are required. Missing effective state after bounded installation polling fails the campaign. The same option also reserves a bound, non-listening fixture port and checks actual refused-dial retry into a capacity-full candidate in the same pool: HTTP 502, zero backend arrivals, then successful recovery. Monitoring and circuit breaking are explicitly disabled for this retry scenario so the closed endpoint remains selectable. These checks do not qualify queued draining or adaptive tuning.

Add `--require-fc-warmup` to check configured return warmup independently. The runner changes only its own fixture endpoint's health state through the API, verifies red-state rejection, restores green in a `finally` block, and requires installed warming state with a temporary capacity of one. After installed warming state ends, two simultaneous SSE requests must reach that endpoint with inflight 2. The option also tests two ports on the same owned host: an explicit `epPort` must change only A while B keeps serving, and omitting both `epPort` and `epProto` must apply to both endpoints. Both variants require independent receipt or zero-arrival evidence and restore green in `finally`. This verifies the explicit health-state transition and capacity recovery; it does not qualify a physical host outage or automatic monitor detection. With `--require-h2`, both FC options repeat over real HTTP/2.

Add `--require-cb` for the default circuit-breaker regression on a dedicated runner-owned backend. Concurrent real TCP connection failures must trip the default threshold 5, restored backend traffic must remain refused with HTTP 503 and zero arrivals during OPEN, and the default 30-second timeout must produce logged HALF_OPEN followed by successful connection, CLOSED and matching backend receipts. With `--require-h2`, failures/recovery include real HTTP/2 and cancellation must leave a recovered sibling stream usable on the same connection. The first successful TCP connection closes HALF_OPEN at success threshold 1; held SSE streams do not establish a continued halfopen probe limit. The maximum 3 probe reservations before success require separate production-code concurrency-harness evidence. No unsupported threshold or timeout API option is configured.

The optional `verify_h2_multiplex.py` client uses Python `h2` for real concurrent streams. Four simultaneous streams must complete with matching request IDs and independent backend receipts. On the SSE-only L7 route, `--cancel` resets stream 1 after a progress event and requires stream 3 to complete normally. This checks sibling isolation and does not certify backend task cancellation semantics.

```sh
python3 -B cicd/mcp-stateless/verify_h2_multiplex.py --url http://20.20.20.8:2030/mcp --cancel --journal /tmp/uc8-backend-a.jsonl --journal /tmp/uc8-backend-b.jsonl
```

`fault_backend.py` intentionally emits malformed HTTP/1 responses solely for bridge regression; it is not an MCP SDK/backend example. With `--require-h2`, the CI runner verifies truncated/duplicate Content-Length, ambiguous CL+TE, malformed chunks, nonempty trailers, close before headers and reset midway through a body. Each fault must fail within a bounded time without a successful complete result, and have exactly one matching fault-backend request record. Gateway health and scoped cleanup are retained separately. The normal fixture and manual backend examples never enable these faults.

Each malformed upstream case is also replayed alongside a healthy SSE stream on the same HTTP/2 connection. The malformed stream must reset without normal completion while the sibling finishes with its own request ID and independent backend receipt. L7 catchall examples use an explicit `PATH STARTS_WITH /` condition: an empty `matchSets` list matches nothing.

## Normative references

- [2026-07-28 Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [2026-07-28 base protocol and metadata](https://modelcontextprotocol.io/specification/2026-07-28/basic/index)
- [2026-07-28 versioning](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning)
- [2026-07-28 progress](https://modelcontextprotocol.io/specification/2026-07-28/basic/patterns/progress)
- [2026-07-28 schema](https://github.com/modelcontextprotocol/modelcontextprotocol/blob/main/schema/2026-07-28/schema.ts)

## Independent SDK and review regressions

Install the explicitly pinned official `mcp==2.3.0` and `h2==4.3.0` in a campaign environment. Add `--require-sdk` to the isolated runner. This verifies a real official SDK client and backend in both JSON and finite SSE modes, including 2,000-key objects and 700 KiB uploads with independent raw-body and execution journals. It qualifies that tool slice only; it does not qualify OAuth, MRTR, subscriptions or every SDK capability.

The H2 zero-window test holds a real 64 MiB SDK result until the Gateway reports its 50 MiB high-water pause, releases flow control, checks exact result bytes and the 10 MiB low-water resume, then resets a separate stream owning an actual long-running SDK task and requires backend cancellation. A backend that reads only the upload prefix and resets proves partial application receipt and absence of normal completion; this is separate from pre-connect retry and response-framing faults.

Gateway validation error bodies are asserted separately from backend errors. Only `2026-07-28` is accepted; malformed or unknown future versions are denied, with zero backend arrival. Long unrelated headers must pass with a real L7 policy while an oversized referenced operand must be refused.
