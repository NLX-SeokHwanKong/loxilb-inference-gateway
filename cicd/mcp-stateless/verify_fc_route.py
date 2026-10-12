#!/usr/bin/env python3
"""Real /mcp capacity saturation with an active SSE request and receipt isolation."""
import argparse
import copy
import json
from pathlib import Path
import queue
import socket
import ssl
import threading
import time
import urllib.error
from urllib.parse import urlsplit
import uuid
from client import connection, headers_for, messages, request_body
from verify_l7 import management, policy_document
from verify_h2 import parse_payload


def _rows(journals):
    return [json.loads(line) for path in journals for line in path.read_text().splitlines() if line]


def _receipt(events, body, backend):
    finals = [event for event in events if event.get('id') == body['id']]
    if len(finals) != 1 or 'result' not in finals[0]:
        raise AssertionError('Missing successful matching final response: ' + body['id'])
    receipt = finals[0]['result']['_meta']['com.loxilb.fixture/receipt']
    if receipt['request_id'] != body['id'] or receipt['backend_id'] != backend:
        raise AssertionError('Wrong backend/request receipt: ' + json.dumps(receipt))
    return receipt


def _evidence(journals, bodies, receipts, backend_a, backend_b):
    rows = _rows(journals)
    for key, body in bodies.items():
        selected = [row for row in rows if row.get('request_id') == body['id']]
        if key == 'blocked':
            if selected:
                raise AssertionError('Capacity-refused request reached a backend: ' + json.dumps(selected))
            continue
        backend = backend_a if key in ('hold', 'recovery') else backend_b
        if len(selected) != 2 or {row['event'] for row in selected} != {'received', 'executed'}:
            raise AssertionError('Expected exactly one independent arrival/execution pair')
        if any(row['backend_id'] != backend for row in selected):
            raise AssertionError('Request escaped its explicit L7 route')
        if receipts[key] not in selected:
            raise AssertionError('Wire receipt differs from independent execution journal')
    return {'blocked_backend_arrivals': 0, 'blocked_backend_executions': 0,
            'receipts': receipts}



def _effective_counter(url, inflight):
    readback = management(url)[1]
    effective = readback['serviceArguments'].get('fc_effective')
    if not effective or effective.get('mode') != 'enforce' or effective.get('ep_max_inflight') != 1 or effective.get('queue_depth') != 0:
        raise AssertionError('Installed FC effective state missing or incorrect: ' + json.dumps(readback))
    if effective.get('inflight') != inflight or effective.get('queued') != 0:
        raise AssertionError('Installed FC counters incorrect: ' + json.dumps(effective))
    return effective


def _bodies():
    identity = uuid.uuid4().hex
    result = {
        'hold': request_body('resources/read', identity + '-hold', uri='fixture://status'),
        'blocked': request_body('resources/read', identity + '-blocked', uri='fixture://status'),
        'free': request_body('prompts/get', identity + '-free', name='welcome'),
        'recovery': request_body('resources/read', identity + '-recovery', uri='fixture://status'),
    }
    result['hold']['params']['_meta']['progressToken'] = result['hold']['id']
    return result


