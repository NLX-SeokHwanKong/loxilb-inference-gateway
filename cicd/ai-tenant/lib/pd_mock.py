#!/usr/bin/env python3
"""pd_mock.py — a prefill/decode backend pair for cicd/ai-tenant that can HOLD.

cicd/vllm-pd-disagg/mock_vllm.py answers at once from a single-threaded
server, so a P/D request can never be held executing, and a case can never
fill a P/D pool's ceiling to make a request wait. This mock speaks the same
wire shape (the prefill leg answers a non-streaming chat completion carrying
kv_transfer_params, the decode leg answers the completion) from a threaded
server, and holds the DECODE leg of a request until it is released.

A request is named by its nonce: the X-Test-Nonce header when the leg
carries it, else the first user message's content (the ai-tenant driver puts
the nonce there), so the name survives a gateway that rewrites headers.

Endpoints (both roles):
  POST /v1/chat/completions   the leg (prefill: at once; decode: held)
  GET  /health                readiness
  GET  /__receipts/<nonce>    {"count": legs of this role that arrived}
  GET  /__release/<nonce>     let the held decode leg(s) of that nonce answer
  GET  /__release_all         let every held leg answer
  GET  /__inflight            {"inflight": legs executing right now}

Usage: pd_mock.py --role prefill|decode --port P [--hold-max-s 90]
"""
import argparse
import json
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, HTTPServer
from socketserver import ThreadingMixIn

COND = threading.Condition()
RECEIPTS = {}
RELEASED = set()
RELEASE_GEN = [0]
INFLIGHT = [0]
ROLE = "prefill"
HOLD_MAX_S = 90.0


class Server(ThreadingMixIn, HTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def _json(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        p = self.path
        if p == "/health":
            return self._json(200, {"status": "ok", "role": ROLE})
        if p.startswith("/__receipts/"):
            with COND:
                n = RECEIPTS.get(p[len("/__receipts/"):], 0)
            return self._json(200, {"count": n})
        if p.startswith("/__release/"):
            with COND:
                RELEASED.add(p[len("/__release/"):])
                COND.notify_all()
            return self._json(200, {"released": True})
        if p == "/__release_all":
            with COND:
                RELEASE_GEN[0] += 1
                COND.notify_all()
            return self._json(200, {"released": "all"})
        if p == "/__inflight":
            with COND:
                return self._json(200, {"inflight": INFLIGHT[0]})
        return self._json(404, {"error": "not found"})

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw.decode() or "{}")
        except ValueError:
            return self._json(400, {"error": "invalid json"})
        nonce = self.headers.get("X-Test-Nonce", "")
        if not nonce:
            try:
                nonce = str(req["messages"][0]["content"])
            except (KeyError, IndexError, TypeError):
                nonce = ""
        with COND:
            RECEIPTS[nonce] = RECEIPTS.get(nonce, 0) + 1
            INFLIGHT[0] += 1
        try:
            model = req.get("model", "band-model")
            if ROLE == "prefill":
                return self._json(200, {
                    "id": "chatcmpl-" + uuid.uuid4().hex[:12], "object": "chat.completion",
                    "created": int(time.time()), "model": model,
                    "choices": [{"index": 0, "message": {"role": "assistant", "content": ""},
                                 "finish_reason": "length"}],
                    "usage": {"prompt_tokens": 12, "completion_tokens": 1, "total_tokens": 13},
                    "kv_transfer_params": {
                        "do_remote_prefill": True, "do_remote_decode": False,
                        "remote_block_ids": [[0, 1, 2, 3]], "remote_host": "127.0.0.1",
                        "remote_port": 9001, "remote_engine_id": "prefill-" + uuid.uuid4().hex[:8],
                        "remote_request_id": str(uuid.uuid4()), "tp_size": 1}})
            deadline = time.monotonic() + HOLD_MAX_S
            with COND:
                gen = RELEASE_GEN[0]
                while nonce not in RELEASED and RELEASE_GEN[0] == gen:
                    left = deadline - time.monotonic()
                    if left <= 0:
                        break
                    COND.wait(left)
            return self._json(200, {
                "id": "chatcmpl-" + uuid.uuid4().hex[:12], "object": "chat.completion",
                "created": int(time.time()), "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "decoded " + nonce},
                             "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16}})
        finally:
            with COND:
                INFLIGHT[0] -= 1


def main():
    global ROLE, HOLD_MAX_S
    ap = argparse.ArgumentParser()
    ap.add_argument("--role", choices=("prefill", "decode"), required=True)
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--hold-max-s", type=float, default=90.0)
    a = ap.parse_args()
    ROLE, HOLD_MAX_S = a.role, a.hold_max_s
    Server(("0.0.0.0", a.port), Handler).serve_forever()


if __name__ == "__main__":
    main()
