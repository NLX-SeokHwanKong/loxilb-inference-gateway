#!/usr/bin/env python3
"""Independent requests on reusable HTTP/1.1 connections; TLS verification enabled."""
import argparse
import http.client
import json
import ssl
from urllib.parse import urlsplit
from fixture import VERSION, PREFIX, encode_header


def request_body(method, request_id, **params):
    params['_meta'] = {PREFIX + 'protocolVersion': VERSION,
                       PREFIX + 'clientCapabilities': {},
                       PREFIX + 'clientInfo': {'name': 'uc8-wire-client', 'version': '1.0'}}
    return {'jsonrpc': '2.0', 'id': request_id, 'method': method, 'params': params}


def headers_for(body):
    headers = {'Content-Type': 'application/json', 'Accept': 'application/json, text/event-stream',
               'MCP-Protocol-Version': body['params']['_meta'][PREFIX + 'protocolVersion'],
               'Mcp-Method': body['method']}
    params = body['params']
    if body['method'] in ('tools/call', 'prompts/get', 'resources/read'):
        headers['Mcp-Name'] = encode_header(params.get('name', params.get('uri')))
    if body['method'] == 'tools/call' and params.get('name') == 'echo':
        region = params.get('arguments', {}).get('region')
        if region is not None:
            headers['Mcp-Param-Region'] = encode_header(region)
    return headers


def messages(response):
    """Yield events as they arrive; ignore comments and assemble multiline SSE data."""
    media = response.getheader('Content-Type', '').split(';')[0]
    if media == 'application/json':
        yield json.loads(response.read())
        return
    if media != 'text/event-stream':
        raise ValueError('Unexpected response media type: ' + media)
    data = []
    while True:
        raw = response.readline()
        if not raw:
            if data:
                raise ValueError('Incomplete SSE event')
            return
        line = raw.decode('utf-8').rstrip('\r\n')
        if not line:
            if data:
                yield json.loads('\n'.join(data))
                data = []
        elif line.startswith('data:'):
            value = line[5:]
            data.append(value[1:] if value.startswith(' ') else value)


def connection(url, ca=None):
    parsed = urlsplit(url)
    if parsed.scheme not in ('http', 'https') or not parsed.hostname or parsed.username:
        raise ValueError('Explicit http(s) URL required')
    if parsed.scheme == 'https':
        conn = http.client.HTTPSConnection(parsed.hostname, parsed.port,
            context=ssl.create_default_context(cafile=ca), timeout=10)
    else:
        conn = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=10)
    return conn, parsed.path or '/mcp'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True)
    parser.add_argument('--ca')
    parser.add_argument('--requests', type=int, default=10)
    parser.add_argument('--new-connection', action='store_true')
    parser.add_argument('--progress', action='store_true')
    args = parser.parse_args()
    if args.requests < 1:
        parser.error('--requests must be positive')
    conn, path = connection(args.url, args.ca)
    try:
        for index in range(args.requests):
            body = request_body('tools/call', index, name='echo',
                                arguments={'message': 'request-%d' % index, 'region': 'edge-a'})
            if args.progress:
                body['params']['_meta']['progressToken'] = 'progress-%d' % index
            conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
            response = conn.getresponse()
            status = response.status
            events = list(messages(response))
            final = [event for event in events if 'id' in event]
            if status != 200 or len(final) != 1 or final[0]['id'] != index or 'result' not in final[0]:
                raise ValueError('Request failed: ' + json.dumps({'status': status, 'events': events}))
            if final[0]['result'].get('resultType') != 'complete':
                raise ValueError('Missing complete resultType')
            print(json.dumps({'status': status, 'events': events}, ensure_ascii=False), flush=True)
            if args.new_connection:
                conn.close()
                conn, path = connection(args.url, args.ca)
    finally:
        conn.close()


if __name__ == '__main__':
    main()