def _h1(url, journals, backend_a, backend_b, ca, effective_url):
    bodies = _bodies()
    hold, path = connection(url, ca)
    progress = queue.Queue()
    ended = threading.Event()
    events = []
    errors = []
    status = []
    def consume():
        try:
            body = bodies['hold']
            hold.request('POST', path, json.dumps(body).encode(), headers_for(body))
            response = hold.getresponse()
            status.append(response.status)
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
    try:
        first = progress.get(timeout=10)
        if status != [200] or first['params']['progressToken'] != bodies['hold']['id'] or ended.is_set():
            raise AssertionError('No independently active hold-A SSE request')
        active_effective = _effective_counter(effective_url, 1)
        if ended.is_set():
            raise AssertionError('Hold ended during active counter observation')
        checks = {}
        receipts = {}
        for key in ('blocked', 'free'):
            if ended.is_set():
                raise AssertionError('Hold completed before capacity probe; fixture delay too short')
            conn, request_path = connection(url, ca)
            try:
                body = bodies[key]
                conn.request('POST', request_path, json.dumps(body).encode(), headers_for(body))
                response = conn.getresponse()
                code = response.status
                payload = response.read()
                observed = parse_payload(payload.decode()) if code == 200 else []
            finally:
                conn.close()
            if ended.is_set():
                raise AssertionError('Capacity probe did not complete while hold-A was active')
            if key == 'blocked' and code not in (429, 503):
                raise AssertionError('Busy A route must refuse without selecting free B; status=' + str(code))
            if key == 'free':
                if code != 200:
                    raise AssertionError('Independent B route must remain available; status=' + str(code))
                receipts[key] = _receipt(observed, body, backend_b)
            checks[key] = {'status': code, 'completed_before_hold': True}
        if not ended.wait(10):
            raise AssertionError('Hold-A final response timed out')
        if errors:
            raise errors[0]
        receipts['hold'] = _receipt(events, bodies['hold'], backend_a)
        # Keep hold's client connection open: recovery must come from final
        # response completion, rather than teardown accidentally freeing A.
        conn, request_path = connection(url, ca)
        try:
            body = bodies['recovery']
            conn.request('POST', request_path, json.dumps(body).encode(), headers_for(body))
            response = conn.getresponse()
            if response.status != 200:
                raise AssertionError('A capacity permit leaked after final response')
            receipts['recovery'] = _receipt(list(messages(response)), body, backend_a)
        finally:
            conn.close()
        checks['recovery'] = {'status': 200, 'hold_client_connection_still_open': True}
        return {'transport': 'HTTP/1.1', 'probes': checks,
                'active_effective': active_effective, 'recovered_effective': _effective_counter(effective_url, 0),
                **_evidence(journals, bodies, receipts, backend_a, backend_b)}
    finally:
        hold.close()
        worker.join(timeout=1)


def _h2(url, journals, backend_a, backend_b, ca, effective_url):
    import h2.connection
    import h2.events
    parsed = urlsplit(url)
    sock = socket.create_connection((parsed.hostname, parsed.port or (443 if parsed.scheme == 'https' else 80)), timeout=5)
    if parsed.scheme == 'https':
        context = ssl.create_default_context(cafile=str(ca) if ca else None)
        context.set_alpn_protocols(['h2'])
        sock = context.wrap_socket(sock, server_hostname=parsed.hostname)
        if sock.selected_alpn_protocol() != 'h2':
            sock.close()
            raise AssertionError('Capacity test did not negotiate real h2')
    engine = h2.connection.H2Connection()
    engine.initiate_connection()
    sock.sendall(engine.data_to_send())
    bodies = _bodies()
    stream_ids = {'hold': 1, 'blocked': 3, 'free': 5, 'recovery': 7}
    statuses = {}; payloads = {stream: b'' for stream in stream_ids.values()}; ended = set()
    def submit(key):
        body = bodies[key]
        raw = json.dumps(body).encode()
        fields = [(':method', 'POST'), (':scheme', parsed.scheme), (':authority', parsed.netloc),
                  (':path', parsed.path or '/mcp')]
        fields += [(key.lower(), value) for key, value in headers_for(body).items()]
        fields.append(('content-length', str(len(raw))))
        stream = stream_ids[key]
        engine.send_headers(stream, fields)
        engine.send_data(stream, raw, end_stream=True)
        sock.sendall(engine.data_to_send())
    def pump(deadline):
        if time.monotonic() >= deadline:
            raise AssertionError('HTTP/2 capacity probe timed out')
        data = sock.recv(65536)
        if not data:
            raise AssertionError('HTTP/2 connection ended while hold-A stream active')
        for event in engine.receive_data(data):
            if isinstance(event, h2.events.ResponseReceived):
                statuses[event.stream_id] = int(dict(event.headers)[b':status'])
            elif isinstance(event, h2.events.DataReceived):
                engine.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
                payloads[event.stream_id] += event.data
            elif isinstance(event, h2.events.StreamEnded):
                ended.add(event.stream_id)
            elif isinstance(event, h2.events.StreamReset):
                raise AssertionError('Capacity test received a transport reset instead of an HTTP outcome')
        outgoing = engine.data_to_send()
        if outgoing:
            sock.sendall(outgoing)
    try:
        deadline = time.monotonic() + 15
        submit('hold')
        def completed_progress():
            # DATA frames may split JSON/SSE at any byte, including mid-name.
            complete=payloads[1].decode().replace('\r\n','\n').split('\n\n')[:-1]
            return [json.loads(line[6:]) for block in complete
                    for line in block.splitlines() if line.startswith('data: ')]
        while not any(event.get('method')=='notifications/progress' for event in completed_progress()):
            pump(deadline)
        first_events = completed_progress()
        if statuses.get(1) != 200 or 1 in ended or not any(
                event.get('method') == 'notifications/progress' and
                event['params']['progressToken'] == bodies['hold']['id'] for event in first_events):
            raise AssertionError('No actual active HTTP/2 hold-A progress')
        active_effective = _effective_counter(effective_url, 1)
        submit('blocked'); submit('free')
        while not {3, 5}.issubset(ended):
            pump(deadline)
            if 1 in ended:
                raise AssertionError('Probes did not complete while HTTP/2 hold-A remained active')
        if statuses.get(3) not in (429, 503) or statuses.get(5) != 200:
            raise AssertionError('Wrong HTTP/2 route capacity outcomes: ' + json.dumps(statuses))
        receipts = {'free': _receipt(parse_payload(payloads[5].decode()), bodies['free'], backend_b)}
        while 1 not in ended:
            pump(deadline)
        receipts['hold'] = _receipt(parse_payload(payloads[1].decode()), bodies['hold'], backend_a)
        submit('recovery')
        while 7 not in ended:
            pump(deadline)
        if statuses.get(7) != 200:
            raise AssertionError('HTTP/2 A capacity permit leaked after final END_STREAM')
        receipts['recovery'] = _receipt(parse_payload(payloads[7].decode()), bodies['recovery'], backend_a)
        return {'transport': 'HTTP/2', 'same_client_connection': True,
                'active_effective': active_effective, 'recovered_effective': _effective_counter(effective_url, 0),
                'probes': {'blocked': {'status': statuses[3], 'completed_before_hold': True},
                           'free': {'status': statuses[5], 'completed_before_hold': True},
                           'recovery': {'status': statuses[7], 'same_client_connection': True}},
                **_evidence(journals, bodies, receipts, backend_a, backend_b)}
    finally:
        sock.close()


