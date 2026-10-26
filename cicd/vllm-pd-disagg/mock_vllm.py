#!/usr/bin/env python3
"""
Mock vLLM server for P/D disaggregation CICD testing.

Simulates OpenAI-compatible API with prefill/decode behavior:
- Prefill mode: Returns non-streaming response with kv_transfer_params
- Decode mode: Returns streaming SSE response, accepts kv_transfer_params

Usage:
    python3 mock_vllm.py --role prefill --port 8000
    python3 mock_vllm.py --role decode --port 8000
"""

import argparse
import json
import os
import socket
import threading
import time
import uuid
from http.server import HTTPServer, ThreadingHTTPServer, BaseHTTPRequestHandler

MODEL_NAME = "Qwen/Qwen3-0.6B"
SERVER_ROLE = "normal"  # Set by command-line args
NIXL_PORT = 0           # Set by main(); used by _make_kv_params to avoid hardcoded 9001 (R2 fix)
EP_IDX = 0              # Set by main() from --ep-idx CLI flag; echoed back via X-Prefill-Ep / X-Decode-Ep
                        # response headers so the HA failover test can correlate which (prefill_ep, decode_ep) pair
                        # served each request (required for restore_rate).
_health_ok = True       # Toggled by admin server; GIL-safe (CPython, simple bool assignment)
_request_count = 0      # Incremented per POST request; used by --fail-every.
                        # WR-07: protected by _request_count_lock — under the
                        # single-threaded HTTPServer the GIL covers a bare
                        # `+= 1`, but if anyone swaps for ThreadingHTTPServer
                        # the read-modulo-write of --fail-every becomes
                        # non-deterministic. Lock is cheap; harden now.
_request_count_lock = threading.Lock()
_fail_next = False      # One-shot: next inference request answers HTTP 500 (or N)
                        # (origin-computed error — the connection stays up).
                        # Armed via POST /admin/fail-next, cleared on consume
                        # or /admin/reset; mirrors the mock_sglang_pd.py knob.
_fail_next_status = 500 # The status the one-shot answers: /admin/fail-next?status=N
                        # (100..599). Default 500, so every existing caller keeps
                        # its meaning; a 4xx arms an origin reject (MON-M matrix).
_fail_next_skip = 0     # &skip=K: let K inference requests through before the
                        # one-shot fires, so a fault can land on the second of two
                        # requests a client sends on one kept connection.
_inference_count = 0    # Every POST to an inference path, injected faults included.
                        # Unlike _request_count (moved only under --fail-every), it
                        # always counts, so a scenario can prove whether a leg was
                        # called at all. Read via GET /admin/status.
_keepalive = False      # Off: every JSON reply is HTTP/1.0 + "Connection: close" (the
                        # historical shape). On: HTTP/1.1 without "close", as vLLM's
                        # uvicorn answers, so a client may keep its connection to the
                        # gateway across requests. The mock still closes its own
                        # socket after each reply (legal for HTTP/1.1, and the
                        # single-threaded server never parks on an idle connection).
_args = None            # Set by main(); allows handlers to access parsed args
_serial = threading.Lock()  # Held across every request on the API port, so requests
                        # are handled one at a time exactly as under the historical
                        # single-threaded HTTPServer. The server is threaded only so
                        # that a paced stream (below) can let go of it while it waits.
_stream_pace_ms = 0     # POST /admin/stream-pace?ms=N&chunks=K: a decode stream sends
_stream_chunks = 0      # K content chunks N ms apart, releasing _serial while it waits,
                        # so the health probes and /metrics scrapes keep being answered
                        # through a generation longer than the gateway's P/D idle cap
                        # (row 14: a P/D stream on a rule without sse_mode). 0 is the
                        # historical 10-token stream at 50 ms; /admin/reset restores it.
_stream_chunked = False # POST /admin/stream-chunked-on|off. Off: a stream is HTTP/1.0,
                        # "Connection: close", no length, delimited by the mock closing
                        # (the historical shape). On: HTTP/1.1 Transfer-Encoding: chunked,
                        # ended by the 0-chunk, as vLLM's uvicorn streams; "close" only
                        # when keepalive is off. The mock closes its socket either way.
_stream_end_mode = ""   # POST /admin/stream-end?mode=hold&sec=N|mode=cut[&at=mid]: how a
_stream_end_hold = 0    # decode stream ends instead of "data: [DONE]" (and the 0-chunk).
_stream_end_at = ""     # hold: the engine goes silent for N s with the socket open, then
                        # closes (a dropped terminator). cut: it closes at once (an
                        # engine dying). Without at=mid the stream has sent its finish
                        # chunk (finish_reason "stop") first: the generation finished and
                        # only the terminator is missing. at=mid: the finish chunk is
                        # never sent; the stream ends after a content chunk, mid-
                        # generation. Both leave the gateway to end the client's answer
                        # (Q2). "" is the historical ending; /admin/reset restores it.
_drop_next = False      # One-shot: POST /admin/drop-next: the next inference request is
                        # read (and counted) and its connection closed with no byte
                        # written: an engine that dies before its answer. Cleared on
                        # consume or /admin/reset.
_answer_delay_ms = 0    # POST /admin/answer-delay?ms=N (1..60000): every inference request
                        # is counted on arrival, then held N ms before its answer (a slow
                        # engine, for a client that leaves while it waits). 0 = no hold;
                        # /admin/reset restores it.
