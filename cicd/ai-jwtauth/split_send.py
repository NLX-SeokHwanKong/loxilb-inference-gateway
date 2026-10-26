#!/usr/bin/env python3
"""Send a request as separate writes with a pause between them, so each
write reaches the gateway as a read of its own. Arguments after the pause
are files of raw bytes, sent in order; the word READ reads one response
(Content-Length framed) and writes its body to <out>.<n> and its header
block to <out>.<n>.head.

usage: split_send.py <vip> <port> <out> <pause-ms> <file|READ>...
prints one status per READ, space separated ("000" for no answer)
"""
import socket
import sys
import time

vip, port, out, pause = sys.argv[1], int(sys.argv[2]), sys.argv[3], int(sys.argv[4]) / 1000.0
s = socket.create_connection((vip, port), timeout=10)
s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
n, codes = 0, []


def read_one():
    buf = b""
    try:
        while b"\r\n\r\n" not in buf:
            d = s.recv(65536)
            if not d:
                return "000", b"", b""
            buf += d
        head, _, body = buf.partition(b"\r\n\r\n")
        want = 0
        for ln in head.split(b"\r\n")[1:]:
            k, _, v = ln.partition(b":")
            if k.strip().lower() == b"content-length":
                want = int(v)
        while len(body) < want:
            d = s.recv(65536)
            if not d:
                break
            body += d
        return head.split(b" ")[1].decode(), body, head
    except (socket.timeout, OSError):
        return "000", b"", b""


for a in sys.argv[5:]:
    if a == "READ":
        n += 1
        code, body, head = read_one()
        open("%s.%d" % (out, n), "wb").write(body)
        open("%s.%d.head" % (out, n), "wb").write(head)
        codes.append(code)
    else:
        s.sendall(open(a, "rb").read())
        time.sleep(pause)
s.close()
print(" ".join(codes))