def replay(url, management_url, journals, template, port=2036,
           backend_a='fc-backend-a', backend_b='fc-backend-b', require_h2=False, ca=None):
    """Caller owns isolated container/fixtures; retain installed rules for readback."""
    document = copy.deepcopy(template)
    document['serviceArguments'].update(port=port, sel=0,
        fc_mode='enforce', fc_max_outstanding=8, fc_ep_max_inflight=1,
        fc_max_queue_depth=0, fc_adaptive='off')
    service = document['serviceArguments']
    service.pop('api_key_auth', None)
    key = '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    try:
        management(management_url + key)
    except urllib.error.HTTPError as error:
        if error.code != 404:
            raise
    else:
        raise RuntimeError('Reserved FC port already occupied; refusing mutation')
    management(management_url + '/config/loadbalancer', 'POST', document)
    started = time.monotonic()
    attempts = []
    while True:
        readback = management(management_url + key)[1]
        effective = readback['serviceArguments'].get('fc_effective')
        attempts.append({'elapsed_seconds': round(time.monotonic() - started, 6), 'readback': readback})
        if effective and effective.get('mode') == 'enforce' and effective.get('ep_max_inflight') == 1 and effective.get('queue_depth') == 0:
            break
        if time.monotonic() - started >= 10:
            raise AssertionError('FC effective enforce/per-endpoint1/queue0 never installed: ' + json.dumps(attempts))
        time.sleep(0.1)
    policy = policy_document(readback['serviceArguments']['id'], str(uuid.uuid4()))
    # Existing validated resource/name route points exactly to original index0;
    # independent prompt/name route points to index1. No artificial AI path.
    management(management_url + '/config/l7policy', 'POST', policy)
    checks = [_h1(url, journals, backend_a, backend_b, ca, management_url + key)]
    if require_h2:
        checks.append(_h2(url, journals, backend_a, backend_b, ca, management_url + key))
    return {'scope': 'actual /mcp route capacity saturation, no backend route escape',
            'rule_readback': readback, 'installation_observations': attempts, 'policy_readback': management(management_url + '/config/l7policy/id/' + policy['id'])[1],
            'checks': checks}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--management-url', required=True)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--template', type=Path, required=True)
    parser.add_argument('--port', type=int, default=2036)
    parser.add_argument('--require-h2', action='store_true')
    parser.add_argument('--ca')
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.management_url, args.journal,
                           json.loads(args.template.read_text()), args.port,
                           require_h2=args.require_h2, ca=args.ca), indent=2))
