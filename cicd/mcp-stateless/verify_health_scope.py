#!/usr/bin/env python3
"""Same-IP endpoint port isolation and explicit host-wide health-state scope."""
import copy
import json
import time
import urllib.error
from urllib.parse import urlsplit, urlunsplit
import uuid
from client import connection, headers_for, messages, request_body
from verify_fc_route import _receipt, _rows
from verify_h2 import curl_requests, parse_payload
from verify_l7 import management, policy_document


def replay(url, management_url, journals, template, require_h2=False, port=2046):
    document = copy.deepcopy(template)
    document['serviceArguments'].update(port=port, sel=0, fc_mode='enforce', fc_max_outstanding=8,
        fc_ep_max_inflight=4, fc_max_queue_depth=0, fc_adaptive='off', fc_warmup_ms=12000)
    document['serviceArguments'].pop('api_key_auth', None)
    service = document['serviceArguments']
    endpoints = document['endpoints']
    if len(endpoints) != 2 or endpoints[0]['endpointIP'] != endpoints[1]['endpointIP'] or endpoints[0]['targetPort'] == endpoints[1]['targetPort']:
        raise ValueError('Health scope requires two different ports on the same owned host')
    detail = management_url + '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    try:
        management(detail)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    else:
        raise RuntimeError('Reserved health-scope port occupied')
    management(management_url + '/config/loadbalancer', 'POST', document)
    observed = []
    def effective(warming=None):
        started = time.monotonic()
        while True:
            readback = management(detail)[1]
            result = readback['serviceArguments'].get('fc_effective')
            observed.append({'elapsed_seconds': time.monotonic() - started, 'readback': readback})
            if result and result.get('mode') == 'enforce' and result.get('ep_max_inflight') == 4 and result.get('warmup_ms') == 12000 and (warming is None or result.get('warming_endpoints') == warming):
                return result, readback
            if time.monotonic() - started >= 10:
                raise AssertionError('Health-scope installed warmup mismatch: ' + json.dumps(observed))
            time.sleep(.1)
    before, readback = effective(0)
    policy = policy_document(readback['serviceArguments']['id'], str(uuid.uuid4()))
    management(management_url + '/config/l7policy', 'POST', policy)
    address = urlsplit(url)
    target = urlunsplit((address.scheme, address.hostname + ':' + str(port), address.path, address.query, ''))
    run = uuid.uuid4().hex
    probes = []
    transports = ['HTTP/1.1'] + (['HTTP/2'] if require_h2 else [])
    def probe(phase, route, expected, transport):
        method, params, backend = (('resources/read', {'uri': 'fixture://status'}, 'fc-backend-a') if route == 'A'
                                   else ('prompts/get', {'name': 'welcome'}, 'fc-backend-b'))
        body = request_body(method, run + '-' + phase + '-' + route + '-' + transport, **params)
        rows_before = _rows(journals)
        if transport == 'HTTP/2':
            transfer = curl_requests(target, None, [body])[0]
            if transfer['http_version'] != '2':
                raise AssertionError('Health-scope probe did not use real HTTP/2')
            code = transfer['status']
            events = parse_payload(transfer['payload']) if code == 200 else []
        else:
            conn, path = connection(target)
            try:
                conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
                response = conn.getresponse()
                code = response.status
                events = list(messages(response)) if code == 200 else []
                if code != 200:
                    response.read()
            finally:
                conn.close()
        rows_after = _rows(journals)
        if code != expected:
            raise AssertionError('Health-scope incorrect outcome: ' + json.dumps({'phase': phase, 'route': route, 'status': code, 'expected': expected}))
        receipt = None
        if expected != 200:
            if rows_after != rows_before:
                raise AssertionError('Inactive route produced a backend arrival')
        else:
            receipt = _receipt(events, body, backend)
            new = rows_after[len(rows_before):] if len(journals) == 1 else [row for row in rows_after if row.get('request_id') == body['id']]
            if len(new) != 2 or {row['event'] for row in new} != {'received', 'executed'} or receipt not in new or any(row['backend_id'] != backend for row in new):
                raise AssertionError('Health-scope routing lacks independent receipts')
        probes.append({'phase': phase, 'route': route, 'transport': transport, 'status': code,
                       'backend_arrivals': 1 if expected == 200 else 0, 'receipt': receipt})
    state_url = management_url + '/config/endpointhoststate'
    exact = {'hostName': endpoints[0]['endpointIP'], 'epPort': endpoints[0]['targetPort'], 'epProto': 'tcp', 'state': 'red'}
    broad = {'hostName': endpoints[0]['endpointIP'], 'state': 'red'}
    invalid_tuples = []
    for invalid in ({'hostName': endpoints[0]['endpointIP'], 'epPort': endpoints[0]['targetPort'], 'state': 'red'},
                    {'hostName': endpoints[0]['endpointIP'], 'epProto': 'tcp', 'state': 'red'}):
        rows_before = _rows(journals)
        try:
            management(state_url, 'POST', invalid)
        except urllib.error.HTTPError as error:
            if error.code != 400:
                raise AssertionError('Invalid health tuple expected400: ' + json.dumps({'request': invalid, 'status': error.code, 'response': error.read().decode(errors='replace')})) from error
        else:
            raise AssertionError('Unpaired endpoint-port/protocol tuple was accepted')
        if _rows(journals) != rows_before:
            raise AssertionError('Invalid health tuple generated backend arrivals')
        invalid_tuples.append({'request': invalid, 'status': 400, 'backend_arrivals': 0})
    effective(0)
    for transport in transports:
        probe('invalid-tuples-unchanged', 'A', 200, transport)
        probe('invalid-tuples-unchanged', 'B', 200, transport)
    try:
        management(state_url, 'POST', exact)
        for transport in transports:
            probe('A-port-red', 'A', 503, transport)
            probe('A-port-red-B-active', 'B', 200, transport)
    finally:
        exact['state'] = 'green'
        management(state_url, 'POST', exact)
    warm_one, _ = effective(1)
    for transport in transports:
        probe('A-port-return-B-active', 'B', 200, transport)
        probe('A-port-return-A-active', 'A', 200, transport)
    try:
        management(state_url, 'POST', broad)
        for transport in transports:
            probe('host-wide-red', 'A', 503, transport)
            probe('host-wide-red', 'B', 503, transport)
    finally:
        broad['state'] = 'green'
        management(state_url, 'POST', broad)
    warm_both, _ = effective(2)
    for transport in transports:
        probe('host-wide-return', 'A', 200, transport)
        probe('host-wide-return', 'B', 200, transport)
    return {'scope': 'explicit same-IP epPort isolation and omitted-port host-wide transition',
            'before': before, 'port_return': warm_one, 'host_return': warm_both,
            'observations': observed, 'policy': policy, 'invalid_tuples': invalid_tuples, 'probes': probes}
