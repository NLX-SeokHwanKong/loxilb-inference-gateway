#!/usr/bin/env python3
"""Bounded MCP 2026-07-28 wire regression fixture; not a complete MCP SDK."""
import argparse
import base64
import binascii
import http.server
import json
import hashlib
import math
import re
import ssl
import threading
import time
import uuid

VERSION = '2026-07-28'
PREFIX = 'io.modelcontextprotocol/'
MAX_BODY = 1024 * 1024


def encode_header(value):
    """Encode unsafe ASCII, Unicode, whitespace and literal sentinel values."""
    if (value != value.strip() or any(ord(c) < 32 or ord(c) > 126 for c in value)
            or (value.startswith('=?base64?') and value.endswith('?='))):
        return '=?base64?' + base64.b64encode(value.encode()).decode() + '?='
    return value


def decode_header(value):
    if any(ord(c) < 32 or ord(c) > 126 for c in value):
        raise ValueError('Invalid header character')
    if value.startswith('=?base64?') and value.endswith('?='):
        try:
            return base64.b64decode(value[9:-2], validate=True).decode('utf-8')
        except (ValueError, UnicodeError, binascii.Error) as exc:
            raise ValueError('Invalid Base64 sentinel') from exc
    if value != value.strip():
        raise ValueError('Header whitespace requires Base64')
    return value


def unique_object(pairs):
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError('Duplicate JSON member')
        out[key] = value
    return out


class FixtureServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, backend_id, journal, response_mode='json', origins=(), delay=0.05):
        if not re.fullmatch(r'[A-Za-z0-9_.-]{1,128}', backend_id):
            raise ValueError('backend ID must be a header-safe identifier')
        # Empty is independent zero-arrival evidence; missing is not. Create
        # append-only journal before exposing a listening socket.
        if journal:
            with open(journal, 'a', encoding='utf-8'):
                pass
        super().__init__(address, Handler)
        self.backend_id = backend_id
        self.journal = journal
        self.response_mode = response_mode
        self.origins = set(origins)
        self.delay = delay
        self.lock = threading.Lock()
        self.receipts = []

    def record(self, entry):
        with self.lock:
            if self.journal:
                with open(self.journal, 'a', encoding='utf-8') as out:
                    out.write(json.dumps(entry, ensure_ascii=False) + '\n')
                    out.flush()
            if entry.get('event') == 'executed':
                self.receipts.append(entry)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def setup(self):
        super().setup()
        self.connection.settimeout(5)
        self.connection_id = uuid.uuid4().hex

    def log_message(self, *_args):
        pass

    def reply(self, status, obj=None, extra=None):
        data = b'' if obj is None else json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(data)))
        for key, value in (extra or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(data)

    def error(self, status, code, message, request_id=None, data=None):
        obj = {'jsonrpc': '2.0', 'error': {'code': code, 'message': message}}
        if request_id is not None:
            obj['id'] = request_id
        if data is not None:
            obj['error']['data'] = data
        self.reply(status, obj)

    def do_GET(self):
        self.close_connection = True
        self.reply(405, extra={'Allow': 'POST', 'Connection': 'close'})

    do_DELETE = do_GET
    do_PUT = do_GET

    def header(self, name, required=True, encoded=False):
        values = self.headers.get_all(name, [])
        if len(values) != 1:
            if not values and not required:
                return None
            raise ValueError('Missing or duplicated ' + name)
        value = values[0]
        if encoded:
            return decode_header(value)
        if not value or any(ord(c) < 33 or ord(c) > 126 for c in value):
            raise ValueError('Malformed ' + name)
        return value

    def do_POST(self):
        if self.path != '/mcp':
            self.close_connection = True
            self.server.record({'event': 'rejected-path', 'backend_id': self.server.backend_id,
                                'path': self.path, 'connection_id': self.connection_id})
            self.reply(404, extra={'Connection': 'close'})
            return
        # Decode bounded Content-Length or chunked bodies; reject CL/TE ambiguity.
        lengths = self.headers.get_all('Content-Length', [])
        transfers = self.headers.get_all('Transfer-Encoding', [])
        if (len(lengths) > 1 or len(transfers) > 1 or (lengths and transfers)
                or (not lengths and not transfers)
                or (transfers and transfers[0].lower().strip() != 'chunked')):
            self.close_connection = True
            self.error(400, -32600, 'Unambiguous supported body framing required')
            return
        try:
            if transfers:
                parts = []
                total = 0
                while True:
                    line = self.rfile.readline(8193)
                    if not line.endswith(b'\r\n') or len(line) > 8192:
                        raise ValueError('Malformed chunk size')
                    token = line[:-2].split(b';', 1)[0]
                    if not re.fullmatch(b'[0-9a-fA-F]+', token):
                        raise ValueError('Malformed chunk size')
                    size = int(token, 16)
                    total += size
                    if total > MAX_BODY:
                        self.close_connection = True
                        self.error(413, -32600, 'Body limit exceeded')
                        return
                    if size == 0:
                        # This bounded fixture intentionally accepts no trailers.
                        if self.rfile.readline(8193) != b'\r\n':
                            raise ValueError('Trailers unsupported')
                        break
                    part = self.rfile.read(size)
                    if len(part) != size or self.rfile.read(2) != b'\r\n':
                        raise ValueError('Truncated chunk')
                    parts.append(part)
                raw = b''.join(parts)
            else:
                if not lengths[0].isdigit():
                    raise ValueError('Invalid Content-Length')
                length = int(lengths[0])
                if length > MAX_BODY:
                    self.close_connection = True
                    self.error(413, -32600, 'Body limit exceeded')
                    return
                raw = self.rfile.read(length)
                if len(raw) != length:
                    raise ValueError('Truncated body')
        except (ValueError, TimeoutError):
            self.close_connection = True
            self.error(400, -32600, 'Invalid request framing')
            return
        try:
            identity_body = json.loads(raw.decode('utf-8'), object_pairs_hook=unique_object,
                                       parse_constant=lambda _: (_ for _ in ()).throw(ValueError('Nonfinite JSON')))
            identity = identity_body.get('id') if isinstance(identity_body, dict) else None
        except (ValueError, UnicodeError):
            identity = None
        self.server.record({'event': 'received', 'backend_id': self.server.backend_id, 'request_id': identity,
                            'body_sha256': hashlib.sha256(raw).hexdigest(),
                            'connection_id': self.connection_id, 'transport_id': uuid.uuid4().hex})
        origins = self.headers.get_all('Origin', [])
        if origins and (len(origins) != 1 or origins[0] not in self.server.origins):
            self.error(403, -32600, 'Origin not allowed')
            return
        try:
            ctypes = self.headers.get_all('Content-Type', [])
            if len(ctypes) != 1 or ctypes[0].split(';')[0].strip().lower() != 'application/json':
                self.error(415, -32600, 'application/json required')
                return
            accepts = ','.join(self.headers.get_all('Accept', []))
            types = set()
            for part in accepts.split(','):
                pieces = [piece.strip() for piece in part.split(';')]
                quality = 1.0
                for parameter in pieces[1:]:
                    if parameter.startswith('q='):
                        try:
                            quality = float(parameter[2:])
                        except ValueError:
                            quality = 0.0
                if 0 < quality <= 1:
                    types.add(pieces[0])
            if not {'application/json', 'text/event-stream'} <= types:
                self.error(406, -32600, 'Accept must include JSON and SSE')
                return
            body = json.loads(raw.decode('utf-8'), object_pairs_hook=unique_object,
                              parse_constant=lambda _: (_ for _ in ()).throw(ValueError('Nonfinite JSON')))
        except (ValueError, UnicodeError):
            self.error(400, -32700, 'Invalid JSON')
            return
        request_id = body.get('id') if isinstance(body, dict) else None
        if (not isinstance(body, dict) or body.get('jsonrpc') != '2.0'
                or not isinstance(body.get('method'), str)
                or type(request_id) not in (str, int)
                or 'result' in body or 'error' in body):
            self.error(400, -32600, 'Single JSON-RPC request required')
            return
        params = body.get('params')
        meta = params.get('_meta') if isinstance(params, dict) else None
        if (not isinstance(meta, dict) or not isinstance(meta.get(PREFIX + 'protocolVersion'), str)
                or not isinstance(meta.get(PREFIX + 'clientCapabilities'), dict)):
            self.error(400, -32602, 'Per-request metadata required', request_id)
            return
        if 'progressToken' in meta and type(meta['progressToken']) not in (str, int):
            self.error(400, -32602, 'Invalid progress token', request_id)
            return
        method = body['method']
        try:
            version = self.header('MCP-Protocol-Version')
            if version != meta[PREFIX + 'protocolVersion'] or self.header('Mcp-Method') != method:
                raise ValueError('Header/body mismatch')
            if method in ('tools/call', 'prompts/get', 'resources/read'):
                source = params.get('uri' if method == 'resources/read' else 'name')
                if not isinstance(source, str) or self.header('Mcp-Name', encoded=True) != source:
                    raise ValueError('Name header/body mismatch')
            if method == 'tools/call' and params.get('name') == 'echo':
                args = params.get('arguments', {})
                if not isinstance(args, dict):
                    self.error(400, -32602, 'Arguments must be object', request_id)
                    return
                region = args.get('region')
                mirrored = self.header('Mcp-Param-Region', required=region is not None, encoded=True)
                if mirrored != region:
                    raise ValueError('Region header/body mismatch')
        except ValueError as exc:
            self.error(400, -32020, str(exc), request_id)
            return
        if version != VERSION:
            self.error(400, -32022, 'Unsupported protocol version', request_id,
                       {'supported': [VERSION], 'requested': version})
            return
        result = self.dispatch(method, params)
        if result is None:
            self.error(404, -32601, 'Method not found', request_id)
            return
        if isinstance(result, str):
            self.error(400, -32602, result, request_id)
            return
        receipt = {'event': 'executed', 'backend_id': self.server.backend_id, 'request_id': request_id,
                   'method': method, 'name': params.get('name', params.get('uri')),
                   'connection_id': self.connection_id, 'receipt_id': uuid.uuid4().hex}
        self.server.record(receipt)
        result.update({'resultType': 'complete', '_meta': {
            PREFIX + 'serverInfo': {'name': self.server.backend_id, 'version': '1.0'},
            'com.loxilb.fixture/receipt': receipt}})
        response = {'jsonrpc': '2.0', 'id': request_id, 'result': result}
        if self.server.response_mode == 'json':
            self.reply(200, response, {'X-Fixture-Backend': self.server.backend_id})
            return
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.send_header('Transfer-Encoding', 'chunked')
        self.send_header('X-Accel-Buffering', 'no')
        self.send_header('X-Fixture-Backend', self.server.backend_id)
        self.end_headers()
        try:
            self.chunk(b': request scoped stream\n\n')
            if 'progressToken' in meta:
                for progress in (1, 2):
                    notification = {'jsonrpc': '2.0', 'method': 'notifications/progress',
                                    'params': {'progressToken': meta['progressToken'],
                                               'progress': progress, 'total': 2}}
                    self.event(notification)
                    time.sleep(self.server.delay)
            self.event(response)
            self.wfile.write(b'0\r\n\r\n')
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError, TimeoutError):
            self.close_connection = True
            self.server.record({**receipt, 'event': 'cancelled'})

    def chunk(self, data):
        self.wfile.write(('%x\r\n' % len(data)).encode() + data + b'\r\n')
        self.wfile.flush()

    def event(self, obj):
        self.chunk(('event: message\ndata: ' + json.dumps(obj, ensure_ascii=False) + '\n\n').encode())

    def dispatch(self, method, params):
        if method == 'server/discover':
            return {'supportedVersions': [VERSION], 'capabilities': {'tools': {}, 'resources': {}, 'prompts': {}}}
        if method == 'tools/list':
            return {'tools': [{'name': 'echo', 'description': 'Return fixture message.', 'inputSchema': {
                'type': 'object', 'properties': {'message': {'type': 'string'},
                'region': {'type': 'string', 'x-mcp-header': 'Region'}}, 'required': ['message']}}]}
        if method == 'tools/call':
            if params.get('name') != 'echo':
                return 'Unknown tool'
            args = params.get('arguments', {})
            if not isinstance(args, dict) or not isinstance(args.get('message'), str):
                return 'String message required'
            return {'content': [{'type': 'text', 'text': args['message']}], 'isError': False}
        if method == 'resources/list':
            return {'resources': [{'uri': 'fixture://status', 'name': 'status', 'mimeType': 'text/plain'}]}
        if method == 'resources/read':
            if params.get('uri') != 'fixture://status':
                return 'Unknown resource'
            return {'contents': [{'uri': params['uri'], 'mimeType': 'text/plain', 'text': 'ready'}]}
        if method == 'prompts/list':
            return {'prompts': [{'name': 'welcome', 'description': 'Fixture prompt'}]}
        if method == 'prompts/get':
            if params.get('name') != 'welcome':
                return 'Unknown prompt'
            return {'messages': [{'role': 'user', 'content': {'type': 'text', 'text': 'Hello'}}]}
        return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bind', default='127.0.0.1')
    parser.add_argument('--port', type=int, default=8080)
    parser.add_argument('--backend-id', default='backend-a')
    parser.add_argument('--journal', required=True)
    parser.add_argument('--response-mode', choices=('json', 'sse'), default='json')
    parser.add_argument('--progress-delay', type=float, default=.05)
    parser.add_argument('--allow-origin', action='append', default=[])
    parser.add_argument('--cert')
    parser.add_argument('--key')
    args = parser.parse_args()
    if not math.isfinite(args.progress_delay) or args.progress_delay < 0:
        parser.error('--progress-delay must be finite and nonnegative')
    if bool(args.cert) != bool(args.key):
        parser.error('--cert and --key must be paired')
    server = FixtureServer((args.bind, args.port), args.backend_id, args.journal,
                           args.response_mode, args.allow_origin, delay=args.progress_delay)
    if args.cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(args.cert, args.key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == '__main__':
    main()
