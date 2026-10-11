#!/usr/bin/env python3
"""Actual refused TCP dial followed by a capacity-full same-route candidate."""
import copy
import json
import queue
import socket
import threading
import urllib.error
from urllib.parse import urlsplit, urlunsplit
import uuid
from client import connection, headers_for, messages, request_body
from verify_fc_route import _effective_counter, _receipt, _rows
from verify_l7 import management, policy_document


def replay(url, management_url, journals, template, port=2042, closed_port=8299, backend='fc-backend-a'):
    document = copy.deepcopy(template)
    document['serviceArguments'].update(port=port, sel=0, fc_mode='enforce', fc_max_outstanding=8,
        fc_ep_max_inflight=1, fc_max_queue_depth=0, fc_adaptive='off', cb_enable=False, monitor=False)
    document['serviceArguments'].pop('api_key_auth', None)
    live = copy.deepcopy(document['endpoints'][0])
    closed = copy.deepcopy(live)
    closed['targetPort'] = closed_port
    document['endpoints'] = [closed, live]
    service = document['serviceArguments']
    detail = management_url + '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    try:
        management(detail)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    else:
        raise RuntimeError('Reserved FC retry port occupied')
    # Binding exclusively without listen reserves this runner-owned endpoint
    # while real TCP connects receive refusal. Never stop another process.
    reservation = socket.socket()
    hold = None
    worker = None
    try:
        reservation.bind((live['endpointIP'], closed_port))
        management(management_url + '/config/loadbalancer', 'POST', document)
        observations = []
        import time
        started = time.monotonic()
        while True:
            readback = management(detail)[1]
            effective = readback['serviceArguments'].get('fc_effective')
            observations.append({'elapsed_seconds': time.monotonic() - started, 'readback': readback})
            if effective and effective.get('mode') == 'enforce' and effective.get('ep_max_inflight') == 1 and effective.get('queue_depth') == 0:
                break
            if time.monotonic() - started >= 10:
                raise AssertionError('Installed retry FC missing: ' + json.dumps(observations))
            time.sleep(.1)
        if readback['serviceArguments'].get('cb_enable', False) or readback['serviceArguments'].get('monitor', False):
            raise AssertionError('Retry probe requires disabled circuit breaker and monitoring')
        policy = policy_document(readback['serviceArguments']['id'], str(uuid.uuid4()))
        policy['rules'] = [policy['rules'][1], policy['rules'][2], policy['rules'][-1]]
        policy['rules'][0]['position'] = 1
        policy['rules'][0]['action']['forward']['backendRefs'] = [{'ep': 1, 'weight': 1}]
        policy['rules'][1]['position'] = 2
        policy['rules'][1]['action']['forward']['backendRefs'] = [{'ep': 0, 'weight': 1}]
        policy['rules'][2]['position'] = 3
        policy['rules'][2]['action']['forward']['backendRefs'] = [{'ep': 0, 'weight': 1}, {'ep': 1, 'weight': 1}]
        management(management_url + '/config/l7policy', 'POST', policy)
        address = urlsplit(url)
        target = urlunsplit((address.scheme, address.hostname + ':' + str(port), address.path, address.query, ''))
        identity = uuid.uuid4().hex
        body = request_body('resources/read', identity + '-hold', uri='fixture://status')
        body['params']['_meta']['progressToken'] = body['id']
        hold, path = connection(target)
        progress = queue.Queue()
        ended = threading.Event()
        events = []
        errors = []
        def consume():
            try:
                hold.request('POST', path, json.dumps(body).encode(), headers_for(body))
                response = hold.getresponse()
                if response.status != 200:
                    raise AssertionError('Retry capacity holder failed')
                for event in messages(response):
                    events.append(event)
                    if event.get('method') == 'notifications/progress':
                        progress.put(event)
            except Exception as error:
                errors.append(error)
            finally:
                ended.set()
        worker = threading.Thread(target=consume, daemon=True)
        worker.start()
        first = progress.get(timeout=10)
        if first['params']['progressToken'] != body['id'] or ended.is_set():
            raise AssertionError('Retry probe lacks active holder')
        active = _effective_counter(detail, 1)
        blocked = request_body('tools/list', identity + '-blocked')
        before = _rows(journals)
        conn, path = connection(target)
        try:
            conn.request('POST', path, json.dumps(blocked).encode(), headers_for(blocked))
            response = conn.getresponse()
            status = response.status
            response.read()
        finally:
            conn.close()
        if status != 502 or ended.is_set():
            raise AssertionError('Actual dial retry into full candidate must refuse502 while holder active: ' + str(status))
        after = _rows(journals)
        if after != before:
            raise AssertionError('Refused retry produced backend arrival/execution')
        retained = _effective_counter(detail, 1)
        if not ended.wait(10):
            raise AssertionError('Retry holder timed out')
        if errors:
            raise errors[0]
        receipts = [_receipt(events, body, backend)]
        settled = _effective_counter(detail, 0)
        # A is now free, but a closed-only route must never escape to it.
        closed_only = request_body('prompts/get', identity + '-closed-only', name='welcome')
        rows_before = _rows(journals)
        conn, path = connection(target)
        try:
            conn.request('POST', path, json.dumps(closed_only).encode(), headers_for(closed_only))
            response = conn.getresponse()
            closed_status = response.status
            response.read()
        finally:
            conn.close()
        if closed_status != 502 or _rows(journals) != rows_before:
            raise AssertionError('Closed-only retry escaped its route or returned incorrect status: ' + str(closed_status))
        closed_effective = _effective_counter(detail, 0)
        # Four requests traverse both catchall RR positions twice, proving
        # repeated successful live retry and exactly-once backend arrival.
        for number in range(4):
            recovery = request_body('tools/list', identity + '-recovery-' + str(number))
            conn, path = connection(target)
            try:
                conn.request('POST', path, json.dumps(recovery).encode(), headers_for(recovery))
                response = conn.getresponse()
                if response.status != 200:
                    failure = response.read().decode(errors='replace')
                    raise AssertionError('Retry capacity did not recover: ' + json.dumps({'request_id': recovery['id'], 'recovery_index': number, 'status': response.status, 'payload': failure, 'effective': management(detail)[1]}))
                receipts.append(_receipt(list(messages(response)), recovery, backend))
            finally:
                conn.close()
        rows = _rows(journals)
        for receipt in receipts:
            paired = [row for row in rows if row.get('request_id') == receipt['request_id']]
            if len(paired) != 2 or {row['event'] for row in paired} != {'received', 'executed'} or receipt not in paired or any(row['backend_id'] != backend for row in paired):
                raise AssertionError('Retry independent backend receipt mismatch')
        return {'transport': 'HTTP/1.1', 'scope': 'real refused TCP dial and same-pool endpoint capacity',
            'closed_port': closed_port, 'installation_observations': observations, 'policy': policy,
            'active': active, 'blocked_status': status, 'blocked_backend_arrivals': 0, 'retained': retained,
            'settled': settled, 'closed_only_status': closed_status, 'closed_only_backend_arrivals': 0,
            'closed_only_effective': closed_effective, 'recovered': _effective_counter(detail, 0), 'receipts': receipts}
    finally:
        if hold:
            hold.close()
        if worker:
            worker.join(timeout=1)
        reservation.close()
