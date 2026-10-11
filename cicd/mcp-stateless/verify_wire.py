#!/usr/bin/env python3
"""Adversarial HTTP/1.1 framing checks with independent backend arrival evidence."""
import argparse
import json
from pathlib import Path
import socket
import select
import ssl
import time
import uuid
from urllib.parse import urlsplit
from client import headers_for, request_body
from verify_gateway import journal_size


def connect(url, ca=None):
    parsed = urlsplit(url)
    port = parsed.port or (443 if parsed.scheme == 'https' else 80)
    sock = socket.create_connection((parsed.hostname, port), timeout=5)
    if parsed.scheme == 'https':
        context = ssl.create_default_context(cafile=str(ca) if ca else None)
        context.set_alpn_protocols(['http/1.1'])
        sock = context.wrap_socket(sock, server_hostname=parsed.hostname)
    return sock


def wire(body, headers=None, chunked=False, trailer=None, path='/mcp', head_only=False):
    raw = json.dumps(body).encode()
    headers = dict(headers_for(body) if headers is None else headers)
    headers['Host'] = 'uc8-gateway.local'
    if chunked:
        headers['Transfer-Encoding'] = 'chunked'
    else:
        headers['Content-Length'] = str(len(raw))
    head = ('POST ' + path + ' HTTP/1.1\r\n' + ''.join(key + ': ' + value + '\r\n' for key, value in headers.items()) + '\r\n').encode()
    if head_only:
        return head, raw
    if chunked:
        parts = [raw[:19], raw[19:]]
        raw = b''.join(('%x\r\n' % len(part)).encode() + part + b'\r\n' for part in parts)
        raw += b'0\r\n' + ((trailer + '\r\n').encode() if trailer else b'') + b'\r\n'
    return head + raw


def read_response(stream):
    line = stream.readline(8193)
    if not line.startswith(b'HTTP/'):
        raise AssertionError('Missing HTTP response status')
    status = int(line.split()[1])
    headers = {}
    while True:
        line = stream.readline(8193)
        if line == b'\r\n':
            break
        if not line or len(line) > 8192:
            raise AssertionError('Malformed response headers')
        key, value = line.decode().split(':', 1)
        headers[key.lower()] = value.strip()
    data = b''
    if status < 200:
        return status, headers, data
    if headers.get('transfer-encoding', '').lower() == 'chunked':
        while True:
            size = int(stream.readline().split(b';')[0], 16)
            if size == 0:
                for _ in range(128):
                    trailer = stream.readline(8193)
                    if trailer == b'\r\n':
                        break
                    if not trailer or len(trailer) > 8192:
                        raise AssertionError('Truncated response trailers')
                else:
                    raise AssertionError('Excessive response trailers')
                break
            data += stream.read(size)
            if stream.read(2) != b'\r\n':
                raise AssertionError('Malformed response chunk')
    elif 'content-length' in headers:
        data = stream.read(int(headers['content-length']))
    else:
        raise AssertionError('Unframed response')
    return status, headers, data


