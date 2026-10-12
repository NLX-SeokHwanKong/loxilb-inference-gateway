#!/usr/bin/env python3
"""Exact 3:1 MCP weighted routing on persistent and fresh HTTP/1.1 or HTTP/2 connections."""
import argparse
from collections import Counter
import json
from pathlib import Path
import uuid
from urllib.parse import urlsplit
from client import connection, headers_for, messages, request_body
from verify_h2 import curl_requests, parse_payload


def replay(url, journals, backend_a, backend_b, ca=None, http2=False, count=24):
    if count < 8 or count % 4:
        raise ValueError('Use a request count >=8 divisible by four for exact3:1 verification')
    phases = []
    receipts = []
    run = uuid.uuid4().hex
    for fresh in (False, True):
        bodies = [request_body('tools/call', run + '-%s-%d' % (fresh, index), name='echo',
                               arguments={'message': 'weighted'}) for index in range(count)]
        observed = []
        if http2:
            transfers = ([curl_requests(url, ca, [body])[0] for body in bodies] if fresh
                         else curl_requests(url, ca, bodies))
            for index, transfer in enumerate(transfers):
                if transfer['status'] != 200 or transfer['http_version'] != '2':
                    raise AssertionError('Weighted request did not use real HTTP/2 successfully')
                expected_connections = 1 if fresh or index == 0 else 0
                if transfer['new_connections'] != expected_connections:
                    raise AssertionError('Weighted HTTP/2 client connection contract failed')
                observed.append(parse_payload(transfer['payload']))
        else:
            conn, path = connection(url, ca)
            original = None
            try:
                for body in bodies:
                    conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
                    if not fresh:
                        if original is None:
                            original = conn.sock
                        if original is None or conn.sock is not original:
                            raise AssertionError('Weighted HTTP/1.1 client did not reuse its TCP connection')
                    response = conn.getresponse()
                    if response.status != 200:
                        raise AssertionError('Weighted request failed: HTTP%d' % response.status)
                    observed.append(list(messages(response)))
                    if fresh:
                        conn.close(); conn, path = connection(url, ca)
            finally:
                conn.close()
        phase = []
        for body, events in zip(bodies, observed):
            finals = [event for event in events if 'id' in event]
            if len(finals) != 1 or finals[0]['id'] != body['id']:
                raise AssertionError('Weighted response request identity mismatch')
            result = finals[0]['result']
            if result.get('resultType') != 'complete' or result['content'][0]['text'] != 'weighted':
                raise AssertionError('Weighted response content mismatch')
            receipt = result['_meta']['com.loxilb.fixture/receipt']
            if receipt['request_id'] != body['id']:
                raise AssertionError('Weighted backend receipt identity mismatch')
            phase.append(receipt); receipts.append(receipt)
        actual = dict(Counter(row['backend_id'] for row in phase))
        expected = {backend_a: count * 3 // 4, backend_b: count // 4}
        if actual != expected:
            raise AssertionError('Expected exact3:1 per independent phase: ' + json.dumps({'expected': expected, 'actual': actual, 'fresh': fresh}))
        phases.append({'fresh_connections': fresh, 'counts': actual, 'requests': count})
    entries = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
    executed = {row['receipt_id']: row for row in entries if row.get('event') == 'executed'}
    if any(executed.get(receipt['receipt_id']) != receipt for receipt in receipts):
        raise AssertionError('Weighted response missing identical independent backend journal receipt')
    return {'scope': 'explicitly configured3:1 weighted MCP request distribution',
            'transport': 'HTTP/2' if http2 else 'HTTP/1.1', 'phases': phases, 'receipts': receipts}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--ca', type=Path)
    parser.add_argument('--http2', action='store_true')
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--backend-a', default='backend-a')
    parser.add_argument('--backend-b', default='backend-b')
    parser.add_argument('--requests', type=int, default=24)
    args = parser.parse_args()
    if urlsplit(args.url).scheme not in ('http', 'https'):
        parser.error('HTTP(S) URL required')
    print(json.dumps(replay(args.url, args.journal, args.backend_a, args.backend_b, args.ca, args.http2, args.requests), indent=2))
