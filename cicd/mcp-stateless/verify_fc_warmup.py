#!/usr/bin/env python3
"""Verify MCP return warmup from installed state and concurrent backend traffic."""
import argparse
import copy
import json
from pathlib import Path
import socket
import ssl
import threading
import time
import urllib.error
from urllib.parse import urlsplit, urlunsplit
import uuid
from client import connection, headers_for, messages, request_body
from verify_fc_route import _receipt, _rows
from verify_h2 import parse_payload
from verify_l7 import management, policy_document


class Request:
    def __init__(self, url, transport, identity, ca):
        self.body = request_body('resources/read', identity, uri='fixture://status')
        self.body['params']['_meta']['progressToken'] = identity
        self.progress = threading.Event()
        self.ended = threading.Event()
        self.events = []
        self.error = None
        self.status = None
        self.worker = threading.Thread(target=self.run, args=(url, transport, ca), daemon=True)
        self.worker.start()

    def observe(self, event):
        self.events.append(event)
        if event.get('method') == 'notifications/progress' and event['params']['progressToken'] == self.body['id']:
            self.progress.set()

    def run(self, url, transport, ca):
        try:
            if transport == 'HTTP/1.1':
                conn, path = connection(url, ca)
                try:
                    conn.request('POST', path, json.dumps(self.body).encode(), headers_for(self.body))
                    response = conn.getresponse()
                    self.status = response.status
                    if self.status == 200:
                        for event in messages(response):
                            self.observe(event)
                    else:
                        response.read()
                finally:
                    conn.close()
            else:
                self.h2(url, ca)
        except BaseException as error:
            self.error = error
        finally:
            self.ended.set()

    def h2(self, url, ca):
        import h2.connection
        import h2.config
        import h2.events
        address = urlsplit(url)
        sock = socket.create_connection((address.hostname, address.port or (443 if address.scheme == 'https' else 80)), timeout=10)
        try:
            if address.scheme == 'https':
                context = ssl.create_default_context(cafile=ca)
                context.set_alpn_protocols(['h2'])
                sock = context.wrap_socket(sock, server_hostname=address.hostname)
                if sock.selected_alpn_protocol() != 'h2':
                    raise AssertionError('Warmup probe did not negotiate HTTP/2')
            engine = h2.connection.H2Connection(config=h2.config.H2Configuration(client_side=True, header_encoding='utf-8'))
            engine.initiate_connection()
            fields = [(':method', 'POST'), (':scheme', address.scheme), (':authority', address.netloc), (':path', address.path or '/mcp')]
            fields += [(key.lower(), value) for key, value in headers_for(self.body).items() if key.lower() != 'host']
            payload = json.dumps(self.body).encode()
            fields.append(('content-length', str(len(payload))))
            engine.send_headers(1, fields)
            engine.send_data(1, payload, end_stream=True)
            sock.sendall(engine.data_to_send())
            body = bytearray()
            seen = 0
            done = False
            while not done:
                data = sock.recv(65536)
                if not data:
                    raise AssertionError('HTTP/2 closed before END_STREAM')
                for event in engine.receive_data(data):
                    if isinstance(event, h2.events.ResponseReceived):
                        self.status = int(dict(event.headers)[':status'])
                    elif isinstance(event, h2.events.DataReceived):
                        body.extend(event.data)
                        engine.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
                        blocks = body.decode().replace('\r\n', '\n').split('\n\n')[:-1]
                        complete = [json.loads(line[6:]) for block in blocks for line in block.splitlines() if line.startswith('data: ')]
                        for item in complete[seen:]:
                            self.observe(item)
                        seen = len(complete)
                    elif isinstance(event, h2.events.StreamReset):
                        raise AssertionError('HTTP/2 reset instead of warmup HTTP outcome')
                    elif isinstance(event, h2.events.StreamEnded):
                        done = True
                out = engine.data_to_send()
                if out:
                    sock.sendall(out)
            if self.status == 200 and not self.events:
                for item in parse_payload(body.decode()):
                    self.observe(item)
        finally:
            sock.close()

    def active(self):
        if not self.progress.wait(10) or self.status != 200 or self.ended.is_set():
            raise AssertionError('Warmup probe lacks an actual active SSE request') from self.error

    def finish(self):
        if not self.ended.wait(10):
            raise AssertionError('Warmup request timed out')
        if self.error:
            raise self.error
        return self.status


def effective(url, installation_observations=None):
    started = time.monotonic()
    while True:
        readback = management(url)[1]
        result = readback['serviceArguments'].get('fc_effective')
        if installation_observations is not None:
            installation_observations.append({'elapsed_seconds': round(time.monotonic() - started, 6), 'readback': readback})
        if result and result.get('mode') == 'enforce' and result.get('ep_max_inflight') == 4 and result.get('warmup_ms') == 12000 and result.get('source', {}).get('warmup_ms') == 'rule':
            return result
        if installation_observations is None or time.monotonic() - started >= 10:
            raise AssertionError('Installed configured warmup is missing: ' + json.dumps(installation_observations if installation_observations is not None else readback))
        time.sleep(0.1)


