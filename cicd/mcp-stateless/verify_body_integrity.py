#!/usr/bin/env python3
"""MCP tool argument bytes stay unchanged with an explicit disabled AI accounting marker."""
import copy
import hashlib
import json
from pathlib import Path
import urllib.error
import uuid
from client import connection, headers_for, messages, request_body
from verify_l7 import management
from verify_h2 import curl_requests, parse_payload


def replay(url, api, journals, template, require_h2=False, port=2037):
    document = copy.deepcopy(template)
    document['serviceArguments'].update(port=port, api_key_auth='disabled')
    service = document['serviceArguments']
    key = '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    try:
        management(api + key)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    else:
        raise RuntimeError('Reserved body-integrity port occupied; refusing mutation')
    management(api + '/config/loadbalancer', 'POST', document)
    readback = management(api + key)[1]
    if readback['serviceArguments'].get('api_key_auth') != 'disabled':
        raise AssertionError('Explicit disabled accounting marker missing from readback')
    checks = []
    for protocol in ('h1', 'h2') if require_h2 else ('h1',):
        body = request_body('tools/call', uuid.uuid4().hex, name='echo', arguments={
            'message': 'preserve tool arguments', 'stream': True,
            'stream_options': {'include_usage': False}, 'nested': {'stream': True}})
        raw = json.dumps(body, indent=2).encode() if protocol == 'h1' else json.dumps(body).encode()
        expected_hash = hashlib.sha256(raw).hexdigest()
        if protocol == 'h1':
            conn, path = connection(url)
            try:
                conn.request('POST', path, raw, headers_for(body))
                response = conn.getresponse()
                if response.status != 200:
                    raise AssertionError('Body-integrity HTTP1 request failed')
                events = list(messages(response))
            finally:
                conn.close()
        else:
            transfer = curl_requests(url, None, [body])[0]
            if transfer['status'] != 200 or transfer['http_version'] != '2':
                raise AssertionError('Body-integrity HTTP2 request failed')
            events = parse_payload(transfer['payload'])
        finals = [event for event in events if event.get('id') == body['id']]
        if len(finals) != 1 or finals[0]['result'].get('resultType') != 'complete':
            raise AssertionError('Missing successful matching body-integrity response')
        receipt = finals[0]['result']['_meta']['com.loxilb.fixture/receipt']
        rows = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
        selected = [row for row in rows if row.get('request_id') == body['id']]
        arrivals = [row for row in selected if row['event'] == 'received']
        executions = [row for row in selected if row['event'] == 'executed']
        if (len(arrivals) != 1 or len(executions) != 1 or executions[0] != receipt
                or arrivals[0].get('body_sha256') != expected_hash):
            raise AssertionError('Backend raw body bytes changed or independent arrival/execution proof missing')
        checks.append({'transport': protocol, 'request_id': body['id'], 'body_bytes': len(raw),
                       'expected_body_sha256': expected_hash, 'backend_body_sha256': arrivals[0]['body_sha256'],
                       'receipt': receipt})
    return {'scope': 'MCP tool argument byte preservation with disabled accounting marker; no auth qualification',
            'rule_readback': readback, 'checks': checks}
