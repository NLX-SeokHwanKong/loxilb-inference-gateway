#!/usr/bin/env python3
"""Reserved-port MCP immutable mode and mixed listener alias API checks."""
import argparse
import copy
import json
from pathlib import Path
import urllib.error
from verify_l7 import management


def request(url, method='GET', body=None):
    try:
        return management(url, method, body)
    except urllib.error.HTTPError as error:
        raw = error.read()
        return error.code, json.loads(raw) if raw else None


def replay(api, template, port):
    document = copy.deepcopy(template)
    service = document['serviceArguments']
    service['port'] = port
    service.pop('host', None)
    key = '/config/loadbalancer/externalipaddress/%s/port/%d/protocol/tcp' % (service['externalIP'], port)
    status, _ = request(api + key)
    if status != 404:
        raise RuntimeError('Reserved port already occupied; refusing mutation')
    status, result = request(api + '/config/loadbalancer', 'POST', document)
    if status not in (200, 201, 204):
        raise AssertionError('Failed to create reserved-port prerequisite: %r' % ((status, result),))
    status, baseline = request(api + key)
    if status != 200:
        raise AssertionError('Missing created rule readback')
    report = []
    for case, changed in [('immutable-mode', copy.deepcopy(document)), ('mixed-listener-alias', copy.deepcopy(document))]:
        changed['serviceArguments']['mcp_protocol_version'] = ''
        if case == 'mixed-listener-alias':
            changed['serviceArguments']['host'] = 'uc8-other.example'
        status, result = request(api + '/config/loadbalancer', 'POST', changed)
        current_status, current = request(api + key)
        if status != 400 or current_status != 200 or current != baseline:
            raise AssertionError(case + ': must reject 400 and preserve exact rule readback: ' + str((status, result)))
        report.append({'case': case, 'status': status, 'error': result, 'readback_unchanged': True})
    # Preserve this precisely scoped rule for an independently observed snapshot replay.
    return {'scope': 'reserved-port mode/alias API guards', 'port': port, 'checks': report, 'readback': baseline}


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--management-url', required=True)
    parser.add_argument('--template', type=Path, required=True)
    parser.add_argument('--port', type=int, default=2032)
    args = parser.parse_args()
    print(json.dumps(replay(args.management_url, json.loads(args.template.read_text()), args.port), indent=2))
