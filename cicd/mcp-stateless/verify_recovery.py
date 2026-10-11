#!/usr/bin/env python3
"""Linux campaign-owned fixture outage/recovery; never stop unrelated processes."""
import argparse
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import uuid
from client import connection, headers_for, messages, request_body
from verify_gateway import journal_size


def invoke(url, method, **params):
    body = request_body(method, uuid.uuid4().hex, **params)
    conn, path = connection(url)
    try:
        conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
        response = conn.getresponse()
        events = list(messages(response))
        receipt = next((event['result']['_meta']['com.loxilb.fixture/receipt']
                        for event in events if 'result' in event), None)
        return response.status, receipt
    finally:
        conn.close()


def replay(runtime, url, rr_url):
    runtime = runtime.resolve()
    state_path = runtime / 'fixtures.json'
    state = json.loads(state_path.read_text())
    if state.get('owner') != 'uc8-mcp-20261011':
        raise RuntimeError('Unknown fixture owner')
    matches = [row for row in state['fixtures'] if row['backend_id'] == 'backend-a']
    if len(matches) != 1:
        raise RuntimeError('Expected exactly one owned backend-a')
    row = matches[0]
    pid = row['pid']
    if (Path('/proc/%d/stat' % pid).read_text().split()[21] != row['start_tick']
            or str(runtime / 'fixture.py').encode() not in Path('/proc/%d/cmdline' % pid).read_bytes()):
        raise RuntimeError('Fixture PID ownership mismatch')
    journals = [Path(item['journal']) for item in state['fixtures']]
    report = []
    status, receipt = invoke(url, 'resources/read', uri='fixture://status')
    if status != 200 or receipt is None or receipt['backend_id'] != 'backend-a':
        raise AssertionError('Single-endpoint route prerequisite failed')
    os.kill(pid, signal.SIGTERM)
    try:
        deadline = time.monotonic() + 5
        while Path('/proc/%d' % pid).exists():
            if time.monotonic() >= deadline:
                raise RuntimeError('Owned fixture did not stop')
            time.sleep(.1)
        before = journal_size(journals)
        status, receipt = invoke(url, 'resources/read', uri='fixture://status')
        if status != 502 or receipt is not None or journal_size(journals) != before:
            raise AssertionError('Unavailable explicit route: expected502/no-arrival; actual=' + json.dumps({'status': status, 'receipt': receipt, 'journal_delta': journal_size(journals)-before}))
        report.append({'case': 'single-route-backend-connect-refused', 'status': status, 'backend_journal_delta': 0})
        distribution = []
        for _ in range(4):
            status, receipt = invoke(rr_url, 'tools/call', name='echo', arguments={'message': 'survivor'})
            if status not in (200, 502) or (receipt is not None and receipt['backend_id'] != 'backend-b'):
                raise AssertionError('Unexpected default-route outage behavior')
            distribution.append({'status': status, 'backend_id': receipt['backend_id'] if receipt else None})
        if not any(item['status'] == 200 for item in distribution):
            raise AssertionError('Default RR did not reach surviving backend-b')
        report.append({'case': 'default-rr-survivor', 'monitoring': 'not configured; no health failover qualification',
                       'requests': distribution})
    finally:
        # Restart only the exact fixture just stopped. Preserve backend-b and journals.
        if Path('/proc/%d' % pid).exists():
            raise RuntimeError('Old owned fixture still exists; refusing duplicate restart')
        with (runtime / 'backend-a.log').open('ab', buffering=0) as log:
            proc = subprocess.Popen(['python3', '-B', str(runtime / 'fixture.py'), '--bind', '172.30.98.1',
                                     '--port', str(row['port']), '--backend-id', row['backend_id'],
                                     '--journal', row['journal'], '--response-mode', row['mode']],
                                    stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True)
        time.sleep(.2)
        if proc.poll() is not None:
            raise RuntimeError('Owned fixture recovery startup failed')
        row['pid'] = proc.pid
        row['start_tick'] = Path('/proc/%d/stat' % proc.pid).read_text().split()[21]
        state_path.write_text(json.dumps(state, indent=2) + '\n')
    status, receipt = invoke(url, 'resources/read', uri='fixture://status')
    if status != 200 or receipt is None or receipt['backend_id'] != 'backend-a':
        raise AssertionError('Recovered backend-a did not serve its explicit route')
    entries = [json.loads(line) for path in journals for line in path.read_text().splitlines()]
    if receipt not in entries:
        raise AssertionError('Recovered response has no independent backend receipt')
    report.append({'case': 'single-route-backend-recovered', 'receipt': receipt})
    return {'scope': 'owned Linux fixture outage and recovery', 'checks': report}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--runtime', type=Path, required=True)
    parser.add_argument('--url', required=True)
    parser.add_argument('--rr-url', required=True)
    args = parser.parse_args()
    print(json.dumps(replay(args.runtime, args.url, args.rr_url), indent=2))