# The gateway's kv_transfer_params buffer is PD_KV_PARAMS_MAX_LEN = 65536 (sockproxy.h);
# the extractor overflows on `val_len >= capacity`. Target comfortably above it so the
# arm does not sit on the boundary, where a one-field change to the object could silently
# drop the payload back under the bound and turn the overflow arm green-but-vacuous.
PD_KV_PARAMS_OVERSIZE_TARGET = 80000
_kv_params_mode = "on"  # "on" | "off" | "oversize" — shape of kv_transfer_params in the
                        # PREFILL response. Default "on" is byte-identical to the
                        # historical behaviour, so the scenarios that docker-cp this file
                        # (llamacpp-lb, sglang-pd-disagg, trtllm-pd-disagg) are unaffected.
                        # A runtime admin knob rather than an env var on purpose: the gateway
                        # reads this per response, so an arm can switch shape without
                        # recreating the container, and the control arm can be the identical
                        # request with only this one field changed.
                        #   off      -> the key is OMITTED entirely; the gateway parses a
                        #               prefill response that simply has no params
                        #   oversize -> the value exceeds PD_KV_PARAMS_MAX_LEN (64KB), which
                        #               is the only way to reach the extractor's overflow arm
_metrics_mode = "absent"  # "absent" | "vllm" | "garbage" | "sglang" — what GET /metrics answers.
                          # Default "absent" is byte-identical to the historical
                          # behaviour (the path falls through to 404), so every
                          # scenario that docker-cp's this file is unaffected and
                          # phases A-M see exactly the fixture they always saw.
                          #
                          # This exists because the gateway's own worker scraper polls
                          # /metrics on every P/D endpoint every 10s and reports the
                          # OUTCOME as loxilb_ai_worker_scrape_total{result}. The three
                          # modes are the three outcomes a reachable endpoint can produce,
                          # and they are switched at runtime rather than at spawn so one
                          # arm's control is the same endpoint with one field changed:
                          #   absent  -> 404            -> result="http_error"
                          #   vllm    -> 200 + the recognized series -> result="ok"
                          #   garbage -> 200 + nothing recognized    -> result="unparseable"
                          # The fourth outcome, result="unreachable", needs the listener
                          # gone and so cannot be a mode here.
                          #   sglang  -> 200 + the SGLang family names (num_queue_reqs,
                          #              token_usage) the parser maps to the same two signals;
                          #              added for the P/D decode-pressure scenario, whose S8
                          #              case tests the gateway-side parser on that family.
_kv_usage_pct = None      # None = the historical static body (kv_cache_usage_perc 0.25 in mode
                          # "vllm", token_usage 0.25 in mode "sglang"). A number 0..100, set by
                          # POST /admin/metrics-kv-usage/<pct>, gives THIS mock its own KV-cache
                          # pressure so a decode-selection case can shape each decode EP
                          # differently. /admin/reset restores None.
_kv_usage_omit = False    # POST /admin/metrics-kv-usage-omit: serve the queue-depth family and
                          # the cache config but NO usage family — the shape of an engine that
                          # does not export KV pressure (TRT-LLM-like). The gateway must read
                          # "unknown" here, never "0 %". Cleared by /admin/reset or by setting a
                          # value with /admin/metrics-kv-usage/<pct>.

# The narrow 3-series set pkg/aimetrics/lineparser.go recognizes. Served verbatim in
# mode "vllm": the parser reports "found" on the FIRST recognized family, so all three
# are emitted to keep this an honest vLLM /metrics rather than a minimum that happens
# to trip the parser. Values are static — nothing in this scenario asserts on them,
# and a moving value would make the sample the arm under test instead of the outcome.
_VLLM_METRICS_BODY = """# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting 3.0
# HELP vllm:kv_cache_usage_perc KV-cache usage as a fraction.
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc 0.25
# HELP vllm:cache_config_info Cache configuration.
# TYPE vllm:cache_config_info gauge
vllm:cache_config_info{num_gpu_blocks="2048",block_size="16"} 1.0
"""

# Mode "garbage": a 200 with a well-formed exposition body that carries NO recognized
# family. Deliberately not empty and not malformed — "unparseable" in the scraper's
# vocabulary means "answered, readable, but nothing we know", and an empty body would
# leave it ambiguous with a truncated read.
_GARBAGE_METRICS_BODY = """# HELP some_other_exporter_up Nothing this gateway parses.
# TYPE some_other_exporter_up gauge
some_other_exporter_up 1.0
"""


def _kv_usage_ratio_text():
    """The usage ratio the two bodies below emit: the historical 0.25 unless an
    admin call set this mock's percentage (0.92 for 92, 0.4 for 40)."""
    pct = 25.0 if _kv_usage_pct is None else float(_kv_usage_pct)
    return f"{pct / 100.0:g}"


