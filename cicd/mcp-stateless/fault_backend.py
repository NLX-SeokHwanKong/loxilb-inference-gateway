#!/usr/bin/env python3
"""Intentionally malformed HTTP/1 backend for isolated bridge regression only."""
import argparse
import json
from pathlib import Path
import socket
import socketserver
import struct
import threading


class Handler(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(5)
        if not self.rfile.readline(8193).startswith(b'POST '):
            return
        headers = {}
        for _ in range(100):
            line = self.rfile.readline(8193)
            if line == b'\r\n':
                break
            if not line or len(line) > 8192:
                return
            key, value = line.decode().split(':', 1)
            headers[key.lower()] = value.strip()
        size = int(headers.get('content-length', '0'))
        if not 0 < size < 8192:
            return
        body = json.loads(self.rfile.read(size))
        case = body['params']['arguments']['message']
        row = {'event': 'fault-request', 'request_id': body['id'], 'case': case}
        with self.server.lock:
            with self.server.journal.open('a') as output:
                output.write(json.dumps(row) + '\n')
        prefix = b'HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\n'
        responses = {
            'truncated-content-length': prefix + b'Content-Length: 1000\r\n\r\n{"jsonrpc":"2.0"',
            'duplicate-content-length': prefix + b'Content-Length: 2\r\nContent-Length: 3\r\n\r\n{}',
            'ambiguous-cl-te': prefix + b'Content-Length: 2\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n',
            'chunked-trailer': prefix + b'Transfer-Encoding: chunked\r\n\r\n' + ('%x\r\n' % len(b'{"jsonrpc":"2.0"')).encode() + b'{"jsonrpc":"2.0"\r\n0\r\nX-Fault-Trailer: mutate\r\n\r\n',
            'malformed-chunk': prefix + b'Transfer-Encoding: chunked\r\n\r\nz\r\nx\r\n0\r\n\r\n',
            'close-before-headers': b'',
            'reset-mid-body': prefix + b'Content-Length: 1000\r\n\r\n{"jsonrpc":"2.0"',
        }
        if case not in responses:
            return
        self.wfile.write(responses[case]); self.wfile.flush()
        if case == 'reset-mid-body':
            self.connection.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack('ii', 1, 0))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bind', required=True)
    parser.add_argument('--port', type=int, required=True)
    parser.add_argument('--journal', type=Path, required=True)
    args = parser.parse_args()
    with socketserver.ThreadingTCPServer((args.bind, args.port), Handler) as server:
        server.daemon_threads = True
        server.journal = args.journal
        server.lock = threading.Lock()
        server.serve_forever()