def replay(url, journals, ca=None):
    run_id = uuid.uuid4().hex
    report = []
    def valid(suffix):
        return request_body('tools/call', run_id + suffix, name='echo', arguments={'message': 'wire'})
    def execute(name, request, expect, fragment=False):
        before = journal_size(journals)
        uploaded = 0
        with connect(url, ca) as sock:
            if len(request) > 128 * 1024:
                # Observe an early HTTP rejection while uploading. Requiring a
                # complete sendall before reading races a bounded server close.
                deadline = time.monotonic() + 5
                while uploaded < len(request):
                    readable, writable, _ = select.select([sock], [sock], [], max(0, deadline-time.monotonic()))
                    if readable:
                        break
                    if not writable or time.monotonic() >= deadline:
                        raise AssertionError(name + ': upload/rejection timeout')
                    try:
                        uploaded += sock.send(request[uploaded:uploaded + 4096])
                    except (BrokenPipeError, ConnectionResetError):
                        break
            elif fragment:
                for start in range(0, len(request), 7):
                    sock.sendall(request[start:start + 7])
                    time.sleep(.001)
            else:
                sock.sendall(request)
            with sock.makefile('rb') as stream:
                status, _, _ = read_response(stream)
        if status != expect:
            raise AssertionError(name + ': expected %d, got %d' % (expect, status))
        delta = journal_size(journals) - before
        if (expect >= 400 and delta != 0) or (expect == 200 and delta != 2):
            raise AssertionError(name + ': wrong backend arrival/execution delta %d' % delta)
        report.append({'case': name, 'status': status, 'backend_journal_delta': delta,
                       **({'upload_bytes_before_response': uploaded, 'declared_request_bytes': len(request)} if uploaded else {})})
    execute('fragmented-valid', wire(valid('-fragment')), 200, True)
    execute('chunked-valid', wire(valid('-chunked'), chunked=True), 200)
    for trailer in ('Mcp-Method: tools/list', 'X-Unrecognized-Trailer: value'):
        execute('chunked-trailer-' + trailer.split(':')[0],
                wire(valid('-trailer'), chunked=True, trailer=trailer), 400)
    for key, value in (('Accept', 'application/json, text/event-stream;q=0'),
                       ('Content-Type', 'application/json-junk')):
        body = valid('-negative')
        headers = headers_for(body)
        headers[key] = value
        if key == 'Mcp-Name':
            body['params']['name'] = value
        execute('invalid-' + key, wire(body, headers), 400)
    long_host = wire(valid('-long-host')).replace(b'Host: uc8-gateway.local', b'Host: ' + b'h' * 256)
    execute('long-host', long_host, 400)
    execute('long-path', wire(valid('-long-path'), path='/' + 'p' * 256), 413)
    oversized = request_body('tools/call', run_id + '-oversized', name='echo', arguments={'message': 'x' * (768 * 1024)})
    execute('body-over-768-KiB', wire(oversized), 413)
    for method in ('GET', 'DELETE'):
        execute('method-' + method, (method + ' /mcp HTTP/1.1\r\n\r\n').encode(), 405)
    # A valid header-only request must receive 100 before the client sends body.
    body = valid('-expect')
    headers = headers_for(body)
    headers['Expect'] = '100-continue'
    head, payload = wire(body, headers, head_only=True)
    before = journal_size(journals)
    with connect(url, ca) as sock:
        sock.sendall(head)
        with sock.makefile('rb') as stream:
            if read_response(stream)[0] != 100:
                raise AssertionError('Expected interim 100 Continue before body')
            if journal_size(journals) != before:
                raise AssertionError('Headers alone reached backend before body validation')
            sock.sendall(payload)
            if read_response(stream)[0] != 200:
                raise AssertionError('Valid Expect body request failed')
    if journal_size(journals) - before != 2:
        raise AssertionError('Valid Expect request missing backend arrival/execution')
    report.append({'case': 'expect-100-before-body', 'status': 200, 'backend_journal_delta': 2})
    first = wire(valid('-first'))
    bad = valid('-bad')
    headers = headers_for(bad)
    headers['Mcp-Method'] = 'tools/list'
    second = wire(bad, headers)
    # A malformed message in an already-buffered pipeline rejects the whole
    # buffered burst before any side effects. Sequential keep-alive stays usable.
    execute('pipelined-valid-then-invalid-before-delivery', first + second, 400)
    before = journal_size(journals)
    with connect(url, ca) as sock:
        sock.sendall(first)
        with sock.makefile('rb') as stream:
            if read_response(stream)[0] != 200:
                raise AssertionError('First sequential valid request failed')
            sock.sendall(second)
            if read_response(stream)[0] != 400:
                raise AssertionError('Late malformed request not rejected')
    if journal_size(journals) - before != 2:
        raise AssertionError('Late malformed request reached backend')
    report.append({'case': 'keepalive-late-invalid', 'statuses': [200, 400], 'backend_journal_delta': 2})
    return {'scope': 'HTTP/1.1 fragmented, chunked, Expect, trailers, bounds, pipeline rejection', 'checks': report}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--ca', type=Path)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.journal, args.ca), indent=2))


if __name__ == '__main__':
    main()