def _vllm_metrics_body():
    """Mode "vllm". With no admin knob touched this is _VLLM_METRICS_BODY byte for
    byte, so every scenario that only switches the mode sees the historical body.
    With /admin/metrics-kv-usage/<pct> the usage line carries that value; with
    /admin/metrics-kv-usage-omit the usage family is absent and the other two
    families stay, which is how the gateway tells "not reported" from "0 %"."""
    if _kv_usage_pct is None and not _kv_usage_omit:
        return _VLLM_METRICS_BODY
    usage = "" if _kv_usage_omit else (
        "# HELP vllm:kv_cache_usage_perc KV-cache usage as a fraction.\n"
        "# TYPE vllm:kv_cache_usage_perc gauge\n"
        f"vllm:kv_cache_usage_perc {_kv_usage_ratio_text()}\n")
    return (
        "# HELP vllm:num_requests_waiting Number of requests waiting to be processed.\n"
        "# TYPE vllm:num_requests_waiting gauge\n"
        "vllm:num_requests_waiting 3.0\n"
        + usage +
        "# HELP vllm:cache_config_info Cache configuration.\n"
        "# TYPE vllm:cache_config_info gauge\n"
        'vllm:cache_config_info{num_gpu_blocks="2048",block_size="16"} 1.0\n')


def _sglang_metrics_body():
    """Mode "sglang": the SGLang family names for the same two signals
    (pkg/aimetrics/lineparser.go maps sglang:num_queue_reqs to the queue depth and
    sglang:token_usage to the KV-cache usage). Same knobs as the vLLM body."""
    usage = "" if _kv_usage_omit else (
        "# HELP sglang:token_usage The token usage\n"
        "# TYPE sglang:token_usage gauge\n"
        f"sglang:token_usage {_kv_usage_ratio_text()}\n")
    return (
        "# HELP sglang:num_queue_reqs The number of requests in the waiting queue\n"
        "# TYPE sglang:num_queue_reqs gauge\n"
        "sglang:num_queue_reqs 3.0\n"
        + usage)

# Identity of this server instance, echoed as the X-Served-By response header so
# a test can attribute a proxied response to the endpoint that produced it. In
# Kubernetes POD_NAME (downward API) names the pod; hostname covers docker/netns.
SERVED_BY = os.environ.get("POD_NAME") or socket.gethostname()


