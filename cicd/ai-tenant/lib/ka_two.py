#!/usr/bin/env python3
"""Two HTTP/1.1 requests on ONE keep-alive connection (X16).

  ka_two.py <host> <port> <api-key> <model> <nonce1> <nonce2> <gate-file> <out-prefix>

The first request carries X-Test-Hold: 1 (the mock holds it if it ever gets
there). Its response is read in full by its Content-Length. The second request
is sent on the same socket once <gate-file> exists (the case creates it when
the pool is empty), without a hold. curl would reconnect quietly when the
gateway closes; this client never does, so a closed connection shows as
"closed" instead of a second 200.

Writes <out-prefix>-1.{code,hdr,body} and <out-prefix>-2.{code,hdr,body}; a
.code line is "<status> <seconds>", or "closed <seconds>" / "error:<what>
<seconds>" when no response came on the connection.
"""
import os
import socket
import sys
import time


def request(model, nonce, key, host, hold):
    body = ('{"model":"%s","messages":[{"role":"user","content":"%s"}],'
            '"max_tokens":4}' % (model, nonce)).encode()
    head = ("POST /v1/chat/completions HTTP/1.1\r\n"
            "Host: %s\r\n"
            "Content-Type: application/json\r\n"
            "X-Api-Key: %s\r\n"
            "X-Test-Nonce: %s\r\n"
            "%s"
            "Content-Length: %d\r\n"
            "\r\n" % (host, key, nonce, "X-Test-Hold: 1\r\n" if hold else "",
                      len(body)))
    return head.encode() + body


def read_response(sock, buf):
    """Read one response; returns (status, headers-text, body, rest)."""
    while b"\r\n\r\n" not in buf:
        chunk = sock.recv(65536)
        if not chunk:
            raise EOFError
        buf += chunk
    head, rest = buf.split(b"\r\n\r\n", 1)
    lines = head.decode("latin-1").split("\r\n")
    status = lines[0].split(" ")[1]
    clen = None
    chunked = False
    for ln in lines[1:]:
        k, _, v = ln.partition(":")
        if k.strip().lower() == "content-length":
            clen = int(v.strip())
        if k.strip().lower() == "transfer-encoding" and "chunked" in v.lower():
            chunked = True
    if chunked:
        while b"\r\n0\r\n\r\n" not in rest and not rest.startswith(b"0\r\n\r\n"):
            chunk = sock.recv(65536)
            if not chunk:
                raise EOFError
            rest += chunk
        end = rest.find(b"0\r\n\r\n") + 5
        return status, head.decode("latin-1"), rest[:end], rest[end:]
    if clen is None:
        raise ValueError("no Content-Length")
    while len(rest) < clen:
        chunk = sock.recv(65536)
        if not chunk:
            raise EOFError
        rest += chunk
    return status, head.decode("latin-1"), rest[:clen], rest[clen:]


def write(prefix, i, code, hdr, body, t):
    with open("%s-%d.code" % (prefix, i), "w") as f:
        f.write("%s %.3f\n" % (code, t))
    with open("%s-%d.hdr" % (prefix, i), "w") as f:
        f.write(hdr)
    with open("%s-%d.body" % (prefix, i), "wb") as f:
        f.write(body)


def main():
    host, port, key, model, n1, n2, gate, prefix = sys.argv[1:9]
    sock = socket.create_connection((host, int(port)), timeout=60)
    buf = b""
    t0 = time.time()
    sock.sendall(request(model, n1, key, host, True))
    try:
        st, hdr, body, buf = read_response(sock, buf)
        write(prefix, 1, st, hdr, body, time.time() - t0)
    except EOFError:
        write(prefix, 1, "closed", "", b"", time.time() - t0)
        return
    except Exception as e:  # noqa: BLE001 - recorded as the case's evidence
        write(prefix, 1, "error:%s" % type(e).__name__, "", b"", time.time() - t0)
        return
    for _ in range(240):
        if os.path.exists(gate):
            break
        time.sleep(0.25)
    t0 = time.time()
    try:
        sock.sendall(request(model, n2, key, host, False))
        st, hdr, body, buf = read_response(sock, buf)
        write(prefix, 2, st, hdr, body, time.time() - t0)
    except (EOFError, BrokenPipeError, ConnectionResetError):
        write(prefix, 2, "closed", "", b"", time.time() - t0)
    except Exception as e:  # noqa: BLE001
        write(prefix, 2, "error:%s" % type(e).__name__, "", b"", time.time() - t0)
    sock.close()


if __name__ == "__main__":
    main()
