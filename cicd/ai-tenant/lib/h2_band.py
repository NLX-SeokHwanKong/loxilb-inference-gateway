#!/usr/bin/env python3
"""h2_band.py — HTTP/2 (h2c, prior knowledge) driver for cicd/ai-tenant.

One connection as one tenant (its API key), k streams opened back to back,
each held at the h2c backend for --delay-ms. Every stream prints one JSON
line with its :status, its x-loxilb-admission-* headers and the error code
of a refusal body; a final summary line carries the counts. With --rst N,
the first N streams still unanswered after --rst-after-s are cancelled with
RST_STREAM(CANCEL) while the connection stays up.

Usage: h2_band.py <host> <port> --api-key K --streams N [--delay-ms D]
                  [--nonce-prefix P] [--model M] [--rst N --rst-after-s S]
                  [--hdr "name: value" ...]
"""
import argparse
import json
import socket
import sys
import time

import h2.config
import h2.connection
import h2.errors
import h2.events


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("host")
    ap.add_argument("port", type=int)
    ap.add_argument("--api-key", default="")
    ap.add_argument("--streams", type=int, default=1)
    ap.add_argument("--delay-ms", type=int, default=0)
    ap.add_argument("--nonce-prefix", default="h2band")
    ap.add_argument("--model", default="band-model")
    ap.add_argument("--rst", type=int, default=0)
    ap.add_argument("--rst-after-s", type=float, default=0)
    ap.add_argument("--hdr", action="append", default=[], help="name: value, sent on every stream")
    ap.add_argument("--body-extra", default="{}",
                    help="a JSON object merged into every stream's body (top-level keys)")
    args = ap.parse_args()
    body_extra = json.loads(args.body_extra)

    timeout = args.delay_ms / 1000.0 + 15.0
    sock = socket.create_connection((args.host, args.port), timeout=timeout)
    conn = h2.connection.H2Connection(
        config=h2.config.H2Configuration(client_side=True, header_encoding=None))
    conn.initiate_connection()
    sock.sendall(conn.data_to_send())

    streams = {}
    for i in range(args.streams):
        nonce = "%s-%d" % (args.nonce_prefix, i)
        sid = conn.get_next_available_stream_id()
        body = dict({"model": args.model, "max_tokens": 4,
                     "messages": [{"role": "user", "content": nonce}]}, **body_extra)
        body = json.dumps(body).encode()
        headers = [
            (":method", "POST"), (":path", "/v1/chat/completions"), (":scheme", "http"),
            (":authority", "%s:%d" % (args.host, args.port)),
            ("content-type", "application/json"),
            ("content-length", str(len(body))),
            ("x-test-nonce", nonce),
        ]
        if args.api_key:
            headers.append(("x-api-key", args.api_key))
        for h in args.hdr:
            hname, _, hvalue = h.partition(":")
            headers.append((hname.strip().lower(), hvalue.strip()))
        if args.delay_ms:
            headers.append(("x-test-delay-ms", str(args.delay_ms)))
        conn.send_headers(sid, headers)
        conn.send_data(sid, body, end_stream=True)
        streams[sid] = {"nonce": nonce, "status": "", "headers": {},
                        "body": bytearray(), "done": False, "rst": False}
    sock.sendall(conn.data_to_send())

    start = time.monotonic()
    deadline = start + timeout
    rst_at = start + args.rst_after_s if args.rst else None
    rst_done = 0
    while time.monotonic() < deadline:
        if all(st["done"] for st in streams.values()):
            break
        if rst_at is not None and time.monotonic() >= rst_at:
            for sid in sorted(streams):
                st = streams[sid]
                if rst_done >= args.rst:
                    break
                if st["done"] or st["status"]:
                    continue
                conn.reset_stream(sid, error_code=h2.errors.ErrorCodes.CANCEL)
                st.update(rst=True, done=True, status="RST_SENT")
                rst_done += 1
            sock.sendall(conn.data_to_send())
            rst_at = None
        try:
            sock.settimeout(0.2)
            data = sock.recv(65535)
        except socket.timeout:
            continue
        except OSError:
            break
        if not data:
            break
        for event in conn.receive_data(data):
            st = streams.get(getattr(event, "stream_id", None))
            if isinstance(event, h2.events.ResponseReceived) and st is not None:
                for name, value in event.headers:
                    name = name.decode() if isinstance(name, bytes) else name
                    value = value.decode() if isinstance(value, bytes) else value
                    if name == ":status":
                        st["status"] = value
                    elif name.startswith(("x-loxilb-admission-", "x-llm-d-")) or name == "retry-after":
                        st["headers"][name] = value
            elif isinstance(event, h2.events.DataReceived) and st is not None:
                st["body"] += event.data
                try:
                    conn.acknowledge_received_data(len(event.data), event.stream_id)
                except Exception:  # noqa: BLE001
                    pass
            elif isinstance(event, h2.events.StreamEnded) and st is not None:
                st["done"] = True
            elif isinstance(event, h2.events.StreamReset) and st is not None:
                st["status"] = st["status"] or "RST"
                st["done"] = True
            elif isinstance(event, h2.events.ConnectionTerminated):
                deadline = 0
        out = conn.data_to_send()
        if out:
            sock.sendall(out)
    try:
        conn.close_connection()
        sock.sendall(conn.data_to_send())
        sock.close()
    except OSError:
        pass

    counts = {}
    for sid in sorted(streams):
        st = streams[sid]
        err = ""
        try:
            err = json.loads(bytes(st["body"]).decode("utf8", "replace") or "{}").get("error", "")
        except ValueError:
            pass
        status = st["status"] or "NONE"
        print(json.dumps({"stream": sid, "nonce": st["nonce"], "status": status,
                          "error": err, "headers": st["headers"]}), flush=True)
        key = status + (":" + err if err else "")
        counts[key] = counts.get(key, 0) + 1
    print(json.dumps({"summary": counts}), flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