class MockVLLMHandler(BaseHTTPRequestHandler):
    """Handles OpenAI-compatible API requests."""

    def log_message(self, format, *args):
        """Log to stdout for debugging."""
        print(f"[{SERVER_ROLE}] {format % args}")

    def _send_json(self, status, body):
        """Send a JSON response."""
        data = json.dumps(body).encode("utf-8")
        if _keepalive:
            self.protocol_version = "HTTP/1.1"
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        if not _keepalive:
            self.send_header("Connection", "close")
        # Echo back X-Request-Id if present
        req_id = self.headers.get("X-Request-Id", "")
        if req_id:
            self.send_header("X-Request-Id", req_id)
        # Emit X-Prefill-Ep / X-Decode-Ep based on SERVER_ROLE so the HA
        # validation.sh can correlate which (prefill_ep, decode_ep) served each
        # turn before/after failover and compute restore_rate.
        # Verify with: curl -sI -X POST http://<ep>:8000/v1/chat/completions -d '{}' | grep -i X-Prefill-Ep
        if SERVER_ROLE == "prefill":
            self.send_header("X-Prefill-Ep", str(EP_IDX))
        elif SERVER_ROLE == "decode":
            self.send_header("X-Decode-Ep", str(EP_IDX))
        self.send_header("X-Served-By", SERVED_BY)
        self.close_connection = True
        self.end_headers()
        self.wfile.write(data)

    def _send_sse(self, chunks):
        """Send SSE streaming response."""
        chunked = _stream_chunked
        if chunked:
            self.protocol_version = "HTTP/1.1"
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        if chunked:
            self.send_header("Transfer-Encoding", "chunked")
        if not (chunked and _keepalive):
            self.send_header("Connection", "close")
        req_id = self.headers.get("X-Request-Id", "")
        if req_id:
            self.send_header("X-Request-Id", req_id)
        # Mirror the X-Prefill-Ep / X-Decode-Ep echo from _send_json
        # so SSE streaming responses also carry the EP-identity header.
        if SERVER_ROLE == "prefill":
            self.send_header("X-Prefill-Ep", str(EP_IDX))
        elif SERVER_ROLE == "decode":
            self.send_header("X-Decode-Ep", str(EP_IDX))
        self.send_header("X-Served-By", SERVED_BY)
        self.close_connection = True
        self.end_headers()

        # A paced stream lets the other requests through while it waits; the
        # historical stream keeps the server to itself, as it always did.
        pace = _stream_pace_ms
        end_mode, end_hold, end_at = _stream_end_mode, _stream_end_hold, _stream_end_at
        if end_mode and end_at == "mid" and len(chunks) > 1:
            # The finish chunk is never sent: the engine ends mid-generation.
            chunks = chunks[:-1]
        # A hold must not stall the health probes and scrapes for its length either.
        let_go = pace or end_mode == "hold"
        if let_go:
            _serial.release()
        def put(data):
            if chunked:
                data = b"%x\r\n%s\r\n" % (len(data), data)
            self.wfile.write(data)
            self.wfile.flush()

        try:
            for chunk in chunks:
                put(f"data: {json.dumps(chunk)}\n\n".encode("utf-8"))
                time.sleep(pace / 1000.0 if pace else 0.05)

            where = "mid-generation" if end_at == "mid" else "after the finish chunk"
            if end_mode == "hold":
                print(f"[{SERVER_ROLE}] [stream-end] no terminator {where}; silent "
                      f"{end_hold} s, then close", flush=True)
                time.sleep(end_hold)
            elif end_mode == "cut":
                print(f"[{SERVER_ROLE}] [stream-end] no terminator {where}; closing now",
                      flush=True)
            else:
                put(b"data: [DONE]\n\n")
                if chunked:
                    self.wfile.write(b"0\r\n\r\n")
                    self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # Under a stream-end knob the gateway ends the answer itself and closes
            # the leg; any other stream keeps the historical behaviour.
            if not end_mode:
                raise
        finally:
            if let_go:
                _serial.acquire()

    def do_GET(self):
        with _serial:
            self._do_get()

    def do_POST(self):
        with _serial:
            self._do_post()

    def _do_get(self):
        if self.path == "/v1/models":
            self._handle_models()
        elif self.path == "/health":
            if _health_ok:
                self._send_json(200, {"status": "ok", "role": SERVER_ROLE})
            else:
                self._send_json(503, {"status": "fail", "role": SERVER_ROLE})
        elif self.path == "/metrics":
            self._handle_metrics()
        else:
            self._send_json(404, {"error": "Not found"})

    def _handle_metrics(self):
        """The gateway's worker scraper polls this every 10s per P/D endpoint.

        In mode "absent" the request falls through to the same 404 the path
        produced before this route existed — the branch is here rather than in
        do_GET so the default answer stays a single code path with the other
        unknown paths, and so switching modes cannot change anything else.
        """
        if _metrics_mode == "vllm":
            body = _vllm_metrics_body().encode()
        elif _metrics_mode == "sglang":
            body = _sglang_metrics_body().encode()
        elif _metrics_mode == "garbage":
            body = _GARBAGE_METRICS_BODY.encode()
        else:
            self._send_json(404, {"error": "Not found"})
            return
        self.send_response(200)
        # text/plain, as a Prometheus exporter answers. The gateway's parser is
        # content-type agnostic, so this is fidelity to the real thing rather
        # than something an assert depends on.
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _do_post(self):
        content_len = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(content_len).decode("utf-8") if content_len > 0 else "{}"

        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._send_json(400, {"error": "Invalid JSON"})
            return

        # One-shot fail-next fault injection (origin-computed HTTP 500 with the
        # TCP connection healthy — the exact shape the breaker's origin-error
        # streak demotes on; connect-level failure_count never sees it).
        global _fail_next, _inference_count
        if self.path in ("/v1/chat/completions", "/v1/completions"):
            with _request_count_lock:
                _inference_count += 1
            if _answer_delay_ms:
                time.sleep(_answer_delay_ms / 1000.0)
        global _drop_next
        if _drop_next and self.path in ("/v1/chat/completions", "/v1/completions"):
            _drop_next = False
            print(f"[{SERVER_ROLE}] DROP-NEXT consumed: closing with no answer "
                  f"reqid={self.headers.get('X-Request-Id', '-')}", flush=True)
            self.close_connection = True
            return
        global _fail_next_skip
        if _fail_next and _fail_next_skip > 0 and \
                self.path in ("/v1/chat/completions", "/v1/completions"):
            _fail_next_skip -= 1
        elif _fail_next and self.path in ("/v1/chat/completions", "/v1/completions"):
            _fail_next = False
            print(f"[{SERVER_ROLE}] FAIL-NEXT consumed status={_fail_next_status} "
                  f"reqid={self.headers.get('X-Request-Id', '-')}", flush=True)
            self._send_json(_fail_next_status, {"error": "injected_fault_oneshot",
                                                "status": _fail_next_status,
                                                "role": SERVER_ROLE})
            return

        # --fail-every fault injection (HTTP 500; an isolated 5xx does not trip
        # the breaker — the origin-error streak needs consecutive 5xx, and the
        # interleaved successes here keep resetting it)
        global _request_count
        if _args is not None and _args.fail_every > 0:
            # WR-07: atomic increment + modulo under a lock so the
            # injection cadence is deterministic regardless of HTTPServer
            # threading model. Capture the post-increment value inside
            # the lock then drop the lock before doing I/O.
            with _request_count_lock:
                _request_count += 1
                snapshot = _request_count
                should_fail = (snapshot % _args.fail_every == 0)
            if should_fail:
                self._send_json(500, {"error": "injected_fault", "request_count": snapshot})
                return

        # Log incoming request metadata for CICD verification (T4c max_tokens, T5 ID correlation)
        req_id = self.headers.get("X-Request-Id", "")
        if req_id:
            print(f"[{SERVER_ROLE}] X-Request-Id: {req_id}", flush=True)
        max_tokens = req.get("max_tokens")
        if max_tokens is not None:
            print(f"[{SERVER_ROLE}] max_tokens: {max_tokens}", flush=True)
        user_id = req.get("user")
        if user_id:
            print(f"[{SERVER_ROLE}] user_id: {user_id}", flush=True)

        if self.path == "/v1/chat/completions":
            self._handle_chat_completions(req)
        elif self.path == "/v1/completions":
            self._handle_completions(req)
        else:
            self._send_json(404, {"error": "Not found"})

    def _handle_models(self):
        self._send_json(200, {
            "object": "list",
            "data": [{
                "id": MODEL_NAME,
                "object": "model",
                "created": int(time.time()),
                "owned_by": "mock-vllm",
            }]
        })

    def _handle_chat_completions(self, req):
        """Handle /v1/chat/completions based on server role."""
        cmpl_id = f"chatcmpl-{uuid.uuid4().hex[:12]}"
        model = req.get("model", MODEL_NAME)
        stream = req.get("stream", False)

        # Log received kv_transfer_params for decode verification
        kv_params = req.get("kv_transfer_params")
        if kv_params:
            print(f"[{SERVER_ROLE}] [decode] kv_transfer_params: {json.dumps(kv_params)[:200]}", flush=True)

        if SERVER_ROLE == "prefill":
            # Prefill always returns non-streaming with kv_transfer_params
            self._send_prefill_response(cmpl_id, model)
        elif SERVER_ROLE == "decode":
            if stream:
                self._send_decode_stream(cmpl_id, model)
            else:
                self._send_decode_response(cmpl_id, model)
        else:
            # Normal mode — just respond normally
            if stream:
                self._send_decode_stream(cmpl_id, model)
            else:
                self._send_decode_response(cmpl_id, model)

    def _handle_completions(self, req):
        """Handle /v1/completions (legacy endpoint)."""
        cmpl_id = f"cmpl-{uuid.uuid4().hex[:12]}"
        model = req.get("model", MODEL_NAME)

        if SERVER_ROLE == "prefill":
            payload = {
                "id": cmpl_id,
                "object": "text_completion",
                "created": int(time.time()),
                "model": model,
                "choices": [{
                    "index": 0,
                    "text": "",
                    "finish_reason": "length"
                }],
                "usage": {"prompt_tokens": 10, "completion_tokens": 1, "total_tokens": 11},
            }
            kvp = self._make_kv_params()
            if kvp is not None:
                payload["kv_transfer_params"] = kvp
            self._send_json(200, payload)
        else:
            self._send_json(200, {
                "id": cmpl_id,
                "object": "text_completion",
                "created": int(time.time()),
                "model": model,
                "choices": [{
                    "index": 0,
                    "text": "The answer is 42.",
                    "finish_reason": "stop"
                }],
                "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
            })

    def _send_prefill_response(self, cmpl_id, model):
        """Prefill: non-streaming response with kv_transfer_params."""
        payload = {
            "id": cmpl_id,
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": ""},
                "finish_reason": "length"
            }],
            "usage": {"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13},
        }
        kvp = self._make_kv_params()
        if kvp is not None:
            payload["kv_transfer_params"] = kvp
        self._send_json(200, payload)

    def _send_decode_response(self, cmpl_id, model):
        """Decode: non-streaming response."""
        self._send_json(200, {
            "id": cmpl_id,
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "Hello! I am a mock vLLM decode response."},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
        })

    def _send_decode_stream(self, cmpl_id, model):
        """Decode: SSE streaming response."""
        tokens = ["Hello", "!", " I", " am", " a", " mock", " vLLM", " decode", " response", "."]
        if _stream_chunks:
            tokens = [f" t{i}" for i in range(_stream_chunks)]
        chunks = []
        for i, token in enumerate(tokens):
            chunks.append({
                "id": cmpl_id,
                "object": "chat.completion.chunk",
                "created": int(time.time()),
                "model": model,
                "choices": [{
                    "index": 0,
                    "delta": {"content": token} if i > 0 else {"role": "assistant", "content": token},
                    "finish_reason": None
                }]
            })
        # Final chunk with finish_reason
        chunks.append({
            "id": cmpl_id,
            "object": "chat.completion.chunk",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "delta": {},
                "finish_reason": "stop"
            }]
        })
        self._send_sse(chunks)

    @staticmethod
    def _make_kv_params(tp_size=1, blocks_per_rank=4):
        """Generate mock kv_transfer_params matching real vLLM nixl_connector.py format.

        remote_block_ids is a nested array: one list of block IDs per TP rank.

        Returns None when the kv-params mode is "off", in which case the caller OMITS
        the key rather than sending a null: the gateway's extractor searches the
        response for the key, so a null would still be a present-but-unusable value
        and would not exercise the absent path the way a real non-disaggregated
        serve does.
        """
        if _kv_params_mode == "off":
            return None
        if _kv_params_mode == "oversize":
            # Exceed the extractor's 64KB buffer (PD_KV_PARAMS_MAX_LEN, sockproxy.h).
            # MEASURED, not estimated: a first cut hard-coded 12000 blocks on a guessed
            # digit width and produced 61122 bytes — UNDER the bound, so the overflow arm
            # would never have fired and the family would have read as dead. Grow until the
            # COMPACT serialization clears the bound with margin (the wire form carries
            # separators and is therefore never smaller than what is measured here).
            blocks_per_rank = 12000
            while True:
                probe = [list(range(i * 100, i * 100 + blocks_per_rank))
                         for i in range(tp_size)]
                if len(json.dumps(probe, separators=(',', ':'))) >= PD_KV_PARAMS_OVERSIZE_TARGET:
                    break
                blocks_per_rank *= 2
        remote_block_ids = [
            list(range(i * 100, i * 100 + blocks_per_rank))
            for i in range(tp_size)
        ]
        return {
            "do_remote_prefill": True,
            "do_remote_decode": False,
            "remote_block_ids": remote_block_ids,
            "remote_host": "127.0.0.1",
            "remote_port": NIXL_PORT,
            "remote_engine_id": f"prefill-engine-{uuid.uuid4().hex[:8]}",
            "remote_request_id": str(uuid.uuid4()),
            "tp_size": tp_size
        }


