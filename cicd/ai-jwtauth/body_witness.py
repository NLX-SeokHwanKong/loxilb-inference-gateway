#!/usr/bin/env python3
"""Backend that is the witness of the request BODY: it answers every request
with a JSON document carrying the headers it received (names lowercased),
the body bytes exactly as they arrived (base64), any bytes that arrived
after the declared body ("extra"), and whether the body came up short.
A chunked body is returned with its framing, as read off the socket.

A request that carries "X-Witness-Stream: <seconds>" is answered as a
server-sent-event stream of that length; whatever the connection delivers
while the stream runs is collected, and the last event before [DONE] is the
same witness document with those bytes as "extra". The same document is
also appended, one JSON line a streamed request, to the file given as the
fourth argument, keyed by the request's X-Witness-Id: a stream the gateway
cut cannot carry its own witness to the client.

A request that carries "X-Witness-Reply: canary" is answered with a text
body of lines that look like request headers around a blank CRLF line, the
header block and the body in two writes, and a response header X-Auth-Echo.

usage: body_witness.py <bind-ip> <port> <label> [stream witness file]
"""
import base64
import json
import socket
import sys
import threading
import time

BIND, PORT, LABEL = sys.argv[1], int(sys.argv[2]), sys.argv[3]
WITNESS = sys.argv[4] if len(sys.argv) > 4 else None
CANARY = b"line one\r\nX-Auth-Canary: 1\r\nX-Api-Key: k-in-the-answer\r\n\r\nline after the blank line\r\n"
WLOCK = threading.Lock()


def recv_some(c, timeout):
    c.settimeout(timeout)
    try:
        return c.recv(65536)
    except (socket.timeout, OSError):
        return None


def serve(c):
    buf = b""
    try:
        while True:
            while b"\r\n\r\n" not in buf:
                d = recv_some(c, 30)
                if not d:
                    return
                buf += d
            head, _, rest = buf.partition(b"\r\n\r\n")
            lines = head.split(b"\r\n")
            hdrs = {}
            for ln in lines[1:]:
                k, _, v = ln.partition(b":")
                hdrs.setdefault(k.decode("latin1").strip().lower(), []).append(v.decode("latin1").strip())
            short = False
            if "chunked" in ",".join(hdrs.get("transfer-encoding", [])).lower():
                body = rest
                while True:  # the framing as it arrives, until the line goes quiet
                    d = recv_some(c, 1.5)
                    if not d:
                        break
                    body += d
                extra = b""
            else:
                n = int((hdrs.get("content-length") or ["0"])[0])
                body = rest
                while len(body) < n:
                    d = recv_some(c, 3)
                    if not d:
                        short = True
                        break
                    body += d
                body, extra = body[:n], body[n:]
                d = recv_some(c, 0.5)
                while d:
                    extra += d
                    d = recv_some(c, 0.5)
            secs = float((hdrs.get("x-witness-stream") or ["0"])[0])
            if secs > 0:
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n"
                          b"Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n")

                def ev(obj):
                    d = b"data: " + (obj if isinstance(obj, bytes) else json.dumps(obj).encode()) + b"\n\n"
                    c.sendall(b"%x\r\n" % len(d) + d + b"\r\n")
                end = time.time() + secs
                cut = False

                def witness():
                    return {"label": LABEL, "id": (hdrs.get("x-witness-id") or ["-"])[0], "headers": hdrs,
                            "body_b64": base64.b64encode(body).decode(),
                            "extra_b64": base64.b64encode(extra).decode(), "short": short, "cut": cut}
                try:
                    while time.time() < end:
                        ev({"choices": [{"delta": {"content": "x"}}]})
                        c.settimeout(0.2)
                        try:
                            d = c.recv(65536)
                            if not d:
                                cut = True
                                break
                            extra += d
                        except socket.timeout:
                            pass
                    if not cut:
                        ev({"witness": witness()})
                        ev({"choices": [], "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8}})
                        ev(b"[DONE]")
                        c.sendall(b"0\r\n\r\n")
                except OSError:
                    cut = True
                finally:
                    if WITNESS:
                        with WLOCK:
                            open(WITNESS, "a").write(json.dumps(witness()) + "\n")
                return
            if (hdrs.get("x-witness-reply") or [""])[0] == "canary":
                c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nX-Auth-Echo: from-backend\r\n"
                          b"Content-Length: %d\r\nConnection: close\r\n\r\n" % len(CANARY))
                time.sleep(0.3)
                c.sendall(CANARY)
                return
            raw = json.dumps({
                "label": LABEL, "request_line": lines[0].decode("latin1"), "headers": hdrs,
                "body_b64": base64.b64encode(body).decode(), "extra_b64": base64.b64encode(extra).decode(),
                "short": short,
                "choices": [{"message": {"role": "assistant", "content": "ok"}}],
                "usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
            }).encode()
            close = short or extra
            c.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n%s\r\n"
                      % (len(raw), b"Connection: close\r\n" if close else b"") + raw)
            if close:
                return
            buf = b""
    finally:
        c.close()


s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((BIND, PORT))
s.listen(64)
while True:
    conn, _ = s.accept()
    threading.Thread(target=serve, args=(conn,), daemon=True).start()
