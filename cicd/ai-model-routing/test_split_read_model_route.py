#!/usr/bin/env python3
"""Live repro: a request whose head and body reach the gateway as separate
reads must still be model-routed (ROUTE-01).

llhttp reports the Host callbacks only while parsing the header block. When
the body arrived in a later read, the per-read header state was reset and not
restored, so selection ran with host_str=NULL and took the first pool without
any model check: an unknown model reached a backend (vLLM 404) instead of the
gateway's 503 model_unavailable, and on a multi-model VIP a request went to
the wrong pool.

The script opens a TCP connection to a model-routed service, sends the
request head, sleeps, then sends a JSON body naming a model no pool serves.
It asserts, for every attempt:
  - the answer is 503 with "model_unavailable" (never a backend 404);
  - the gateway log over the run has no "[CATALOG_DFLMODE] ... host_str=(null)"
    and has "[MODEL_ROUTING] 503 sent for model='<model>'".

[CATALOG_DFLMODE] is logged only by a HAVE_HTTP_TRACE + HAVE_PROXY_EXTRA_DEBUG
build at debug level; when the window holds none, the log half is reported
inconclusive (exit 2) rather than passed. --http-only asserts the answers
alone, for a service where a bypassed request reaches a backend that answers
something other than 503 (validation.sh T4b: port 2020 has no wildcard pool,
so the bypass would reach server-llama).

usage: test_split_read_model_route.py <vip> <port>
         [--model NAME] [--path P] [--pause-ms N] [--count N]
         [--container NAME | --log-file PATH | --http-only]
exit 0 pass, 1 fail, 2 inconclusive (no log source, or no debug lines)
"""
import argparse
import datetime
import json
import os
import socket
import subprocess
import sys
import time


def one_request(vip, port, path, model, pause):
    body = json.dumps({"model": model, "max_tokens": 8, "stream": False,
                       "messages": [{"role": "user", "content": "hi"}]}).encode()
    head = ("POST %s HTTP/1.1\r\n"
            "Host: %s:%d\r\n"
            "Content-Type: application/json\r\n"
            "Accept: application/json\r\n"
            "Content-Length: %d\r\n"
            "\r\n" % (path, vip, port, len(body))).encode()
    try:
        s = socket.create_connection((vip, port), timeout=10)
    except OSError as e:
        return ("connect failed: %s" % e).encode()
    s.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    try:
        s.sendall(head)
        time.sleep(pause)
        s.sendall(body)
        buf = b""
        # The 503 is Connection: close without a length; a backend answer may
        # be either, so read the head, then to Content-Length or to close.
        while True:
            try:
                d = s.recv(65536)
            except socket.timeout:
                break
            if not d:
                break
            buf += d
            h, sep, b = buf.partition(b"\r\n\r\n")
            if sep:
                for ln in h.split(b"\r\n")[1:]:
                    k, _, v = ln.partition(b":")
                    if k.strip().lower() == b"content-length" and len(b) >= int(v):
                        return buf
    finally:
        s.close()
    return buf


def read_log(args, since, offset):
    if args.container:
        r = subprocess.run(["docker", "logs", "--since", since, args.container],
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        return r.stdout.decode(errors="replace")
    if args.log_file:
        with open(args.log_file, "rb") as f:
            f.seek(offset)
            return f.read().decode(errors="replace")
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("vip")
    ap.add_argument("port", type=int)
    ap.add_argument("--model", default="route01-unknown-model")
    ap.add_argument("--path", default="/v1/chat/completions")
    ap.add_argument("--pause-ms", type=int, default=50)
    ap.add_argument("--count", type=int, default=3)
    src = ap.add_mutually_exclusive_group()
    src.add_argument("--container", help="gateway container; read with docker logs")
    src.add_argument("--log-file", help="gateway log file; read what is appended")
    src.add_argument("--http-only", action="store_true",
                     help="assert the answers only; no log is read")
    args = ap.parse_args()

    since = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    offset = os.path.getsize(args.log_file) if args.log_file else 0
    ok = True

    for i in range(1, args.count + 1):
        resp = one_request(args.vip, args.port, args.path, args.model,
                           args.pause_ms / 1000.0)
        line = resp.split(b"\r\n", 1)[0].decode(errors="replace")
        parts = line.split(" ")
        status = parts[1] if line.startswith("HTTP/") and len(parts) > 1 else "000"
        good = status == "503" and b"model_unavailable" in resp
        print("attempt %d: %s %s" % (i, status, "OK" if good else "FAILED"))
        if not good:
            ok = False
            print("  response: %r" % resp[:400])
        time.sleep(0.2)

    log = None
    if args.container or args.log_file:
        time.sleep(0.5)  # let the gateway flush its log
        log = read_log(args, since, offset)
    if log is None:
        if args.http_only:
            print("PASS (answers only)" if ok else "FAIL")
            return 0 if ok else 1
        print("log: no --container/--log-file given, log assertions skipped")
        return 2 if ok else 1

    dfl = [l for l in log.splitlines() if "[CATALOG_DFLMODE]" in l]
    nulls = [l for l in dfl if "host_str=(null)" in l]
    routed = [l for l in log.splitlines()
              if "[MODEL_ROUTING] 503 sent for model='%s'" % args.model in l]
    print("log: %d CATALOG_DFLMODE, %d with host_str=(null), %d MODEL_ROUTING 503"
          % (len(dfl), len(nulls), len(routed)))
    for l in nulls:
        print("  " + l)
    if nulls:
        ok = False
    if len(routed) < args.count:
        print("log: expected %d MODEL_ROUTING 503 lines for model '%s'"
              % (args.count, args.model))
        ok = False
    if not ok:
        print("FAIL")
        return 1
    if not dfl:
        print("INCONCLUSIVE: no [CATALOG_DFLMODE] lines — the build or log level "
              "does not emit them, so host_str=(null) cannot be ruled out")
        return 2
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
