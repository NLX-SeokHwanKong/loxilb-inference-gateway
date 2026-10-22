#!/usr/bin/env python3
"""tls_backend.py — an HTTPS chat-completions backend for cicd/ai-tenant X5.

The behaviour of ../ai-admission/mock_backend.py that X5 needs, over TLS:
POST /v1/chat/completions is counted by its X-Test-Nonce and, with
X-Test-Hold: 1, held until released (or --hold-max-s). The certificate it
presents is the scenario's choice: one issued by the CA the rule trusts, or
one that is not. A request only exists here once the gateway's handshake
succeeded, so a receipt on the untrusted backend means the gateway sent a
request to a backend it should have refused.

The control paths are plain HTTP on a second port (--admin-port), so the
harness reads them without TLS:
  GET /__receipts/<nonce>   {"count": n}
  GET /__release/<nonce>    let held requests with that nonce answer
  GET /__release_all
  GET /__handshakes         {"ok": n, "failed": n} TLS handshakes seen

Usage: tls_backend.py --port P --admin-port A --cert C --key K [--hold-max-s 90]
"""
import argparse
import json
import ssl
import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer
from socketserver import ThreadingMixIn

COND = threading.Condition()
RECEIPTS = {}
RELEASED = set()
GEN = [0]
HS = {"ok": 0, "failed": 0}
HOLD_MAX_S = 90.0


class Threaded(ThreadingMixIn, HTTPServer):
    daemon_threads = True
    allow_reuse_address = True
    request_queue_size = 128


class TLSServer(Threaded):
    """Handshake per connection in the worker thread, counted."""

    ctx = None

    def get_request(self):
        sock, addr = self.socket.accept()
        return sock, addr

    def finish_request(self, request, client_address):
        try:
            tls = self.ctx.wrap_socket(request, server_side=True)
        except (ssl.SSLError, OSError):
            with COND:
                HS["failed"] += 1
            try:
                request.close()
            except OSError:
                pass
            return
        with COND:
            HS["ok"] += 1
        self.RequestHandlerClass(tls, client_address, self)


def reply(h, status, obj):
    data = json.dumps(obj).encode()
    h.send_response(status)
    h.send_header("Content-Type", "application/json")
    h.send_header("Content-Length", str(len(data)))
    h.end_headers()
    h.wfile.write(data)


class Inference(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or 0)
        if n:
            self.rfile.read(n)
        nonce = self.headers.get("X-Test-Nonce", "")
        with COND:
            RECEIPTS[nonce] = RECEIPTS.get(nonce, 0) + 1
        if self.headers.get("X-Test-Hold") == "1":
            deadline = time.monotonic() + HOLD_MAX_S
            with COND:
                gen = GEN[0]
                while nonce not in RELEASED and GEN[0] == gen:
                    left = deadline - time.monotonic()
                    if left <= 0:
                        break
                    COND.wait(left)
        reply(self, 200, {"id": "chatcmpl-tls", "object": "chat.completion", "model": "band-model",
                          "choices": [{"index": 0, "message": {"role": "assistant", "content": nonce},
                                       "finish_reason": "stop"}],
                          "usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}})


class Admin(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        p = self.path
        with COND:
            if p.startswith("/__receipts/"):
                return reply(self, 200, {"count": RECEIPTS.get(p[len("/__receipts/"):], 0)})
            if p.startswith("/__release/"):
                RELEASED.add(p[len("/__release/"):])
                COND.notify_all()
                return reply(self, 200, {"released": True})
            if p == "/__release_all":
                GEN[0] += 1
                COND.notify_all()
                return reply(self, 200, {"released": "all"})
            if p == "/__handshakes":
                return reply(self, 200, dict(HS))
        return reply(self, 404, {"error": "not found"})


def main():
    global HOLD_MAX_S
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, required=True)
    ap.add_argument("--admin-port", type=int, required=True)
    ap.add_argument("--cert", required=True)
    ap.add_argument("--key", required=True)
    ap.add_argument("--hold-max-s", type=float, default=90.0)
    a = ap.parse_args()
    HOLD_MAX_S = a.hold_max_s
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(a.cert, a.key)
    TLSServer.ctx = ctx
    srv = TLSServer(("0.0.0.0", a.port), Inference)
    threading.Thread(target=Threaded(("0.0.0.0", a.admin_port), Admin).serve_forever, daemon=True).start()
    srv.serve_forever()


if __name__ == "__main__":
    main()
