#!/usr/bin/env python3
"""Header/body-safe MCP routing and policy detach/reattach during active SSE."""
import argparse
import json
from pathlib import Path
import time
import urllib.request
import uuid
from client import connection, headers_for, messages, request_body
from verify_gateway import journal_size


def management(url, method='GET', body=None):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(url, data=data, headers={'Content-Type': 'application/json'}, method=method)
    with urllib.request.urlopen(request, timeout=10) as response:
        raw = response.read()
        return response.status, json.loads(raw) if raw else None


def policy_document(lb_id, policy_id):
    def condition(key, value):
        return {'field': 'HEADER', 'op': 'EQUAL_TO', 'key': key, 'value': value}
    def route(position, method=None, name=None, refs=(0,)):
        conditions = []
        if method is not None:
            conditions.append(condition('mcp-method', method))
        if name is not None:
            conditions.append(condition('mcp-name', name))
        if not conditions:
            conditions = [{'field': 'PATH', 'op': 'STARTS_WITH', 'value': '/'}]
        return {'position': position, 'matchSets': [{'conditions': conditions}],
                'action': {'kind': 'FORWARD', 'forward': {'backendRefs': [{'ep': ep, 'weight': 1} for ep in refs]}}}
    return {'name': 'UC8 MCP validated method/name routing', 'id': policy_id, 'lbId': lb_id,
            'rules': [route(1, 'tools/call', 'echo', (0, 1)),
                      route(2, 'resources/read', 'fixture://status', (0,)),
                      route(3, 'prompts/get', 'welcome', (1,)),
                      {'position':98,'matchSets':[{'conditions':[condition('x-route-key','reserved')]}],
                       'action':{'kind':'FORWARD','forward':{'backendRefs':[{'ep':0,'weight':1}]}}},
                      route(99, refs=(0,))]}


def replay(url, management_url, journals, backend_a, backend_b, policy, count=12):
    receipts = []
    report = []
    run_id = uuid.uuid4().hex
    def perform(conn, path, body, headers=None):
        conn.request('POST', path, json.dumps(body).encode(), headers or headers_for(body))
        response = conn.getresponse()
        status = response.status
        events = list(messages(response))
        if status != 200:
            raise AssertionError('L7 positive request failed: ' + json.dumps(events))
        finals = [event for event in events if 'id' in event]
        if len(finals) != 1 or finals[0]['id'] != body['id']:
            raise AssertionError('Missing matching L7 response')
        receipt = finals[0]['result']['_meta']['com.loxilb.fixture/receipt']
        if receipt['request_id'] != body['id']:
            raise AssertionError('Incorrect L7 receipt request identity')
        receipts.append(receipt)
        return receipt
    conn, path = connection(url)
    try:
        sockets = []
        groups = {}
        for encoding in ('plain', 'base64'):
            groups[encoding] = set()
            for index in range(count // 2):
                body = request_body('tools/call', run_id + '-%s-%d' % (encoding, index),
                                    name='echo', arguments={'message': 'route'})
                headers = headers_for(body)
                if encoding == 'base64':
                    headers['Mcp-Name'] = '=?base64?ZWNobw==?='
                receipt = perform(conn, path, body, headers)
                groups[encoding].add(receipt['backend_id'])
                sockets.append(conn.sock)
                report.append({'case': encoding + '-echo', 'backend_id': receipt['backend_id']})
        if any(group != {backend_a, backend_b} for group in groups.values()):
            raise AssertionError('Plain/Base64 echo route must each independently reach both backends')
        if not sockets or sockets[0] is None or any(sock is not sockets[0] for sock in sockets):
            raise AssertionError('L7 test did not reuse one client TCP connection')
        for method, params, expected in [('resources/read', {'uri': 'fixture://status'}, backend_a),
                                          ('prompts/get', {'name': 'welcome'}, backend_b)]:
            for index in range(3):
                receipt = perform(conn, path, request_body(method, run_id + '-%s-%d' % (method, index), **params))
                if receipt['backend_id'] != expected:
                    raise AssertionError(method + ': did not reach configured single backend')
                report.append({'case': method, 'backend_id': expected})
        receipt = perform(conn, path, request_body('tools/list', run_id + '-fallback'))
        if receipt['backend_id'] != backend_a:
            raise AssertionError('Explicit PATH catchall did not reach configured fallback backend')
        report.append({'case': 'explicit-path-catchall', 'backend_id': backend_a})
    finally:
        conn.close()
    # Header/body mismatch is rejected before L7 routing. Generic values that
    # cannot fit a configured L7 operand are rejected rather than truncated.
    valid = request_body('tools/call', run_id + '-negative', name='echo', arguments={'message': 'bad'})
    for name, value, expected in [('Mcp-Name', 'welcome', 400), ('Mcp-Name', '=?base64?###?=', 400),
                                   ('X-Route-Key', 'x' * 256, 413)]:
        before = journal_size(journals)
        headers = headers_for(valid)
        headers[name] = value
        conn, path = connection(url)
        try:
            conn.request('POST', path, json.dumps(valid).encode(), headers)
            response = conn.getresponse()
            status = response.status
            response.read()
        finally:
            conn.close()
        if status != expected or journal_size(journals) != before:
            raise AssertionError(name + ': wrong rejection or backend arrival')
        report.append({'case': 'negative-' + name, 'status': status, 'backend_journal_delta': 0})
    # The route is unambiguously backend-b; first progress proves stream activity.
    conn, path = connection(url)
    body = request_body('prompts/get', run_id + '-policy-churn', name='welcome')
    body['params']['_meta']['progressToken'] = body['id']
    try:
        conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
        response = conn.getresponse()
        stream = messages(response)
        first = next(stream)
        if (response.status != 200 or first.get('method') != 'notifications/progress'
                or first['params']['progressToken'] != body['id']):
            raise AssertionError('Missing active SSE before policy detach')
        started = time.monotonic()
        management(management_url + '/config/l7policy/id/' + policy['id'], method='DELETE')
        management(management_url + '/config/l7policy', method='POST', body=policy)
        remainder = list(stream)
        if not remainder or remainder[-1].get('id') != body['id']:
            raise AssertionError('Policy detach/reattach lost active SSE final response')
        if time.monotonic() - started < .025:
            raise AssertionError('SSE had already completed before lifecycle test')
        receipt = remainder[-1]['result']['_meta']['com.loxilb.fixture/receipt']
        if receipt['backend_id'] != backend_b:
            raise AssertionError('Active SSE switched backend during policy churn')
        receipts.append(receipt)
        report.append({'case': 'policy-detach-reattach-during-sse', 'backend_id': backend_b, 'final_id': body['id']})
    finally:
        conn.close()
    entries = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
    executed = {row['receipt_id']: row for row in entries if row.get('event') == 'executed'}
    if any(executed.get(receipt['receipt_id']) != receipt for receipt in receipts):
        raise AssertionError('L7 response missing identical independent journal receipt')
    readback = management(management_url + '/config/l7policy/id/' + policy['id'])[1]
    return {'scope': 'MCP method/name L7 routing and active policy lifecycle',
            'checks': report, 'receipts': receipts, 'policy_readback': readback}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--management-url', required=True)
    parser.add_argument('--policy', type=Path, required=True)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--backend-a', default='backend-a')
    parser.add_argument('--backend-b', default='backend-b')
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.management_url, args.journal, args.backend_a, args.backend_b,
                            json.loads(args.policy.read_text())), indent=2))


if __name__ == '__main__':
    main()
