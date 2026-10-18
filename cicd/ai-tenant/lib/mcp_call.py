#!/usr/bin/env python3
"""mcp_call.py — one stateless-MCP tools/call (echo) through the gateway.

The body and headers come from cicd/mcp-stateless/client.py (the scenario's
own wire client), so the gateway and the fixture backend see exactly what
that scenario sends. A progressToken makes the fixture answer as an event
stream with two progress notifications --progress-delay apart, which holds
the request executing for about twice that delay.

Prints one JSON line: {"id", "status", "error", "ms"}.

Usage: mcp_call.py <url> --id ID [--api-key K] [--progress]
"""
import argparse
import http.client
import json
import os
import sys
import time
from urllib.parse import urlsplit

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "mcp-stateless"))
from client import headers_for, request_body  # noqa: E402


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("url")
    ap.add_argument("--id", required=True)
    ap.add_argument("--api-key", default="")
    ap.add_argument("--progress", action="store_true")
    ap.add_argument("--timeout", type=float, default=60)
    a = ap.parse_args()
    body = request_body("tools/call", a.id, name="echo", arguments={"message": a.id})
    if a.progress:
        body["params"]["_meta"]["progressToken"] = "pt-" + a.id
    hdr = headers_for(body)
    if a.api_key:
        hdr["X-Api-Key"] = a.api_key
    u = urlsplit(a.url)
    t0 = time.monotonic()
    status, err = 0, ""
    try:
        c = http.client.HTTPConnection(u.hostname, u.port, timeout=a.timeout)
        c.request("POST", u.path or "/mcp", json.dumps(body), hdr)
        r = c.getresponse()
        status = r.status
        data = r.read().decode("utf8", "replace")
        if status != 200:
            try:
                err = json.loads(data).get("error", "")
                if isinstance(err, dict):
                    err = err.get("message", "")
            except ValueError:
                err = data[:80]
    except OSError as e:
        err = "transport:" + e.__class__.__name__
    print(json.dumps({"id": a.id, "status": status, "error": err,
                      "ms": int((time.monotonic() - t0) * 1000)}), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
