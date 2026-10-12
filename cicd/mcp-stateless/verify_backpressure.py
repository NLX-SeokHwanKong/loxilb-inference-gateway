#!/usr/bin/env python3
"""Actual H2 zero-window -> H1 SDK response high/low-water and task cancellation."""
import json
import socket
import time
from urllib.parse import urlsplit
import uuid
import h2.connection
import h2.events
import h2.settings
from client import headers_for, request_body
from verify_h2 import parse_payload


def rows(journal): return [json.loads(line) for line in journal.read_text().splitlines()]

def replay(url,journal,gateway_logs):
    address=urlsplit(url);sock=socket.create_connection((address.hostname,address.port),timeout=1)
    conn=h2.connection.H2Connection();conn.initiate_connection()
    conn.update_settings({h2.settings.SettingCodes.INITIAL_WINDOW_SIZE:0})
    sock.sendall(conn.data_to_send())
    size=64*1024*1024
    body=request_body('tools/call',uuid.uuid4().hex,name='blob',arguments={'size':size})
    def send(stream,body):
        raw=json.dumps(body).encode()
        hdr=[(':method','POST'),(':scheme','http'),(':authority',address.netloc),(':path',address.path)]
        hdr.extend((k.lower(),v) for k,v in headers_for(body).items());hdr.append(('content-length',str(len(raw))))
        conn.send_headers(stream,hdr);conn.send_data(stream,raw,end_stream=True);sock.sendall(conn.data_to_send())
    baseline=gateway_logs();send(1,body)
    deadline=time.monotonic()+60;status=None;held_bytes=0
    try:
        while True:
            if time.monotonic()>deadline: raise AssertionError('Zero-window request did not reach high-water')
            try: data=sock.recv(65536)
            except socket.timeout: data=None
            if data==b'': raise AssertionError('Gateway closed zero-window connection')
            if data:
                for event in conn.receive_data(data):
                    if isinstance(event,h2.events.ResponseReceived): status=dict(event.headers).get(b':status')
                    elif isinstance(event,h2.events.StreamReset): raise AssertionError('Backpressure became RST_STREAM')
                    elif isinstance(event,h2.events.DataReceived): held_bytes+=len(event.data)
                pending=conn.data_to_send()
                if pending:sock.sendall(pending)
            if 'HIGH water mark reached' in gateway_logs()[len(baseline):]: break
        if status!=b'200' or held_bytes: raise AssertionError('Expected 200 headers and zero DATA while window=0')
        conn.increment_flow_control_window(65535,stream_id=1);sock.sendall(conn.data_to_send())
        payload=bytearray();ended=False;deadline=time.monotonic()+120
        while not ended:
            if time.monotonic()>deadline: raise AssertionError('Resumed large response timed out')
            try:data=sock.recv(65536)
            except socket.timeout:continue
            if not data:raise AssertionError('Large response truncated')
            for event in conn.receive_data(data):
                if isinstance(event,h2.events.DataReceived):
                    payload.extend(event.data)
                    # receive_data can close the stream before returning its final DATA event.
                    # The public API acknowledges connection bytes and safely skips a closed stream.
                    conn.acknowledge_received_data(event.flow_controlled_length,event.stream_id)
                elif isinstance(event,h2.events.StreamReset): raise AssertionError('Resumed bridge reset')
                elif isinstance(event,h2.events.StreamEnded):ended=True
            pending=conn.data_to_send()
            if pending:sock.sendall(pending)
        events=parse_payload(payload.decode());final=[e for e in events if e.get('id')==body['id']]
        if len(final)!=1 or final[0]['result']['content'][0].get('text')!='x'*size:
            raise AssertionError('Large SDK result bytes changed')
        if 'LOW water mark reached' not in gateway_logs()[len(baseline):]:
            raise AssertionError('No actual low-water release evidence')
    finally:sock.close()
    # A separate stream owns a real long-running SDK task. Disconnect MUST
    # cancel that task, not only stop sending a completed fixture response.
    sock=socket.create_connection((address.hostname,address.port),timeout=.2)
    conn=h2.connection.H2Connection();conn.initiate_connection();sock.sendall(conn.data_to_send())
    token=uuid.uuid4().hex;wait=request_body('tools/call',uuid.uuid4().hex,name='wait',arguments={'token':token})
    send(1,wait);deadline=time.monotonic()+10
    try:
        while not any(r.get('event')=='sdk-started' and r.get('token')==token for r in rows(journal)):
            if time.monotonic()>deadline:raise AssertionError('SDK task never started')
            try:data=sock.recv(65536)
            except socket.timeout:continue
            if not data:raise AssertionError('Connection closed before SDK task start')
            conn.receive_data(data)
            pending=conn.data_to_send()
            if pending:sock.sendall(pending)
        conn.reset_stream(1,error_code=8);sock.sendall(conn.data_to_send())
        deadline=time.monotonic()+10
        while not any(r.get('event')=='sdk-cancelled' and r.get('token')==token for r in rows(journal)):
            if time.monotonic()>deadline:raise AssertionError('Frontend RST did not cancel backend SDK task')
            time.sleep(.1)
        if any(r.get('event')=='sdk-finished' and r.get('token')==token for r in rows(journal)):
            raise AssertionError('Cancelled SDK task finished')
    finally:sock.close()
    return {'scope':'real H2 window, H1 SDK 64MiB result and actual SDK task cancellation',
        'held_window_data_bytes':held_bytes,'result_bytes':size,'response_bytes':len(payload),
        'high_low_water_observed':True,'task_cancel_token':token,'task_cancelled':True}
