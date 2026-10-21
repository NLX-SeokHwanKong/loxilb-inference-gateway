# vLLM P/D Disaggregation CICD Scenario

## Overview

Exit gate for the **vLLM sequential prefill/decode** datapath
(`pd_disagg_mode`, default engine dialect): the gateway rewrites the request
into a prefill probe (`max_tokens=1`, `stream=false`, `kv_transfer_params`),
sends it to a prefill EP, extracts `kv_transfer_params` from the prefill
response, rebuilds the original request for the decode EP, and relays the
decode response (including SSE) to the client. A prefill failure degrades
gracefully (decode recomputes, or mid-request prefill retry on a transport
death) — never a hung client.

Engines are mocked by `mock_vllm.py` (OpenAI-compatible endpoints,
`kv_transfer_params` synthesis, NIXL side-channel listeners, an admin server
with health toggles and one-shot fault knobs).

## Topology

```
l3h1 (10.10.10.1)
      │
      └── llb1 (VIP 10.10.10.254)
             ├── :2020 P/D 1P+1D    ── l3ep1 (31.31.31.1:8000) prefill, nixl 9001
             │                       └─ l3ep2 (32.32.32.1:8000) decode,  nixl 9002
             ├── :2021 non-P/D baseline (l3ep2 as a plain backend)
             ├── :2022 P/D 2P+2D    ── + l3ep3 (33.33.33.1:8000) prefill, nixl 9003
             │                       └─ + l3ep4 (34.34.34.1:8000) decode,  nixl 9004
             ├── :2023 P/D 2P+2D + pd_cache_aware_mode (Tier-1 trie)
             └── :2024 session-header stickiness (HA conversation sync)
```

## Run

```bash
cd cicd/vllm-pd-disagg
./run-pd-cicd.sh                     # config + validation + teardown, logged under logs/
./run-pd-cicd.sh --bail-on-fail      # stop at the first failing phase
./run-pd-cicd.sh --phase=D           # a single phase (see the table below)
./run-pd-cicd.sh --phase=L           # HA failover phase (auto-enables the 2-loxilb topology)
```

Or the raw scenario flow: `./config.sh` → `./validation.sh` → `./rmconfig.sh`
(`LOXILB_DOCKER_IMAGE=<tag>` pins the gateway image).

Success sentinel: `SCENARIO-vllm-pd-disagg [PASS]`.

## Tests

