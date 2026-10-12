#!/usr/bin/env python3
"""Real HTTP/2 concurrent streams and cancellation without sibling interruption."""
import argparse
import json
from pathlib import Path
import socket
import ssl
import time
from urllib.parse import urlsplit
import uuid
import h2.connection
import h2.events
from client import headers_for, request_body
from verify_h2 import parse_payload


def replay(url, journals, ca=None, cancel=False, fault_case=None, fault_journal=None):
    parsed = urlsplit(url)
    sock = socket.create_connection((parsed.hostname, parsed.port or (443 if parsed.scheme == 'https' else 80)), timeout=5)
    if parsed.scheme == 'https':
        context = ssl.create_default_context(cafile=str(ca) if ca else None)
        context.set_alpn_protocols(['h2'])
        sock = context.wrap_socket(sock, server_hostname=parsed.hostname)
        if sock.selected_alpn_protocol() != 'h2':
            raise AssertionError('TLS did not negotiate real h2')
    conn = h2.connection.H2Connection()
    conn.initiate_connection()
    sock.sendall(conn.data_to_send())
    bodies = {}; payloads = {}; arrivals = {}; ended = set(); reset = set(); cancelled = False
    for index in range(2 if cancel or fault_case else 4):
        stream = index * 2 + 1
        body = request_body('prompts/get' if cancel else 'tools/call', uuid.uuid4().hex,
                            **({'name': 'welcome'} if cancel else {'name': 'echo', 'arguments': {'message': 'multiplex'}}))
        if fault_case:
            body = request_body('tools/call', uuid.uuid4().hex, name='echo',
                                arguments={'message': fault_case if index == 0 else 'multiplex',
                                           'region': 'fault' if index == 0 else 'normal'})
        body['params']['_meta']['progressToken'] = body['id']
        raw = json.dumps(body).encode()
        headers = [(':method', 'POST'), (':scheme', parsed.scheme), (':authority', parsed.netloc), (':path', parsed.path or '/mcp')]
        headers += [(key.lower(), value) for key, value in headers_for(body).items()]
        headers.append(('content-length', str(len(raw))))
        conn.send_headers(stream, headers)
        conn.send_data(stream, raw, end_stream=True)
        bodies[stream] = body; payloads[stream] = b''; arrivals[stream] = []
    sock.sendall(conn.data_to_send())
    deadline = time.monotonic() + 15
    try:
        while len(ended | reset) < len(bodies):
            if time.monotonic() >= deadline:
                raise AssertionError('HTTP/2 multiplex completion timeout')
            data = sock.recv(65536)
            if not data:
                raise AssertionError('Connection closed while sibling streams active')
            for event in conn.receive_data(data):
                if isinstance(event, h2.events.ResponseReceived):
                    headers = dict(event.headers)
                    if headers.get(b':status') != b'200':
                        raise AssertionError('Concurrent response non200: ' + str(headers))
                elif isinstance(event, h2.events.DataReceived):
                    conn.acknowledge_received_data(event.flow_controlled_length, event.stream_id)
                    payloads[event.stream_id] += event.data
                    if b'data: ' in event.data:
                        arrivals[event.stream_id].append(time.monotonic())
                    if cancel and event.stream_id == 1 and b'notifications/progress' in payloads[1] and not cancelled:
                        conn.reset_stream(1, error_code=8)
                        reset.add(1); cancelled = True
                elif isinstance(event, h2.events.StreamEnded):
                    ended.add(event.stream_id)
                elif isinstance(event, h2.events.StreamReset):
                    reset.add(event.stream_id)
                    if event.stream_id != 1 or not (cancel or fault_case):
                        raise AssertionError('Unexpected sibling stream reset')
            outgoing = conn.data_to_send()
            if outgoing:
                sock.sendall(outgoing)
    finally:
        sock.close()
    if cancel and not cancelled:
        raise AssertionError('Cancellation did not occur during actual progress')
    if fault_case and (1 not in reset or 1 in ended):
        raise AssertionError('Malformed upstream stream must reset without normal END_STREAM')
    if fault_case:
        rows = [json.loads(line) for line in fault_journal.read_text().splitlines()]
        if sum(row['request_id'] == bodies[1]['id'] for row in rows) != 1:
            raise AssertionError('Fault stream missing exactly one independent upstream request')
    receipts = []
    for stream, body in bodies.items():
        if (cancel or fault_case) and stream == 1:
            continue
        if stream not in ended:
            raise AssertionError('Sibling stream did not end normally')
        events = parse_payload(payloads[stream].decode())
        finals = [event for event in events if 'id' in event]
        if len(finals) != 1 or finals[0]['id'] != body['id']:
            raise AssertionError('Concurrent response ID mismatch')
        if len(events) > 1 and (len(arrivals[stream]) < 2 or arrivals[stream][-1] - arrivals[stream][0] < .025):
            raise AssertionError('Concurrent SSE buffered until completion')
        receipt = finals[0]['result']['_meta']['com.loxilb.fixture/receipt']
        if receipt['request_id'] != body['id']:
            raise AssertionError('Concurrent receipt request identity mismatch')
        receipts.append(receipt)
    entries = [json.loads(row) for path in journals for row in path.read_text().splitlines()]
    if any(receipt not in entries for receipt in receipts):
        raise AssertionError('Concurrent response missing independent backend receipt')
    if not cancel and not fault_case and {row['backend_id'] for row in receipts} != {'backend-a', 'backend-b'}:
        raise AssertionError('Concurrent streams did not reach both backends')
    return {'scope': 'HTTP/2 sibling stream isolation', 'cancelled_stream': 1 if cancel else None,
            'upstream_fault_case': fault_case,
            'completed_streams': sorted(ended - {1} if cancel or fault_case else ended), 'receipts': receipts,
            'limitation': 'does not certify backend task cancellation semantics'}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--ca', type=Path)
    parser.add_argument('--cancel', action='store_true')
    parser.add_argument('--journal', type=Path, action='append', required=True)
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.journal, args.ca, args.cancel), indent=2))
