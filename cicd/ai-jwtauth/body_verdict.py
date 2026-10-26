#!/usr/bin/env python3
"""Compare what the backend received with what the client sent.

usage: body_verdict.py <backend answer (json)> <file of the body bytes sent> [file of bytes sent after the request]
prints: hdr=<count of x-auth-tenant headers>:<value> body=<same|DIFF> extra=<bytes> short=<0|1> inbody=<0|1>
with the third argument "extra=<bytes>" becomes "after=<none|same|DIFF>": what the backend received after the
request against what the client sent after it; a streamed request's record also
prints cut=<1 when the stream ended before the backend finished it>
"""
import base64
import json
import sys

try:
    a = json.load(open(sys.argv[1], "rb"))
except Exception as e:
    print("unreadable-answer (%s)" % type(e).__name__)
    sys.exit(0)
sent = open(sys.argv[2], "rb").read()
got = base64.b64decode(a["body_b64"])
extra = base64.b64decode(a["extra_b64"])
h = a["headers"].get("x-auth-tenant", [])
ex = "extra=%d" % len(extra)
if len(sys.argv) > 3:
    ex = "after=" + ("none" if not extra else "same" if extra == open(sys.argv[3], "rb").read() else "DIFF")
print("hdr=%d:%s body=%s %s short=%d inbody=%d%s" % (
    len(h), ",".join(h) or "-", "same" if got == sent else "DIFF", ex, int(a["short"]),
    int(b"x-auth-tenant" in (got + extra).lower()),
    " cut=%d" % int(a["cut"]) if "cut" in a else ""))
if got != sent or extra or a["short"]:
    sys.stderr.write("sent %d bytes, backend got %d (+%d extra)\n got:   %r\n extra: %r\n" % (
        len(sent), len(got), len(extra), got[:400], extra[:200]))
