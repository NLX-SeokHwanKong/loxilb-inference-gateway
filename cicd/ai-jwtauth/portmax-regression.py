#!/usr/bin/env python3
"""Run after config.sh: singleton portMax must preserve H1/H2 TLS/JWT delivery.

Needs root, curl with HTTP/2, and the original ai-jwtauth namespaces. Uses only
its own three ports (override with --port-base) and cleans its rules on failure.
"""
import argparse
import json
import pathlib
import re
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--port-base', type=int, default=2290)
args = parser.parse_args()
if not 1 <= args.port_base <= 65533:
    parser.error('port-base must leave room for three valid ports')
scenario = pathlib.Path(__file__).resolve().parent
issuer = re.search(r"^KC_ISSUER='([^']+)'", (scenario / '.state').read_text(), re.M).group(1)


def api(method, route, body=None):
    cmd = ['ip', 'netns', 'exec', 'llb1', 'curl', '-sS', '--max-time', '10',
           '-X', method, 'http://127.0.0.1:11111/netlox/v1' + route,
           '-w', '\n%{http_code}']
    if body is not None:
        cmd += ['-H', 'Content-Type: application/json', '--data-binary', '@-']
    p = subprocess.run(cmd, input=json.dumps(body) if body is not None else None,
                       capture_output=True, text=True, check=True)
    data, _, code = p.stdout.rpartition('\n')
    return int(code), json.loads(data) if data else None


def rules():
    code, data = api('GET', '/config/loadbalancer/all')
    assert code == 200
    return data['lbAttr']


def receipt(nonce, h2):
    port = 8090 if h2 else 8080
    p = subprocess.run(['ip', 'netns', 'exec', 'l3ep1', 'curl', '-fsS',
                        *(['--http2-prior-knowledge'] if h2 else []),
                        '--max-time', '5', f'http://31.31.31.1:{port}/__receipts/' + nonce],
                       capture_output=True, text=True, check=True)
    return json.loads(p.stdout)


before = rules()
assert not any(args.port_base <= r['serviceArguments']['port'] <= args.port_base + 2
               for r in before), 'Test ports already occupied; choose another --port-base'
names = {'portmax-regression-' + str(args.port_base + i) for i in range(3)}
assert not any(r['serviceArguments'].get('name') in names for r in before), 'Test name already owned'
results = []
with tempfile.TemporaryDirectory(prefix='portmax-jwt-') as temp:
    auth = pathlib.Path(temp) / 'alice.curl'
    for offset, (tls, h2) in enumerate([(False, False), (True, False), (True, True)]):
        port = args.port_base + offset
        for upper in [None, port, 0, port]:
            name = 'portmax-regression-' + str(port)
            sa = dict(name=name, externalIP='10.10.10.254', port=port, protocol='tcp',
                      mode=4, security=int(tls), host='10.10.10.254', path_prefix='/',
                      path_match_mode='prefix', model_name='llama-70b',
                      api_key_auth='jwt', jwt_auth_profile='kc', backend_protocol='http2' if h2 else 'http1')
            if upper is not None:
                sa['portMax'] = upper
            created = False
            try:
                code, data = api('POST', '/config/loadbalancer', {
                    'serviceArguments': sa,
                    'endpoints': [dict(endpointIP='31.31.31.1', targetPort=8090 if h2 else 8080, weight=1)]})
                assert code == 200 and data.get('result') == 'Success', (code, data)
                created = True
                saved = next(r for r in rules() if r['serviceArguments'].get('name') == name)
                assert saved['serviceArguments'].get('portMax', 0) == 0
                _, fw = api('GET', '/config/firewall/all')
                assert not any(x['ruleArguments'].get('minDestinationPort') == port
                               for x in (fw.get('fwAttr') or [])), 'Spurious range firewall'
                form = urllib.parse.urlencode(dict(client_id='aigw-client', grant_type='password',
                                                   username='alice', password='alicepw')).encode()
                with urllib.request.urlopen(issuer + '/protocol/openid-connect/token',
                                            data=form, timeout=10) as response:
                    token = json.load(response)['access_token']
                auth.write_text('header = "Authorization: Bearer ' + token + '"\n')
                auth.chmod(0o600)
                for authenticated in [True, False]:
                    nonce = 'portmax-' + str(time.time_ns())
                    cmd = ['ip', 'netns', 'exec', 'l3h1', 'curl', '-sS',
                           '--http2' if h2 else '--http1.1', '--connect-timeout', '3',
                           '--max-time', '10', '-H', 'Content-Type: application/json',
                           '-H', 'X-Test-Nonce: ' + nonce, '--data-binary',
                           '{"model":"llama-70b","messages":[{"role":"user","content":"hi"}]}',
                           '-o', '/dev/null', '-w', '%{http_code} %{http_version}',
                           ('https' if tls else 'http') + f'://10.10.10.254:{port}/v1/chat/completions']
                    if tls:
                        cmd += ['--cacert', str(scenario / 'cert/server.crt')]
                    if authenticated:
                        cmd += ['--config', str(auth)]
                    p = subprocess.run(cmd, capture_output=True, text=True, check=True)
                    code, version = p.stdout.split()
                    assert int(code) == (200 if authenticated else 401), p.stdout
                    if h2:
                        assert version == '2', p.stdout
                    assert receipt(nonce, h2) == int(authenticated)
                    results.append(dict(tls=tls, http2=h2, upper=upper,
                                        authenticated=authenticated, http=int(code)))
            finally:
                if created:
                    code, _ = api('DELETE', '/config/loadbalancer/name/' + name)
                    assert code == 200
                    assert not any(r['serviceArguments'].get('name') == name for r in rules())
    def policy(rows):
        rows = json.loads(json.dumps(rows))
        for row in rows:
            for ep in row['endpoints']:
                ep.pop('counter', None)
        return sorted(rows, key=lambda row: row['serviceArguments']['id'])
    assert policy(rules()) == policy(before), 'Original rule policy changed'
print(json.dumps({'status': 'PASS', 'trafficChecks': len(results), 'cases': results}))
