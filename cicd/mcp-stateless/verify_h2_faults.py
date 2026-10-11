#!/usr/bin/env python3
"""Reject malformed upstream responses without fabricating a complete MCP result."""
import argparse
import json
from pathlib import Path
import subprocess
import time
import uuid
from client import headers_for, request_body
from verify_h2 import parse_payload


def replay(url, journal):
    checks = []
    for case in ('truncated-content-length', 'duplicate-content-length', 'ambiguous-cl-te',
                 'malformed-chunk', 'chunked-trailer', 'close-before-headers', 'reset-mid-body'):
        body = request_body('tools/call', uuid.uuid4().hex, name='echo', arguments={'message': case})
        command = ['curl', '--silent', '--show-error', '--http2-prior-knowledge', '--max-time', '5',
                   '--data-binary', json.dumps(body), '--write-out', '\nUC8-FAULT:%{response_code}|%{http_version}\n']
        for key, value in headers_for(body).items():
            command += ['-H', key + ': ' + value]
        command.append(url)
        started = time.monotonic()
        result = subprocess.run(command, capture_output=True, text=True, timeout=7)
        payload, trailer = result.stdout.rsplit('\nUC8-FAULT:', 1)
        status, version = trailer.strip().split('|')
        try:
            events = parse_payload(payload)
        except (ValueError, UnicodeError):
            events = []
        if any(event.get('result', {}).get('resultType') == 'complete' for event in events):
            raise AssertionError(case + ': malformed backend produced fabricated complete result')
        if result.returncode not in (0, 56, 92) or (result.returncode == 0 and int(status) not in (502, 503)):
            raise AssertionError(case + ': unexpected bridge failure contract: ' + str((result.returncode, status, result.stderr)))
        rows = [json.loads(line) for line in journal.read_text().splitlines()]
        if sum(row['request_id'] == body['id'] for row in rows) != 1:
            raise AssertionError(case + ': expected exactly one independent fault-backend arrival')
        checks.append({'case': case, 'http_status': int(status), 'http_version': version,
                       'curl_exit': result.returncode, 'elapsed_seconds': time.monotonic() - started,
                       'stderr': result.stderr.strip(), 'request_id': body['id'], 'complete_result': False})
    return {'scope': 'real HTTP/2 frontend malformed HTTP/1 backend responses', 'checks': checks}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--journal', type=Path, required=True)
    args = parser.parse_args()
    print(json.dumps(replay(args.url, args.journal), indent=2))
