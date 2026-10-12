#!/usr/bin/env python3
"""Actual concurrent CB failure/open/timeout/recovery with owned TCP backend."""
from concurrent.futures import ThreadPoolExecutor
import copy
import json
import re
import time
import urllib.error
from urllib.parse import urlsplit, urlunsplit
import uuid
from client import connection, headers_for, messages, request_body
from verify_fc_route import _rows, _receipt
from verify_h2 import curl_requests, parse_payload
from verify_l7 import management


def replay(url, management_url, journals, template, backend_control, gateway_logs,
           require_h2=False, port=2044, backend='cb-backend'):
    document = copy.deepcopy(template)
    document['serviceArguments'].update(port=port, sel=0, cb_enable=True, monitor=False)
    for name in list(document['serviceArguments']):
        if name.startswith('fc_') or name == 'api_key_auth':
            document['serviceArguments'].pop(name)
    document['endpoints'] = [document['endpoints'][0]]
    service = document['serviceArguments']
    detail = management_url + '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    try:
        management(detail)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    else:
        raise RuntimeError('Reserved CB port occupied')
    management(management_url + '/config/loadbalancer', 'POST', document)
    readback = management(detail)[1]
    if readback['serviceArguments'].get('cb_enable') is not True or readback['serviceArguments'].get('monitor', False):
        raise AssertionError('CB scenario requires enabled breaker and disabled monitor')
    address = urlsplit(url)
    target = urlunsplit((address.scheme, address.hostname + ':' + str(port), address.path, address.query, ''))
    run = uuid.uuid4().hex
    baseline_logs = gateway_logs()
    baseline_rows = _rows(journals)
    records = []
    def perform(item):
        phase, index, transport = item
        body = request_body('tools/list', '%s-%s-%d' % (run, phase, index))
        if transport == 'HTTP/2':
            transfer = curl_requests(target, None, [body])[0]
            if transfer['http_version'] != '2':
                raise AssertionError('CB request did not use real HTTP/2')
            code = transfer['status']
            payload = transfer['payload']
            events = parse_payload(transfer['payload']) if code == 200 else []
        else:
            conn, path = connection(target)
            try:
                conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
                response = conn.getresponse()
                code = response.status
                events = list(messages(response)) if code == 200 else []
                payload = response.read().decode(errors='replace') if code != 200 else ''
            finally:
                conn.close()
        return {'phase': phase, 'transport': transport, 'status': code, 'body': body,
                'receipt': _receipt(events, body, backend) if code == 200 else None,
                'negative_payload': payload if code != 200 else None}
    transports = ['HTTP/1.1'] * 6 if not require_h2 else ['HTTP/1.1', 'HTTP/2'] * 3
    def concurrent(phase):
        with ThreadPoolExecutor(max_workers=6) as workers:
            return list(workers.map(perform, [(phase, index, transport) for index, transport in enumerate(transports)]))
    observations = []
    def wait_transition(pattern, timeout):
        start = time.monotonic()
        while True:
            log = gateway_logs()[len(baseline_logs):]
            matched = [line for line in log.splitlines() if re.search(pattern, line)]
            if matched:
                observations.append({'transition': pattern, 'elapsed_seconds': time.monotonic() - start, 'lines': matched})
                return matched
            if time.monotonic() - start >= timeout:
                raise AssertionError('Missing actual CB transition: ' + pattern)
            time.sleep(.2)
    try:
        backend_control(False)
        failed = concurrent('dial-failure')
        records.extend(failed)
        if any(row['status'] not in (502, 503) for row in failed):
            raise AssertionError('Concurrent actual TCP failures did not reach default threshold5: ' + json.dumps(failed))
        wait_transition(r'CLOSED\s*(?:→|->)\s*OPEN.*\b5 failures\b', 5)
        if _rows(journals) != baseline_rows:
            raise AssertionError('Failed TCP dials generated backend arrivals')
        # Restore the owned listening socket, while OPEN must continue to deny
        # selection. No manual endpoint state or CB timeout override is used.
        backend_control(True)
        refused = concurrent('open-refusal')
        records.extend(refused)
        if any(row['status'] != 503 for row in refused) or _rows(journals) != baseline_rows:
            raise AssertionError('OPEN must refuse with503 and zero backend arrivals')
        wait_transition(r'OPEN\s*(?:→|->)\s*HALF_OPEN', 45)
        probe=perform(('halfopen-probe',0,'HTTP/1.1'))
        if probe['status']!=200: raise AssertionError('HALF_OPEN first probe failed')
        wait_transition(r'HALF_OPEN\s*(?:→|->)\s*CLOSED',5)
        recovered = [probe]+concurrent('recovered')
        records.extend(recovered)
        if any(row['status'] != 200 for row in recovered):
            raise AssertionError('Recovered HALF_OPEN/CLOSED requests failed')
        wait_transition(r'HALF_OPEN\s*(?:→|->)\s*CLOSED', 5)
        rows = _rows(journals)
        for record in recovered:
            receipt = record['receipt']
            paired = [row for row in rows if row.get('request_id') == record['body']['id']]
            if len(paired) != 2 or {row['event'] for row in paired} != {'received', 'executed'} or receipt not in paired:
                raise AssertionError('CB recovery lacks independent arrival/execution pair')
        if len(rows) - len(baseline_rows) != len(recovered) * 2:
            raise AssertionError('CB negative traffic reached backend or duplicated delivery')
        sibling = None
        if require_h2:
            from verify_h2_multiplex import replay as multiplex_replay
            sibling = multiplex_replay(target, journals, cancel=True)
            if any(receipt['backend_id'] != backend for receipt in sibling['receipts']):
                raise AssertionError('CB recovered sibling escaped its backend')
        return {'scope': 'default CB concurrent TCP failure, OPEN zero-arrival refusal, timeout and successful recovery',
            'defaults': {'failure_threshold': 5, 'open_timeout_seconds': 30, 'halfopen_max_probes': 3, 'success_threshold': 1},
            'halfopen_scope': 'first successful TCP connection closes breaker; no SSE-held probe-cap claim',
            'readback': readback, 'transitions': observations, 'requests': records, 'negative_backend_arrivals': 0,
            'recovered_h2_cancel_sibling': sibling}
    finally:
        backend_control(True)