class AdminHandler(BaseHTTPRequestHandler):
    """Admin HTTP server — health toggle and status.

    Binds to 127.0.0.1:9000 only (loopback). Never exposed externally.
    Access via: docker exec <ep> curl -s -X POST http://localhost:9000/admin/health-fail

    SECURITY: loopback binding limits reachability to 'docker exec'.
    Additionally, if the env var MOCK_VLLM_ADMIN_TOKEN is set, every
    admin request MUST carry a matching 'X-Admin-Token' header. This
    upgrades the boundary from 'authentication-by-network-location'
    to 'authentication-by-shared-secret', so the mock is safe to reuse
    outside CICD (e.g. as a chaos-engineering tool in a shared cluster).
    When the env var is unset the loopback-only behaviour is preserved
    for backwards compatibility with the existing CICD harness (WR-11).
    """

    def _auth_ok(self) -> bool:
        """WR-11: shared-secret check. Returns True if no token is
        configured (CICD mode) or if the request carries the right one."""
        expected = os.environ.get("MOCK_VLLM_ADMIN_TOKEN", "")
        if not expected:
            return True
        return self.headers.get("X-Admin-Token", "") == expected

    def _reply_json(self, obj):
        data = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        global _health_ok, _fail_next, _fail_next_status, _kv_params_mode, _metrics_mode
        global _keepalive, _fail_next_skip, _stream_pace_ms, _stream_chunks
        global _stream_chunked, _stream_end_mode, _stream_end_hold, _stream_end_at
        global _answer_delay_ms
        global _drop_next
        global _kv_usage_pct, _kv_usage_omit
        if not self._auth_ok():
            self.send_response(401)
            self.end_headers()
            return
        path, _, query = self.path.partition("?")
        if path == "/admin/fail-next":
            # ?status=N picks the injected status; no query keeps the historical 500.
            # &skip=K lets K inference requests through first.
            status, skip = 500, 0
            for kv in query.split("&") if query else []:
                k, _, v = kv.partition("=")
                if k == "status":
                    status = int(v) if v.isdigit() else 0
                elif k == "skip":
                    skip = int(v) if v.isdigit() else -1
            if not 100 <= status <= 599 or skip < 0:
                self.send_response(400)
                self.end_headers()
                return
            _fail_next = True
            _fail_next_status = status
            _fail_next_skip = skip
            self._reply_json({"fail_next": True, "fail_next_status": status,
                              "fail_next_skip": skip})
            print(f"[{SERVER_ROLE}] [admin] fail-next ARMED status={status} skip={skip}",
                  flush=True)
        elif path == "/admin/stream-pace":
            # ?ms=N&chunks=K, both required and positive; /admin/reset turns it off.
            ms, chunks = 0, 0
            for kv in query.split("&") if query else []:
                k, _, v = kv.partition("=")
                if k == "ms":
                    ms = int(v) if v.isdigit() else 0
                elif k == "chunks":
                    chunks = int(v) if v.isdigit() else 0
            if not 0 < ms <= 60000 or not 0 < chunks <= 100000:
                self.send_response(400)
                self.end_headers()
                return
            _stream_pace_ms, _stream_chunks = ms, chunks
            self._reply_json({"stream_pace_ms": ms, "stream_chunks": chunks})
            print(f"[{SERVER_ROLE}] [admin] stream pace = {chunks} chunks every {ms} ms",
                  flush=True)
        elif path == "/admin/stream-end":
            # ?mode=hold&sec=N (1..600) or ?mode=cut, each with an optional &at=mid
            # (the finish chunk is never sent); /admin/reset restores [DONE].
            mode, sec, at = "", 0, ""
            for kv in query.split("&") if query else []:
                k, _, v = kv.partition("=")
                if k == "mode":
                    mode = v
                elif k == "sec":
                    sec = int(v) if v.isdigit() else 0
                elif k == "at":
                    at = v
            if not (mode == "cut" or (mode == "hold" and 0 < sec <= 600)) or at not in ("", "mid"):
                self.send_response(400)
                self.end_headers()
                return
            _stream_end_mode, _stream_end_hold = mode, (sec if mode == "hold" else 0)
            _stream_end_at = at
            self._reply_json({"stream_end_mode": _stream_end_mode,
                              "stream_end_hold": _stream_end_hold,
                              "stream_end_at": _stream_end_at})
            print(f"[{SERVER_ROLE}] [admin] stream end = {_stream_end_mode} "
                  f"{_stream_end_hold} s {_stream_end_at or 'after the finish chunk'}",
                  flush=True)
        elif path == "/admin/answer-delay":
            # ?ms=N, 1..60000; /admin/reset turns it off.
            ms = 0
            for kv in query.split("&") if query else []:
                k, _, v = kv.partition("=")
                if k == "ms":
                    ms = int(v) if v.isdigit() else 0
            if not 0 < ms <= 60000:
                self.send_response(400)
                self.end_headers()
                return
            _answer_delay_ms = ms
            self._reply_json({"answer_delay_ms": ms})
            print(f"[{SERVER_ROLE}] [admin] answer delay = {ms} ms", flush=True)
        elif path == "/admin/drop-next":
            _drop_next = True
            self._reply_json({"drop_next": True})
            print(f"[{SERVER_ROLE}] [admin] drop-next ARMED", flush=True)
        elif path in ("/admin/stream-chunked-on", "/admin/stream-chunked-off"):
            _stream_chunked = path.endswith("-on")
            self._reply_json({"stream_chunked": _stream_chunked})
            print(f"[{SERVER_ROLE}] [admin] stream chunked = {_stream_chunked}", flush=True)
        elif path in ("/admin/keepalive-on", "/admin/keepalive-off"):
            _keepalive = path.endswith("-on")
            self._reply_json({"keepalive": _keepalive})
            print(f"[{SERVER_ROLE}] [admin] keepalive = {_keepalive}", flush=True)
        elif self.path in ("/admin/kv-params-on", "/admin/kv-params-off",
                           "/admin/kv-params-oversize"):
            # Shape of kv_transfer_params in the PREFILL response. The print is the
            # arm's receipt that the knob actually moved: a control that silently
            # failed to switch would otherwise be scored as a product result.
            _kv_params_mode = self.path.rsplit("-", 1)[1]
            self._reply_json({"kv_params_mode": _kv_params_mode})
            print(f"[{SERVER_ROLE}] [admin] kv-params mode = {_kv_params_mode}", flush=True)
        elif self.path in ("/admin/metrics-mode-absent", "/admin/metrics-mode-vllm",
                           "/admin/metrics-mode-garbage", "/admin/metrics-mode-sglang"):
            # What GET /metrics answers, and so which scrape OUTCOME the gateway
            # records for this endpoint. The print is the arm's receipt that the
            # knob actually moved: a control that silently failed to switch would
            # otherwise be scored as a product result.
            _metrics_mode = self.path.rsplit("-", 1)[1]
            self._reply_json({"metrics_mode": _metrics_mode})
            print(f"[{SERVER_ROLE}] [admin] metrics mode = {_metrics_mode}", flush=True)
        elif path.startswith("/admin/metrics-kv-usage/"):
            # This mock's KV-cache usage in percent, 0..100 (a float is accepted so a
            # case can place a value just below or above a threshold). Clears "omit".
            # 400 on anything else, so a typo cannot pass as a 0 % reading.
            raw = path[len("/admin/metrics-kv-usage/"):]
            try:
                pct = float(raw)
            except ValueError:
                pct = -1.0
            if not 0.0 <= pct <= 100.0:
                self.send_response(400)
                self.end_headers()
                return
            _kv_usage_pct = pct
            _kv_usage_omit = False
            self._reply_json({"kv_usage_pct": _kv_usage_pct, "kv_usage_omit": False,
                              "metrics_mode": _metrics_mode})
            print(f"[{SERVER_ROLE}] [admin] metrics kv usage = {_kv_usage_pct} %", flush=True)
        elif path == "/admin/metrics-kv-usage-omit":
            # Keep queue depth and cache config, drop the usage family: the gateway
            # must store "unknown" for this EP, not 0 %.
            _kv_usage_omit = True
            self._reply_json({"kv_usage_pct": _kv_usage_pct, "kv_usage_omit": True,
                              "metrics_mode": _metrics_mode})
            print(f"[{SERVER_ROLE}] [admin] metrics kv usage OMITTED", flush=True)
        elif self.path == "/admin/reset":
            _kv_usage_pct = None
            _kv_usage_omit = False
            _fail_next = False
            _fail_next_status = 500
            _fail_next_skip = 0
            _keepalive = False
            _kv_params_mode = "on"
            _metrics_mode = "absent"
            _stream_pace_ms, _stream_chunks = 0, 0
            _stream_chunked = False
            _stream_end_mode, _stream_end_hold, _stream_end_at = "", 0, ""
            _answer_delay_ms = 0
            _drop_next = False
            self._reply_json({"fail_next": False, "fail_next_status": _fail_next_status,
                              "keepalive": _keepalive, "kv_params_mode": _kv_params_mode,
                              "metrics_mode": _metrics_mode, "stream_pace_ms": 0,
                              "stream_chunks": 0, "stream_chunked": False,
                              "stream_end_mode": "", "stream_end_hold": 0,
                              "stream_end_at": "", "answer_delay_ms": 0,
                              "drop_next": False, "kv_usage_pct": None,
                              "kv_usage_omit": False})
            print(f"[{SERVER_ROLE}] [admin] knobs RESET", flush=True)
        elif self.path == "/admin/health-fail":
            _health_ok = False
            data = json.dumps({"health_ok": False}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            print(f"[{SERVER_ROLE}] [admin] health set to FAIL", flush=True)
        elif self.path == "/admin/health-ok":
            _health_ok = True
            data = json.dumps({"health_ok": True}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            print(f"[{SERVER_ROLE}] [admin] health set to OK", flush=True)
        else:
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if not self._auth_ok():
            self.send_response(401)
            self.end_headers()
            return
        if self.path == "/admin/status":
            data = json.dumps({
                "health_ok": _health_ok,
                "fail_next": _fail_next,
                "fail_next_status": _fail_next_status,
                "fail_next_skip": _fail_next_skip,
                "keepalive": _keepalive,
                "inference_requests": _inference_count,
                "kv_params_mode": _kv_params_mode,
                "metrics_mode": _metrics_mode,
                "stream_pace_ms": _stream_pace_ms,
                "stream_chunks": _stream_chunks,
                "stream_chunked": _stream_chunked,
                "stream_end_mode": _stream_end_mode,
                "stream_end_hold": _stream_end_hold,
                "stream_end_at": _stream_end_at,
                "answer_delay_ms": _answer_delay_ms,
                "kv_usage_pct": _kv_usage_pct,
                "kv_usage_omit": _kv_usage_omit,
                "role": SERVER_ROLE,
                "request_count": _request_count,
            }).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, format, *args):
        pass  # suppress admin server access logs


def _start_admin_server():
    """Start admin HTTP server on 127.0.0.1:9000 (loopback only).

    Daemon thread — exits when main thread exits.
    SECURITY: 127.0.0.1 binding ensures admin API is not reachable from the
    host network — only via 'docker exec' inside the container.
    """
    admin = HTTPServer(("127.0.0.1", 9000), AdminHandler)
    admin.serve_forever()


def _start_nixl_listener(port, role):
    """Start a mock NIXL side-channel TCP listener (simulates VLLM_NIXL_SIDE_CHANNEL_PORT).

    Real vLLM listens on this port for ZMQ/UCX KV block transfer coordination.
    The mock simply accepts TCP connections and logs them so CICD T9 can verify
    the port is reachable and loxilb is embedding the correct address in X-Request-Id.
    """
    try:
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        sock.bind(("0.0.0.0", port))
        sock.listen(16)
        print(f"[{role}] NIXL side-channel listening on port {port} (VLLM_NIXL_SIDE_CHANNEL_PORT)", flush=True)
        while True:
            try:
                conn, addr = sock.accept()
                print(f"[{role}] NIXL side-channel connection from {addr[0]}:{addr[1]}", flush=True)
                conn.close()
            except OSError:
                break
        sock.close()
    except Exception as exc:
        print(f"[{role}] NIXL side-channel listener failed on port {port}: {exc}", flush=True)


def main():
    global SERVER_ROLE, NIXL_PORT, EP_IDX, _args

    parser = argparse.ArgumentParser(description="Mock vLLM server for P/D testing")
    parser.add_argument("--role", choices=["prefill", "decode", "normal"],
                        default="normal", help="Server role (prefill/decode/normal)")
    parser.add_argument("--port", type=int, default=8000, help="Listen port")
    parser.add_argument("--host", default="0.0.0.0", help="Bind address")
    parser.add_argument("--nixl-port", type=int, default=0,
                        help="NIXL side-channel TCP port (VLLM_NIXL_SIDE_CHANNEL_PORT); 0=disabled")
    parser.add_argument("--fail-every", type=int, default=0,
                        help="Return HTTP 500 on every Nth request (0=disabled). "
                             "NOTE: does NOT trigger circuit breaker (CB requires TCP failure).")
    parser.add_argument("--ep-idx", type=int, default=0,
                        help="EP index (1..N); echoed as X-Prefill-Ep / X-Decode-Ep response "
                             "header so Phase L CICD harness can correlate the (prefill, decode) "
                             "pair across failover for restore_rate measurement.")
    args = parser.parse_args()

    SERVER_ROLE = args.role
    NIXL_PORT = args.nixl_port
    EP_IDX = args.ep_idx
    _args = args

    # Start admin HTTP server on loopback (127.0.0.1:9000) as daemon thread.
    # Provides /admin/health-fail, /admin/health-ok, /admin/fail-next[?status=N&skip=K],
    # /admin/keepalive-on|off, /admin/stream-pace?ms=N&chunks=K,
    # /admin/stream-chunked-on|off, /admin/stream-end?mode=hold&sec=N|cut,
    # /admin/answer-delay?ms=N, /admin/drop-next, /admin/reset,
    # /admin/status endpoints.
    admin_thread = threading.Thread(target=_start_admin_server, daemon=True)
    admin_thread.start()

    # Start NIXL side-channel listener if configured (simulates vLLM --kv-connector-port).
    # This allows T9 in validation.sh to verify loxilb embeds the NIXL port, not HTTP port,
    # in the X-Request-Id ___prefill_addr_IP:NIXL_PORT___ / ___decode_addr_IP:NIXL_PORT___ format.
    if args.nixl_port:
        nixl_thread = threading.Thread(
            target=_start_nixl_listener,
            args=(args.nixl_port, SERVER_ROLE),
            daemon=True,
        )
        nixl_thread.start()
    # Threaded for the paced stream only: _serial keeps every other request
    # one at a time.
    server = ThreadingHTTPServer((args.host, args.port), MockVLLMHandler)
    server.daemon_threads = True
    print(f"Mock vLLM server starting: role={SERVER_ROLE} addr={args.host}:{args.port} nixl_port={args.nixl_port or 'disabled'} admin=127.0.0.1:9000 fail_every={args.fail_every or 'disabled'} ep_idx={EP_IDX}")
    print(f"Model: {MODEL_NAME}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    server.server_close()


if __name__ == "__main__":
    main()