def replay(url, management_url, journals, template, port=2040, backend_a='fc-backend-a', backend_b='fc-backend-b', require_h2=False, ca=None):
    results = []
    for offset, transport in enumerate(['HTTP/1.1'] + (['HTTP/2'] if require_h2 else [])):
        document = copy.deepcopy(template)
        document['serviceArguments'].update(port=port + offset, sel=0, fc_mode='enforce', fc_max_outstanding=8,
            fc_ep_max_inflight=4, fc_max_queue_depth=0, fc_adaptive='off', fc_warmup_ms=12000)
        document['serviceArguments'].pop('api_key_auth', None)
        original = copy.deepcopy(document['endpoints'])
        document['endpoints'] = [original[0]]
        service = document['serviceArguments']
        key = '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], service['port'])
        detail = management_url + key
        try:
            management(detail)
        except urllib.error.HTTPError as error:
            if error.code != 404:
                raise
        else:
            raise RuntimeError('Reserved warmup port already occupied')
        management(management_url + '/config/loadbalancer', 'POST', document)
        installation_observations = []
        before = effective(detail, installation_observations)
        if before.get('warming_endpoints') != 0:
            raise AssertionError('Fresh pool unexpectedly warming')
        # Use a stable one-endpoint pool: the host-state backend signal is
        # IP-wide even when epPort selects its monitor. Two ports on the same
        # host would both return and warm. Membership replacement may recreate
        # the pool, so it cannot prove an existing endpoint's return warmup.
        readback = management(detail)[1]
        policy = policy_document(readback['serviceArguments']['id'], str(uuid.uuid4()))
        for rule in policy['rules']:
            rule['action']['forward']['backendRefs'] = [{'ep': 0, 'weight': 1}]
        management(management_url + '/config/l7policy', 'POST', policy)
        address = urlsplit(url)
        target = urlunsplit((address.scheme, address.hostname + ':' + str(service['port']), address.path, address.query, ''))
        identity = uuid.uuid4().hex
        host = {'hostName': original[0]['endpointIP'], 'epPort': original[0]['targetPort'], 'epProto': 'tcp', 'state': 'red'}
        try:
            management(management_url + '/config/endpointhoststate', 'POST', host)
            down = Request(target, transport, identity + '-down', ca)
            if down.finish() != 503:
                raise AssertionError('Explicit red A endpoint remained routable: ' + json.dumps({'status': down.status, 'effective': effective(detail)}))
        finally:
            # Restore the exact runner-owned endpoint even if a probe fails.
            host['state'] = 'green'
            management(management_url + '/config/endpointhoststate', 'POST', host)
        warm = effective(detail)
        if warm.get('warming_endpoints') != 1:
            raise AssertionError('Returning A did not enter installed warmup: ' + json.dumps(warm))
        hold = Request(target, transport, identity + '-warm-hold', ca)
        hold.active()
        active = effective(detail)
        if active.get('inflight') != 1 or active.get('warming_endpoints') != 1:
            raise AssertionError('Warmup active counter is incorrect: ' + json.dumps(active))
        refused = Request(target, transport, identity + '-warm-refused', ca)
        if refused.finish() not in (429, 503) or hold.ended.is_set():
            raise AssertionError('Warmup must limit actual concurrent A requests to one')
        if hold.finish() != 200:
            raise AssertionError('Warmup held A request failed')
        deadline = time.monotonic() + 20
        observations = []
        while True:
            recovered = effective(detail)
            observations.append(recovered)
            if recovered.get('warming_endpoints') == 0:
                break
            if time.monotonic() >= deadline:
                raise AssertionError('Installed warmup never completed')
            time.sleep(0.1)
        # Completion is read from installed state, then verified by concurrent
        # traffic. Elapsed time by itself is never the recovery criterion.
        first = Request(target, transport, identity + '-recovered-1', ca)
        first.active()
        second = Request(target, transport, identity + '-recovered-2', ca)
        second.active()
        concurrent = effective(detail)
        if first.ended.is_set() or concurrent.get('inflight') != 2 or concurrent.get('warming_endpoints') != 0:
            raise AssertionError('Full endpoint capacity did not recover after installed warmup ended: ' + json.dumps(concurrent))
        requests = [hold, first, second]
        receipts = [_receipt(request.events, request.body, backend_a) for request in requests if request.finish() == 200]
        if len(receipts) != 3:
            raise AssertionError('Missing successful warmup/recovery receipts')
        rows = _rows(journals)
        if any(row.get('request_id') in (down.body['id'], refused.body['id']) for row in rows):
            raise AssertionError('Warmup-refused request reached a backend')
        for request, receipt in zip(requests, receipts):
            paired = [row for row in rows if row.get('request_id') == request.body['id']]
            if len(paired) != 2 or {row['event'] for row in paired} != {'received', 'executed'} or receipt not in paired or any(row['backend_id'] != backend_a for row in paired):
                raise AssertionError('Warmup backend evidence mismatch')
        settled = effective(detail)
        if settled.get('inflight') != 0:
            raise AssertionError('Warmup probes leaked capacity')
        results.append({'transport': transport, 'before': before, 'installation_observations': installation_observations, 'warm': warm, 'active': active, 'refused_status': refused.status,
            'warmup_observations': observations, 'recovered_concurrent': concurrent, 'settled': settled, 'receipts': receipts,
            'down_status': down.status, 'down_backend_arrivals': 0, 'refused_backend_arrivals': 0, 'policy': policy})
    return {'scope': 'MCP configured return warmup and actual capacity recovery', 'checks': results}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--management-url', required=True)
    parser.add_argument('--journal', type=Path, action='append', required=True)
    parser.add_argument('--template', type=Path, required=True)
    parser.add_argument('--port', type=int, default=2040)
    parser.add_argument('--require-h2', action='store_true')
    parser.add_argument('--ca')
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.management_url, args.journal, json.loads(args.template.read_text()), args.port,
                           require_h2=args.require_h2, ca=args.ca), indent=2))
