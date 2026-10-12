"""Wire-level fixture checks. Gateway qualification requires a separate replay."""
import copy
from contextlib import closing
import http.client
import json
import tempfile
import subprocess
import sys
import threading
import time
import unittest
from pathlib import Path
from client import headers_for, messages, request_body
from fixture import FixtureServer, PREFIX, encode_header, decode_header


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.path = Path(self.temp.name) / 'receipt.jsonl'
        self.server = FixtureServer(('127.0.0.1', 0), 'backend-a', str(self.path), delay=0.03)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.conn = self.connect()

    def tearDown(self):
        self.conn.close()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()
        self.temp.cleanup()

    def connect(self):
        return http.client.HTTPConnection(*self.server.server_address, timeout=3)

    def send(self, body, headers=None, conn=None):
        conn = conn or self.conn
        conn.request('POST', '/mcp', json.dumps(body).encode(), headers_for(body) if headers is None else headers)
        response = conn.getresponse()
        status = response.status
        events = list(messages(response))
        self.assertIsNone(response.getheader('Mcp-Session-Id'))
        return status, events

    def reject(self, body, headers, code=-32020, status=400):
        before = len(self.server.receipts)
        got, events = self.send(body, headers)
        self.assertEqual(got, status)
        self.assertEqual(events[0]['error']['code'], code)
        self.assertEqual(len(self.server.receipts), before)
        if self.path.exists():
            self.assertEqual(sum(json.loads(row)['event'] == 'executed' for row in self.path.read_text().splitlines()), before)

    def test_empty_journal_exists_before_first_request(self):
        self.assertTrue(self.path.is_file())
        self.assertEqual(self.path.read_text(), '')

    def test_cli_rejects_unbounded_progress_delay(self):
        import subprocess
        import sys
        for delay in ('nan', 'inf', '-1'):
            result = subprocess.run([sys.executable, '-B', str(Path(__file__).parent / 'fixture.py'),
                                     '--journal', str(self.path), '--progress-delay', delay],
                                    capture_output=True, text=True, timeout=2)
            self.assertEqual(result.returncode, 2)
            self.assertIn('finite and nonnegative', result.stderr)

    def test_independent_methods_same_connection_and_fresh_connection(self):
        specs = [('server/discover', {}), ('tools/list', {}),
                 ('tools/call', {'name': 'echo', 'arguments': {'message': 'hello'}}),
                 ('resources/read', {'uri': 'fixture://status'}), ('prompts/get', {'name': 'welcome'}),
                 ('resources/list', {}), ('prompts/list', {})]
        ids = []
        for index, (method, params) in enumerate(specs):
            status, events = self.send(request_body(method, index, **params))
            self.assertEqual(status, 200)
            result = events[0]['result']
            self.assertEqual(events[0]['id'], index)
            self.assertEqual(result['resultType'], 'complete')
            receipt = result['_meta']['com.loxilb.fixture/receipt']
            self.assertEqual(receipt['backend_id'], 'backend-a')
            ids.append(receipt['connection_id'])
        self.assertEqual(len(set(ids)), 1)
        with closing(self.connect()) as fresh:
            self.send(request_body('tools/list', 90), conn=fresh)
        self.assertNotEqual(self.server.receipts[-1]['connection_id'], ids[0])
        self.assertEqual(sum(json.loads(row)['event'] == 'executed' for row in self.path.read_text().splitlines()), 8)

    def test_missing_mismatched_and_duplicate_headers_zero_execution(self):
        body = request_body('tools/call', 1, name='echo', arguments={'message': 'hello', 'region': 'edge-a'})
        for name in ('MCP-Protocol-Version', 'Mcp-Method', 'Mcp-Name', 'Mcp-Param-Region'):
            with self.subTest(name=name):
                headers = headers_for(body)
                del headers[name]
                self.reject(body, headers)
                headers = headers_for(body)
                headers[name] = 'wrong'
                self.reject(body, headers)
                before = len(self.server.receipts)
                raw = json.dumps(body).encode()
                self.conn.putrequest('POST', '/mcp')
                for key, value in headers_for(body).items():
                    self.conn.putheader(key, value)
                self.conn.putheader(name.lower(), headers_for(body)[name])
                self.conn.putheader('Content-Length', str(len(raw)))
                self.conn.endheaders(raw)
                response = self.conn.getresponse()
                self.assertEqual(response.status, 400)
                self.assertEqual(list(messages(response))[0]['error']['code'], -32020)
                self.assertEqual(len(self.server.receipts), before)

    def test_versions_and_per_request_metadata_not_inherited(self):
        valid = request_body('tools/list', 1)
        self.send(valid)
        for field in ('protocolVersion', 'clientCapabilities'):
            body = copy.deepcopy(valid)
            del body['params']['_meta'][PREFIX + field]
            self.reject(body, headers_for(valid), -32602)
        body = request_body('tools/list', 2)
        body['params']['_meta'][PREFIX + 'protocolVersion'] = '2025-11-25'
        self.reject(body, headers_for(body), -32022)
        body = request_body('initialize', 3)
        self.reject(body, headers_for(body), -32601, 404)

    def test_base64_unicode_whitespace_and_literal_sentinel(self):
        for value in ('서울', ' padded ', 'line1\nline2', '=?base64?literal?=', 'edge-a'):
            with self.subTest(value=value):
                self.assertEqual(decode_header(encode_header(value)), value)
                body = request_body('tools/call', value, name='echo',
                                    arguments={'message': value, 'region': value})
                status, events = self.send(body)
                self.assertEqual(status, 200)
                self.assertEqual(events[0]['result']['content'][0]['text'], value)
        body = request_body('tools/call', 100, name='echo', arguments={'message': 'x'})
        headers = headers_for(body)
        headers['Mcp-Name'] = '=?base64?###?='
        self.reject(body, headers)
        headers['Mcp-Name'] = encode_header('different')
        self.reject(body, headers)
        headers['Mcp-Name'] = '=?base64?ZWNobw==?='
        self.assertEqual(self.send(body, headers)[0], 200)

    def test_origin_legacy_http_and_session_headers(self):
        body = request_body('tools/list', 1)
        headers = headers_for(body)
        headers['Origin'] = 'https://untrusted.example'
        self.reject(body, headers, -32600, 403)
        headers = headers_for(body)
        headers.update({'Mcp-Session-Id': 'obsolete', 'Last-Event-ID': 'old'})
        self.assertEqual(self.send(body, headers)[0], 200)
        for method in ('GET', 'DELETE'):
            with closing(self.connect()) as conn:
                conn.request(method, '/mcp')
                response = conn.getresponse()
                self.assertEqual(response.status, 405)
                self.assertEqual(response.getheader('Allow'), 'POST')
                response.read()

    def test_real_sse_progress_final_and_connection_reuse(self):
        self.server.response_mode = 'sse'
        body = request_body('tools/call', 1, name='echo', arguments={'message': 'stream'})
        body['params']['_meta']['progressToken'] = 'p1'
        self.conn.request('POST', '/mcp', json.dumps(body).encode(), headers_for(body))
        response = self.conn.getresponse()
        self.assertEqual(response.getheader('Transfer-Encoding'), 'chunked')
        self.assertEqual(response.getheader('X-Accel-Buffering'), 'no')
        stream = messages(response)
        first = next(stream)
        self.assertEqual(first['method'], 'notifications/progress')
        # Final response is not yet available: first event precedes two delayed flushes.
        started = time.monotonic()
        rest = list(stream)
        self.assertGreaterEqual(time.monotonic() - started, 0.025)
        self.assertEqual(first['params']['progressToken'], 'p1')
        self.assertEqual(rest[0]['params']['progress'], 2)
        self.assertEqual(rest[-1]['id'], 1)
        self.assertEqual(rest[-1]['result']['resultType'], 'complete')
        self.assertEqual(self.send(request_body('tools/list', 2))[0], 200)
        self.assertEqual(self.server.receipts[0]['connection_id'], self.server.receipts[1]['connection_id'])

    def test_invalid_shapes_parse_errors_and_tool_arguments(self):
        valid = request_body('tools/list', 1)
        for body in ([valid], {**valid, 'id': None}, {**valid, 'id': True}, {**valid, 'result': {}}):
            self.reject(body, headers_for(valid), -32600)
        body = request_body('tools/call', 5, name='echo', arguments={})
        self.reject(body, headers_for(body), -32602)
        for raw in (b'{', b'{"jsonrpc":"2.0","jsonrpc":"2.0"}', b'{"x":NaN}'):
            before = len(self.server.receipts)
            self.conn.request('POST', '/mcp', raw, headers_for(valid))
            response = self.conn.getresponse()
            self.assertEqual(response.status, 400)
            self.assertEqual(list(messages(response))[0]['error']['code'], -32700)
            self.assertEqual(len(self.server.receipts), before)


    def test_media_negatives_and_framing_rejects(self):
        body = request_body('tools/list', 1)
        for accept in ('application/json', 'text/event-stream', 'application/json, text/event-stream;q=0'):
            headers = headers_for(body)
            headers['Accept'] = accept
            self.reject(body, headers, -32600, 406)
        headers = headers_for(body)
        headers['Content-Type'] = 'text/plain'
        self.reject(body, headers, -32600, 415)
        for extra, status in (([('Content-Length', '0'), ('Content-Length', '0')], 400),
                              ([('Content-Length', '0'), ('Transfer-Encoding', 'chunked')], 400),
                              ([('Content-Length', '1048577')], 413)):
            with closing(self.connect()) as conn:
                before = len(self.server.receipts)
                conn.putrequest('POST', '/mcp')
                for name, value in extra:
                    conn.putheader(name, value)
                conn.endheaders()
                response = conn.getresponse()
                self.assertEqual(response.status, status)
                response.read()
                self.assertEqual(len(self.server.receipts), before)

    def test_chunked_body_keepalive_and_fragmented_headers(self):
        body = request_body('tools/call', 1, name='echo', arguments={'message': 'chunked'})
        raw = json.dumps(body).encode()
        headers = headers_for(body)
        headers['Transfer-Encoding'] = 'chunked'
        # Iterable input forces multiple chunks while preserving HTTP framing.
        self.conn.request('POST', '/mcp', iter([raw[:17], raw[17:]]), headers, encode_chunked=True)
        response = self.conn.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(list(messages(response))[0]['result']['content'][0]['text'], 'chunked')
        self.assertEqual(self.send(request_body('tools/list', 2))[0], 200)
        self.assertEqual(self.server.receipts[-1]['connection_id'], self.server.receipts[-2]['connection_id'])

    def test_cli_json_sse_persistent_and_fresh_connections(self):
        script = Path(__file__).with_name('client.py')
        for mode in ('json', 'sse'):
            self.server.response_mode = mode
            for fresh in (False, True):
                before = len(self.server.receipts)
                command = [sys.executable, '-B', str(script), '--url',
                           'http://%s:%d/mcp' % self.server.server_address,
                           '--requests', '3', '--progress']
                if fresh:
                    command.append('--new-connection')
                result = subprocess.run(command, capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                reports = [json.loads(row) for row in result.stdout.splitlines()]
                self.assertEqual(len(reports), 3)
                ids = {row['connection_id'] for row in self.server.receipts[before:]}
                self.assertEqual(len(ids), 3 if fresh else 1)
                for report in reports:
                    self.assertEqual(report['status'], 200)
                    self.assertEqual(len(report['events']), 3 if mode == 'sse' else 1)


if __name__ == '__main__':
    unittest.main()
