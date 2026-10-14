#!/usr/bin/env python3
"""Send one request, wait for the first bytes of its streamed answer, then
send more bytes on the same connection while the stream is running. The
witness document the backend puts in the stream is written to <out>.

usage: stream_send.py <vip> <port> <out> <request file> <file sent mid-stream>...
prints "<status> <1 when a witness was found, else 0>"
"""
import json
import socket
import sys
import time

s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=10)
s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
s.sendall(open(sys.argv[4], "rb").read())
buf = b""
try:
    while b"data:" not in buf:
        d = s.recv(65536)
        if not d:
            break
        buf += d
    if b"data:" in buf:
        for f in sys.argv[5:]:
            time.sleep(0.3)
            s.sendall(open(f, "rb").read())
    while True:
        d = s.recv(65536)
        if not d:
            break
        buf += d
except (socket.timeout, OSError):
    pass
s.close()
status = buf.split(b" ")[1].decode() if buf.startswith(b"HTTP/") else "000"
found = 0
for ln in buf.split(b"\n"):
    if ln.startswith(b'data: {"witness"'):
        json.dump(json.loads(ln[6:])["witness"], open(sys.argv[3], "w"))
        found = 1
open(sys.argv[3] + ".raw", "wb").write(buf)
print(status, found)
