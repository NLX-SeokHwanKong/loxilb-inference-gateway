#!/usr/bin/env python3
"""Replay against a running Gateway; require local independent backend journals."""
import argparse
import copy
import json
import time
import uuid
from pathlib import Path
from client import connection, headers_for, messages, request_body
from fixture import PREFIX


def journal_size(paths):
    return sum(len(path.read_text(encoding='utf-8').splitlines()) if path.exists() else 0 for path in paths)


def replay(url, journals, backends, ca=None, count=12):
    observed = []
    client_sockets = []
    run_id = uuid.uuid4().hex
    for fresh in (False, True):
        conn, path = connection(url, ca)
        try:
            for index in range(count):
                body = request_body('tools/call', '%s-%s-%d' % (run_id, fresh, index),
                                    name='echo', arguments={'message': 'uc8', 'region': '서울'})
                body['params']['_meta']['progressToken'] = body['id']
                conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
                if not fresh:
                    client_sockets.append(conn.sock)
                response = conn.getresponse()
                if response.status != 200 or response.getheader('Mcp-Session-Id') is not None:
                    raise AssertionError('Invalid status or minted session header')
                events = []
                arrival_times = []
                for event in messages(response):
                    arrival_times.append(time.monotonic())
                    events.append(event)
                if response.getheader('Content-Type', '').split(';')[0] == 'text/event-stream':
                    progress = [event for event in events if event.get('method') == 'notifications/progress']
                    if (len(progress) != 2 or any(event['params']['progressToken'] != body['id'] for event in progress)
                            or [event['params']['progress'] for event in progress] != [1, 2]
                            or len(events) != 3 or 'result' not in events[-1]
                            or arrival_times[-1] - arrival_times[0] < 0.025):
                        raise AssertionError('SSE progress lost, mismatched, or buffered until final result')
                finals = [item for item in events if 'id' in item]
                if len(finals) != 1 or finals[0]['id'] != body['id'] or 'result' not in finals[0]:
                    raise AssertionError('Missing matching final result')
                result = finals[0]['result']
                if result.get('resultType') != 'complete' or result['content'][0]['text'] != 'uc8':
                    raise AssertionError('Invalid complete echo result')
                receipt = result['_meta']['com.loxilb.fixture/receipt']
                if receipt['request_id'] != body['id'] or receipt['backend_id'] not in backends:
                    raise AssertionError('Wrong request/backend receipt identity')
                observed.append(receipt)
                if fresh:
                    conn.close()
                    conn, path = connection(url, ca)
            if not fresh:
                if client_sockets[0] is None or any(sock is not client_sockets[0] for sock in client_sockets):
                    raise AssertionError('Client did not preserve the same TCP connection')
                if set(item['backend_id'] for item in observed) != set(backends):
                    raise AssertionError('Same-connection requests did not reach every expected backend')
        finally:
            conn.close()
    # Discovery is optional for the client; execute other capabilities directly
    # after routing checks, keeping all per-request metadata explicit.
    method_receipts = []
    conn, path = connection(url, ca)
    try:
        for method, params in (('server/discover', {}), ('tools/list', {}),
                               ('resources/list', {}), ('resources/read', {'uri': 'fixture://status'}),
                               ('prompts/list', {}), ('prompts/get', {'name': 'welcome'})):
            body = request_body(method, run_id + '-' + method, **params)
            # clientInfo is recommended but optional and must not be required.
            del body['params']['_meta'][PREFIX + 'clientInfo']
            conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
            response = conn.getresponse()
            events = list(messages(response))
            if response.status != 200 or len(events) != 1 or events[0].get('id') != body['id']:
                raise AssertionError(method + ': missing independent matching response')
            result = events[0]['result']
            if result.get('resultType') != 'complete':
                raise AssertionError(method + ': missing complete resultType')
            receipt = result['_meta']['com.loxilb.fixture/receipt']
            if receipt['request_id'] != body['id'] or receipt['backend_id'] not in backends:
                raise AssertionError(method + ': incorrect receipt identity')
            observed.append(receipt)
            method_receipts.append(receipt)
    finally:
        conn.close()
    entries = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
    executed = {row['receipt_id']: row for row in entries if row.get('event') == 'executed'}
    for receipt in observed:
        if executed.get(receipt['receipt_id']) != receipt:
            raise AssertionError('Response receipt has no identical independent backend journal entry')
    # A received entry is logged before fixture protocol validation. No journal
    # delta therefore proves rejection at Gateway, rather than fixture rejection.
    rejected = []
    valid = request_body('tools/call', run_id + '-negative', name='echo', arguments={'message': 'x'})
    cases = []
    for key in ('MCP-Protocol-Version', 'Mcp-Method', 'Mcp-Name'):
        missing = headers_for(valid)
        del missing[key]
        cases.append(('missing-' + key, valid, missing, None))
        mismatch = headers_for(valid)
        mismatch[key] = 'wrong'
        cases.append(('mismatch-' + key, valid, mismatch, None))
        cases.append(('duplicate-' + key, valid, headers_for(valid), key))
    old = request_body('tools/list', run_id + '-old')
    old['params']['_meta'][PREFIX + 'protocolVersion'] = '2025-11-25'
    cases.append(('old-version', old, headers_for(old), None))
    for field in ('protocolVersion', 'clientCapabilities'):
        missing = copy.deepcopy(valid)
        del missing['params']['_meta'][PREFIX + field]
        cases.append(('missing-body-' + field, missing, headers_for(valid), None))
    for malformed_caps in (None, []):
        malformed = copy.deepcopy(valid)
        malformed['params']['_meta'][PREFIX + 'clientCapabilities'] = malformed_caps
        cases.append(('malformed-client-capabilities-' + str(malformed_caps), malformed, headers_for(valid), None))
    misplaced = copy.deepcopy(valid)
    misplaced['_meta'] = misplaced['params'].pop('_meta')
    cases.append(('misplaced-body-meta', misplaced, headers_for(valid), None))
    for name, body, headers, duplicate in cases:
        before = journal_size(journals)
        conn, path = connection(url, ca)
        try:
            if duplicate:
                raw = json.dumps(body).encode()
                conn.putrequest('POST', path)
                for key, value in headers.items():
                    conn.putheader(key, value)
                conn.putheader(duplicate.lower(), headers[duplicate])
                conn.putheader('Content-Length', str(len(raw)))
                conn.endheaders(raw)
            else:
                conn.request('POST', path, json.dumps(body).encode(), headers)
            response = conn.getresponse()
            status = response.status
            response.read()
            if status != 400:
                raise AssertionError('%s: expected 400, got %d' % (name, status))
            if journal_size(journals) != before:
                raise AssertionError(name + ': rejected request reached backend')
            rejected.append({'case': name, 'status': status, 'backend_journal_delta': 0})
        finally:
            conn.close()
    return {'scope': 'Gateway stateless request routing and header rejection',
            'requests': len(observed), 'backend_ids': sorted({row['backend_id'] for row in observed}),
            'receipts': observed, 'independent_methods': [row['method'] for row in method_receipts],
            'negative_cases': rejected}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--backend-id', action='append', required=True)
    parser.add_argument('--ca')
    parser.add_argument('--requests', type=int, default=12)
    args = parser.parse_args()
    if args.requests < len(args.backend_id) or len(set(args.backend_id)) < 2:
        parser.error('Use at least two different backend IDs and enough requests')
    print(json.dumps(replay(args.url, args.journal, args.backend_id, args.ca, args.requests), indent=2))


if __name__ == '__main__':
    main()
