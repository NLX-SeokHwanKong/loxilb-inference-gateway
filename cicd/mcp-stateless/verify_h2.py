#!/usr/bin/env python3
"""Real curl HTTP/2 TLS replay with trusted CA and independent receipt journals."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
from urllib.parse import urlsplit
from client import headers_for, request_body
from verify_gateway import journal_size


def parse_payload(payload):
    if payload.lstrip().startswith('{'):
        return [json.loads(payload)]
    return [json.loads(line[6:]) for line in payload.splitlines() if line.startswith('data: ')]


def curl_requests(url, ca, bodies, modifications=None):
    command = ['curl']
    with tempfile.TemporaryDirectory(prefix='uc8-h2-') as folder:
        for index, body in enumerate(bodies):
            if index:
                command.append('--next')
            body_path = Path(folder) / ('request-%d.json' % index)
            body_path.write_text(json.dumps(body), encoding='utf-8')
            command += ['--silent', '--show-error', '--no-buffer', '--http2', '--max-time', '15',
                        '--data-binary', '@' + str(body_path)]
            if urlsplit(url).scheme == 'http':
                command.remove('--http2')
                command.append('--http2-prior-knowledge')
            else:
                command += ['--cacert', str(ca)]
            for key, value in headers_for(body).items():
                command += ['-H', key + ': ' + value]
            command += (modifications or [])
            command += ['--write-out', '\nUC8-CURL:%{http_version}|%{response_code}|%{num_connects}\n', url]
        proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        payload = []
        arrival = []
        reports = []
        for line in proc.stdout:
            if line.startswith('UC8-CURL:'):
                version, status, connections = line.strip()[9:].split('|')
                reports.append({'http_version': version, 'status': int(status),
                                'new_connections': int(connections), 'payload': ''.join(payload),
                                'data_arrival': arrival})
                payload = []
                arrival = []
            else:
                payload.append(line)
                if line.startswith('data: '):
                    arrival.append(time.monotonic())
        stderr = proc.stderr.read()
        proc.wait(timeout=3)
        if proc.returncode:
            raise AssertionError('curl failed: ' + stderr)
        if len(reports) != len(bodies):
            raise AssertionError('Incomplete curl transfer reports')
        return reports


def replay(url, ca, journals, backends, count=8):
    run_id = uuid.uuid4().hex
    bodies = []
    for index in range(count):
        body = request_body('tools/call', run_id + '-%d' % index, name='echo',
                            arguments={'message': 'h2-wire', 'region': '서울'})
        body['params']['_meta']['progressToken'] = body['id']
        bodies.append(body)
    reports = curl_requests(url, ca, bodies)
    receipts = []
    for index, report in enumerate(reports):
        if report['http_version'] != '2' or report['status'] != 200:
            raise AssertionError('HTTP/2 was not negotiated or request failed')
        if report['new_connections'] != (1 if index == 0 else 0):
            raise AssertionError('HTTP/2 transfers did not reuse one TLS connection')
        events = parse_payload(report['payload'])
        finals = [event for event in events if 'id' in event]
        if len(finals) != 1 or finals[0]['id'] != bodies[index]['id']:
            raise AssertionError('Incorrect HTTP/2 final response ID')
        result = finals[0]['result']
        if result.get('resultType') != 'complete' or result['content'][0]['text'] != 'h2-wire':
            raise AssertionError('Incorrect HTTP/2 echo result')
        if len(events) > 1:
            progress = events[:-1]
            if (len(progress) != 2 or any(event.get('method') != 'notifications/progress' for event in progress)
                    or any(event['params']['progressToken'] != bodies[index]['id'] for event in progress)
                    or [event['params']['progress'] for event in progress] != [1, 2]
                    or report['data_arrival'][-1] - report['data_arrival'][0] < .025):
                raise AssertionError('HTTP/2 SSE progress lost or buffered')
        receipt = result['_meta']['com.loxilb.fixture/receipt']
        if receipt['request_id'] != bodies[index]['id']:
            raise AssertionError('Incorrect HTTP/2 backend receipt identity')
        receipts.append(receipt)
    if {row['backend_id'] for row in receipts} != set(backends):
        raise AssertionError('HTTP/2 connection did not route requests to every expected backend')
    entries = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
    executed = {row['receipt_id']: row for row in entries if row.get('event') == 'executed'}
    if any(executed.get(row['receipt_id']) != row for row in receipts):
        raise AssertionError('HTTP/2 response missing matching independent journal receipt')
    negatives = []
    base = request_body('tools/call', run_id + '-bad', name='echo', arguments={'message': 'bad'})
    cases = [('method-mismatch', url, ['-H', 'Mcp-Method: tools/list']),
             ('duplicate-method', url, ['-H', 'Mcp-Method: tools/call']),
             ('long-authority', url, ['-H', 'Host: ' + 'a' * 256]),
             ('long-path', urlsplit(url)._replace(path='/' + 'p' * 256).geturl(), [])]
    for name, address, extra in cases:
        before = journal_size(journals)
        report = curl_requests(address, ca, [base], extra)[0]
        expected = (400, 413) if name.startswith('long-') else (400,)
        if report['http_version'] != '2' or report['status'] not in expected or journal_size(journals) != before:
            raise AssertionError(name + ': expected HTTP/2 400 and zero backend arrival')
        denial=json.loads(report['payload'])
        if denial.get('jsonrpc')!='2.0' or not isinstance(denial.get('error',{}).get('code'),int):
            raise AssertionError(name+': H2 gateway denial is not structured JSON-RPC')
        negatives.append({'case': name, 'status': report['status'], 'backend_journal_delta': 0})
    return {'scope': 'real HTTP/2 request routing and negative headers/bounds',
            'transport': 'cleartext prior knowledge' if urlsplit(url).scheme == 'http' else 'TLS',
            'requests': count, 'receipts': receipts, 'transfers': reports, 'negative_cases': negatives}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--ca', type=Path)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--backend-id', action='append', required=True)
    parser.add_argument('--requests', type=int, default=8)
    args = parser.parse_args()
    if (urlsplit(args.url).scheme not in ('http', 'https')
            or (urlsplit(args.url).scheme == 'https' and not args.ca)
            or len(set(args.backend_id)) < 2
            or args.requests < len(set(args.backend_id))):
        parser.error('HTTP(S), trusted CA for HTTPS, and enough requests for two backends are required')
    print(json.dumps(replay(args.url, args.ca, args.journal, args.backend_id, args.requests), indent=2))


if __name__ == '__main__':
    main()
