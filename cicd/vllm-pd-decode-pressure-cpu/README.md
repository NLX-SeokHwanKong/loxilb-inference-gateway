# vllm-pd-decode-pressure-cpu — P/D decode selection under KV pressure (GPU-free)

Drives the KV-pressure term of the P/D **decode** selector end-to-end against a live
gateway, with no GPU and no inference engine: five `mock_vllm.py` backends (2 prefill +
3 decode) behind one P/D service, each decode mock serving its own `vllm:kv_cache_usage_perc`
(or `sglang:token_usage`) on `/metrics`, and the decode that served each request read off the
mock's own `X-Decode-Ep` response header. The gateway's `X-Request-Id` receipt
(`___prefill_addr_…___decode_addr_<ip>:<nixl>_<uuid>`) is recorded next to it and every drive
summary prints how often the two disagree: the receipt is stamped at admission and the decode
connect-time fallback does not re-render it (S6 shows this; it is reported, not asserted).

| Knob | Where | Effect under test |
|---|---|---|
| `LLB_PD_DECODE_KV_WEIGHT=W` | gateway process env (getenv-once) | `0` = shipped min-load scorer; `W>0` adds `kv_pct × W / 100` to a decode EP's score |

## Why a separate scenario

- The weight is read **once at process start**, so the W=0 identity case and the W=8
  pressure cases need two gateway lifecycles; `validation.sh` re-invokes `config.sh`
  between them (the `vllm-pd-admission-cpu` pattern).
- None of the existing P/D scenarios can observe a pressure ordering: their mocks serve no
  usage family by default (`mock_vllm.py` metrics mode `absent`), the SGLang and TRT-LLM
  mocks serve no `/metrics` at all, and `vllm-pd-admission-cpu`'s reflect-echo backends
  have no metrics either. This scenario switches every mock explicitly.

## Cases (Phase A plan §5.1)

| # | Setup (D1 / D2 / D3 = decode mocks) | Expected |
|---|---|---|
| S1 | `W=0`, D1 92 % / D2 40 % / D3 40 % | distribution is the shipped one: a third each, pure RR 3-cycle; with `BASELINE_IMAGE` set, per-decode counts equal the untouched baseline binary's |
| S2 | `W=8`, same usage | D1 receives 0; D2/D3 split by RR |
| S3 | `W=8`, D1 `/metrics` absent (stale after 30 s) | D1 neutralised to the fleet mean → a third again; zero loss |
| S4 | `W=8`, all three absent | counts equal S1 (all-unknown == W=0) |
| S5 | `W=8`, `X-Conversation-Id` pinned to D1, then D1 made the most pressured | keyed requests stay on D1; unkeyed avoid it |
| S6 | `W=8`, D1 mock killed (connect refused) | 12/12 answered 200 by another decode; `[PD_DECODE_CONNECT]` retry lines appear |
| S7 | `W=8`, usage family omitted on all decodes (TRT-LLM-like) | unknown, not 0 %: counts equal S1 |
| S8 | `W=8`, SGLang family (`token_usage`) 0.92 / 0.40 / 0.40 | same as S2 |
| S9 | `W=8`, env changed in a side process, no restart | no effect (documented getenv-once) |

The PR-3 counters (`loxilb_ai_pd_decode_selected_total{path}`, `…_kv_fill_total`) are
asserted by that PR's re-run, not here.

## Mock admin knobs this scenario added to `../vllm-pd-disagg/mock_vllm.py`

| Call (loopback `:9000` in the EP container) | Effect |
|---|---|
| `POST /admin/metrics-kv-usage/<pct>` | this mock's usage, `0..100` (`0.92` for 92); clears omit |
| `POST /admin/metrics-kv-usage-omit` | queue depth and cache config stay, the usage family is dropped |
| `POST /admin/metrics-mode-sglang` | `sglang:num_queue_reqs` + `sglang:token_usage` instead of the vLLM names |

With no knob touched, mode `vllm` serves the historical body byte for byte; `/admin/reset`
restores everything.

## Run (remote testbed; Docker + `ip netns` are not available on macOS)

```bash
cd cicd/vllm-pd-decode-pressure-cpu
sudo ./config.sh && sudo ./validation.sh ; ./rmconfig.sh
# with the byte-identity reference:
BASELINE_IMAGE=<untouched-main-image-tag> sudo -E ./validation.sh
# LOXILB_DOCKER_IMAGE=<tag> pins the gateway image under test (cicd/common.sh)
```

Success sentinel: `=== SCENARIO-vllm-pd-decode-pressure-cpu [OK] ===`. `N_REQ` (default 30)
sets the requests per drive; `SCRAPE_WAIT` (14 s) and `STALE_WAIT` (45 s) are the waits
for one scraper period and for the 30 s telemetry staleness window.

## Traps

- **Sequential drives on purpose.** With no overlap every candidate has `active_conns` 0
  and the same scraped queue depth (the mock serves `num_requests_waiting 3.0`), so only
  the RR tie-break and the kv term can separate the decodes.
- **The RR offset is not reproducible across lifecycles**, so S1's byte-identity compares
  counts and the 3-cycle shape, never the alignment of two sequences.
- **Staleness is 30 s**, not one scraper period: a mock switched to `absent` keeps its last
  value fresh until `telemetry_stale_ms` has elapsed.
- **Float truncation**: the Go sink stores `uint32(ratio × 100)`; 0.92 and 0.4 read as 92
  and 40 exactly, 0.29 would read as 28.