| Block | What it proves |
|---|---|
| T1-T9 | baseline: non-P/D passthrough, `X-Request-Id` auto-inject/preserve format, P/D completions data plane, request-id correlation, SSE streaming, LB statistics, Prometheus P/D metrics, NIXL ports (not HTTP ports) in the request-id receipt |
| Phase A | prefill body rewriting (`max_tokens=1`, `stream=false`, `kv_transfer_params` injection, Content-Length fixup) |
| Phase B | multi-EP pool routing on :2022 (prefill/decode role separation under load) |
| Phase C | the full `X-Request-Id` spec (`___prefill_addr_..___decode_addr_.._<uuid>`) |
| Phase D | failover: EP down/up transitions, prefill mid-request retry, decode re-selection |
| Phase E | concurrency + parallel load (fresh buffers per request, no cross-talk) |
| Phase F | control-plane CRUD (rule add/delete/re-add under live traffic) |
| Phase G | SSE edge cases (fragmented frames, `[DONE]` terminators, keep-alive) |
| Phase H | Prometheus observability (P/D counter families present and moving) |
| Phase I | circuit breaker on TCP-failure triggers (connect-level `failure_count`) |
| Phase J | Tier-0 conversation stickiness |
| Phase K | Tier-1 cache-aware trie routing on :2023 |
| Phase L | HA session restoration across a 2-loxilb MASTER failover (gated; `validation-convsync.sh` / `validation-xsync.sh`) |
| Phase M | `kv_transfer_params` accounting: found/missing as the two arms of one branch, plus the extractor's overflow counter |
| Phase N | worker-scrape outcome accounting: `loxilb_ai_worker_scrape_total{result}` driven through http_error / ok / unparseable / unreachable |
| Phase O | the P/D client-visible status matrix on :2020: normal, prefill 404 / 500 / 429, decode 404 / 500, and a keep-alive pair (normal then prefill 404 on one client connection). Per row: the response, `requests_total{outcome,status}`, `pd_requests_total{phase,status}`, `http_responses_by_status{class}`, the TTFB sample count and both engines' call counts; every other family flat, `outcome="aborted"` and the lifecycle's aborted/client included. Series are matched by label set, in any order (`m_sum`, `m_series`), never by where a label sits in the line. Then, on a model-routed P/D rule the phase adds on :2030 (plain HTTP) and removes: an unknown model in one read and with head and body in separate reads (503 `model_unavailable`: `requests_total{outcome="denied",status="503"}` +1 under `model="other"` and `{outcome="completed",status="503"}` +0, 5xx class +1, TTFB +0, no lifecycle record, both engines +0), and a keep-alive pair whose second request is that refusal. The class counter and TTFB histogram are pulled every 10 s, so each row polls for its sample (two-pull timeout) instead of sleeping |
| Phase P | how a P/D answer ends for the client, on a P/D rule without `sse_mode` (:2031, added and removed by the phase). **PA:** a close-delimited stream (100 chunks 2 s apart, past the 180 s generic P/D idle cap; the engine ends it by closing): every chunk and `[DONE]`, no status line in the body, curl rc 0, the client's connection closed within 10 s of the stream's end, no P/D reaper line. **PB:** a chunked stream (as vLLM's uvicorn) twice on one kept client connection: both 200 ending in `[DONE]`, connects 1 then 0. **PC:** a length-delimited non-streaming answer twice on one kept client connection: both 200, connects 1 then 0. **PD/PE:** a chunked stream whose engine drops `[DONE]` and goes silent (PD) or closes mid-stream (PE): PD records decode/truncated, ends with the gateway's `[DONE]` and moves `loxilb_ai_pd_terminator_synthesized_total` +1; PE records decode/error, the client sees the cut (curl rc 18), the terminator counter stays flat. PD records requests completed/200 (no cause) +1, PE aborted/200 `cause="engine"` +1. **PF:** a short (2 chunks) and a long (20 chunks) chunked stream 200 ms apart: one TTFT and one decode-duration sample each, the long duration >= 3x the short one, the TTFTs within 1/4 of the long duration of each other. **PG:** the client leaves during the prefill (the prefill engine holds its answer 8 s, the client gives up at 2 s): HTTP 000, requests aborted/none `cause="client"` +1, lifecycle aborted/client +1, no TTFT or request-duration sample. **PH:** the client leaves during a paced decode stream, with its 200 and some chunks: requests aborted/200 `cause="client"` +1, lifecycle aborted/client +1, one TTFT sample and no request-duration sample. **PI:** the client leaves before any answer on a plain AI rule (`sse_mode`, no P/D, :2032, added and removed by the phase) in front of the decode engine, which holds its answer 8 s; the client gives up at 2 s: HTTP 000, requests aborted/none `cause="client"` +1, no lifecycle record (no lifecycle family pairs with a request off P/D), no TTFT or request-duration sample. **PJ:** on the same rule the decode engine reads the request and closes with no byte written (`/admin/drop-next`): the client gets the gateway's 502 `backend_unreachable`, recorded requests completed/502 +1 (as a refused connect or a TLS leg failing before its answer), no lifecycle record. **PL:** on the same rule, the rule is deleted 2 s into a request the decode engine holds: HTTP 000, requests aborted/none `cause="gateway"` +1 (the listener went before any answer), no lifecycle record. **PK:** the P/D rule again with a capacity of one and a queue of one (`fc_max_outstanding`, `fc_max_queue_depth`): a holder takes the unit while the prefill engine holds its answer 8 s, a second request waits in the queue and its client gives up at 2 s. The queued one is requests aborted/none `cause="client"` +1 and lifecycle aborted/client +1 (the client left at the gate); the holder completed/200 and complete/success; nothing else moves. **PM:** that P/D rule is deleted 2 s into a request the prefill engine holds: HTTP 000, requests aborted/none `cause="gateway"` +1 and lifecycle aborted/gateway +1, nothing else. **PN:** the P/D rule again; it is deleted 3 s into a paced decode stream, with the client's 200 and chunks out: requests aborted/200 `cause="gateway"` +1 and lifecycle decode/truncated +1 (after the answer began: 8, not 10), nothing else. Prints the gateway's data-plane log and sockets for PA. After every phase, **TZ:** `loxilb_ai_reap_fallback_total` and `loxilb_ai_route_host_missing_total` are exported and read 0 |

The **origin-5xx breaker demotion** for this dialect (a prefill 5xx is
swallowed as decode-recompute, so only the origin-error streak can demote an
erroring-but-TCP-healthy prefill) is pinned by leg L of
`../sglang-pd-disagg/` — that scenario hosts both engine mocks on one
gateway, which is exactly the coexistence surface the demotion must respect.

Fault knobs on each mock (loopback admin `:9000`): `/admin/health-fail`,
`/admin/health-ok`, `/admin/fail-next` (one-shot origin 500; `?status=N` picks
another status, `&skip=K` lets K inference requests through first),
`/admin/keepalive-on|off` (HTTP/1.1 replies without `Connection: close`, so a client
may keep its connection to the gateway), `/admin/stream-pace?ms=N&chunks=K` (a decode stream of K chunks N ms apart; the mock
answers other requests while it waits), `/admin/stream-chunked-on|off` (a stream as HTTP/1.1 chunked, ended by the 0-chunk, instead of
HTTP/1.0 delimited by the close), `/admin/answer-delay?ms=N` (every inference answer held N ms after
it is counted: a slow engine, for a client that leaves while it waits), `/admin/reset`, `/admin/status` (includes
`inference_requests`, every inference POST the engine received); plus
`--fail-every N` at process start.
