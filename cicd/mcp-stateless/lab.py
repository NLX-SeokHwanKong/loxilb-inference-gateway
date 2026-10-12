#!/usr/bin/env python3
"""Owned single-core Docker topology only; Gateway rules remain operator-managed."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import time
import urllib.request
import uuid

HERE = Path(__file__).resolve().parent


def call(args):
    return subprocess.check_output(args, text=True).strip()


def inspect(kind, name):
    result = subprocess.run(['docker', kind, 'inspect', name], capture_output=True, text=True)
    return json.loads(result.stdout)[0] if result.returncode == 0 else None


def save(path, data):
    temporary = path.with_suffix('.tmp')
    temporary.write_text(json.dumps(data, indent=2) + '\n')
    temporary.replace(path)


def owned(kind, name, owner):
    obj = inspect(kind, name)
    if obj is None:
        return None
    labels = obj.get('Config', {}).get('Labels', {}) if kind == 'container' else obj.get('Labels', {})
    if labels.get('uc8.owner') != owner:
        raise RuntimeError('Owner mismatch; refusing mutation: ' + name)
    return obj


def remove(state):
    # Keep manifests, certs, configuration and journals for diagnosis/recovery.
    for name in reversed(state['containers']):
        if owned('container', name, state['owner']):
            call(['docker', 'rm', '-f', name])
    if owned('network', state['network'], state['owner']):
        call(['docker', 'network', 'rm', state['network']])


def ready(host, port):
    deadline = time.monotonic() + 60
    while True:
        try:
            with socket.create_connection((host, port), timeout=1):
                return
        except OSError:
            if time.monotonic() > deadline:
                raise RuntimeError('Readiness timed out: %s:%d' % (host, port))
            time.sleep(.2)


def describe(state):
    print(json.dumps({k: state[k] for k in ['owner', 'image', 'cpu', 'gateway', 'backend_a', 'backend_b', 'run_dir', 'network']}, indent=2))
    print('Management: http://' + state['gateway'] + ':11111/netlox/v1')
    print('CA: ' + state['run_dir'] + '/cert/rootCA.crt')
    print('No Gateway rules were created. Apply the operator manual next.')


def provision(args, active):
    if not args.image:
        raise RuntimeError('Supply --image or UC8_IMAGE with the qualified local image ID')
    image = call(['docker', 'image', 'inspect', args.image, '--format', '{{.Id}}'])
    call(['docker', 'run', '--rm', '--entrypoint', 'python3', image, '--version'])
    if args.cpu not in os.sched_getaffinity(0):
        raise RuntimeError('--cpu is outside the allowed CPU set')
    if not re.fullmatch(r'[a-z][a-z0-9-]{0,40}', args.prefix):
        raise RuntimeError('--prefix must be a short lowercase Docker name')
    subnet = ipaddress.ip_network(args.subnet)
    if subnet.version != 4 or subnet.prefixlen > 27:
        raise RuntimeError('Use an IPv4 subnet large enough for offsets 10,21,22')
    if active.exists():
        previous = json.loads(active.read_text())
        if not previous.get('removed'):
            raise RuntimeError('Active lab exists; run rmconfig.sh first. Prior state is retained.')
    names = [args.prefix + suffix for suffix in ['-gw', '-backend-a', '-backend-b']]
    network = args.prefix + '-net'
    if any(inspect('container', name) for name in names) or inspect('network', network):
        raise RuntimeError('Requested names already exist; choose a new prefix')
    for name in call(['docker', 'network', 'ls', '--format', '{{.ID}}']).splitlines():
        obj = inspect('network', name)
        for cfg in (obj.get('IPAM') or {}).get('Config', []) or []:
            if cfg.get('Subnet') and ipaddress.ip_network(cfg['Subnet']).version == 4 and subnet.overlaps(ipaddress.ip_network(cfg['Subnet'])):
                raise RuntimeError('Subnet overlaps Docker network ' + obj['Name'])
    for route in json.loads(call(['ip', '-j', 'route'])):
        dest = route.get('dst', '')
        if dest and dest != 'default' and ':' not in dest and subnet.overlaps(ipaddress.ip_network(dest, strict=False)):
            raise RuntimeError('Subnet overlaps host route ' + dest)
    owner = args.prefix + '-' + uuid.uuid4().hex[:12]
    run = active.parent / 'runs' / owner
    run.mkdir(parents=True, mode=0o700)
    for name in ['cert', 'config', 'journals']:
        (run / name).mkdir(mode=0o700)
    state = {'owner': owner, 'image': image, 'cpu': args.cpu, 'network': network,
             'containers': [], 'run_dir': str(run), 'gateway': str(subnet[10]),
             'backend_a': str(subnet[21]), 'backend_b': str(subnet[22]), 'removed': False}
    save(active, state)
    try:
        cert = run / 'cert'
        call(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=UC8 Lab CA', '-keyout', str(cert/'ca.key'), '-out', str(cert/'rootCA.crt')])
        call(['openssl', 'req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=UC8 Lab Gateway', '-keyout', str(cert/'server.key'), '-out', str(cert/'server.csr')])
        (cert/'server.ext').write_text('subjectAltName=IP:' + state['gateway'] + '\n')
        call(['openssl', 'x509', '-req', '-days', '2', '-in', str(cert/'server.csr'), '-CA', str(cert/'rootCA.crt'), '-CAkey', str(cert/'ca.key'), '-CAcreateserial', '-extfile', str(cert/'server.ext'), '-out', str(cert/'server.crt')])
        call(['docker', 'network', 'create', '--subnet', str(subnet), '--label', 'uc8.owner='+owner, network])
        for name, ip, port, identity, mode in [(names[1], state['backend_a'], 8081, 'backend-a', 'json'), (names[2], state['backend_b'], 8082, 'backend-b', 'sse')]:
            state['containers'].append(name); save(active, state)
            call(['docker', 'run', '-d', '--name', name, '--label', 'uc8.owner='+owner, '--cpuset-cpus', str(args.cpu), '--network', network, '--ip', ip,
                  '-v', str(HERE)+':/opt/uc8:ro', '-v', str(run/'journals')+':/evidence', '--entrypoint', 'python3', image,
                  '-B', '/opt/uc8/fixture.py', '--bind', '0.0.0.0', '--port', str(port), '--backend-id', identity, '--journal', '/evidence/'+identity+'.jsonl', '--response-mode', mode])
        state['containers'].append(names[0]); save(active, state)
        call(['docker', 'run', '-d', '--name', names[0], '--label', 'uc8.owner='+owner, '--cpuset-cpus', str(args.cpu), '--privileged', '--network', network, '--ip', state['gateway'],
              '-v', str(cert)+':/opt/loxilb/cert:ro', '-v', str(run/'config')+':/etc/loxilb', image, '-p'])
        ready(state['backend_a'], 8081); ready(state['backend_b'], 8082); ready(state['gateway'], 11111)
        json.load(urllib.request.urlopen('http://'+state['gateway']+':11111/netlox/v1/config/loadbalancer/all', timeout=5))
        save(run/'state.json', state)
        describe(state)
    except BaseException:
        remove(state); state['removed'] = True; save(active, state)
        raise


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=['config', 'remove', 'validate'])
    parser.add_argument('--state-dir', type=Path, default=Path(os.environ.get('UC8_STATE', '/tmp/uc8-mcp-lab')))
    parser.add_argument('--image', default=os.environ.get('UC8_IMAGE'))
    parser.add_argument('--prefix', default='uc8-mcp-lab')
    parser.add_argument('--subnet', default='172.30.100.0/24')
    parser.add_argument('--cpu', type=int, default=0)
    parser.add_argument('--with-mcp-rule', action='store_true', help='Validate the operator-created port2028 rule; never create rules')
    args = parser.parse_args()
    args.state_dir = args.state_dir.resolve()
    args.state_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    active = args.state_dir/'active.json'
    if args.action == 'config':
        provision(args, active); return
    state = json.loads(active.read_text())
    if args.action == 'remove':
        remove(state); state['removed'] = True; save(active, state)
        print('Removed owned topology; retained evidence/config at '+state['run_dir']); return
    if state['removed']:
        raise RuntimeError('Lab has been removed')
    os.sched_setaffinity(0, {state['cpu']})
    for name in state['containers']:
        obj = owned('container', name, state['owner'])
        if not obj or not obj['State']['Running'] or obj['Image'] != state['image'] or obj['HostConfig']['CpusetCpus'] != str(state['cpu']):
            raise RuntimeError('Runtime identity/CPU mismatch: '+name)
    ready(state['backend_a'], 8081); ready(state['backend_b'], 8082)
    version = json.load(urllib.request.urlopen('http://'+state['gateway']+':11111/netlox/v1/version', timeout=5))
    report = {'outcome':'PASS', 'scope':'owned topology readiness/identity, not MCP routing', 'cpu':state['cpu'], 'image':state['image'], 'version':version}
    if args.with_mcp_rule:
        from verify_gateway import replay
        journals = [Path(state['run_dir'])/'journals'/('backend-'+b+'.jsonl') for b in ['a','b']]
        report['traffic'] = replay('http://'+state['gateway']+':2028/mcp', journals, ['backend-a','backend-b'])
        report['scope'] = 'operator-created rule: H1 metadata/routing/receipt/rejection slice'
    save(Path(state['run_dir'])/('validation-'+uuid.uuid4().hex[:8]+'.json'), report)
    print(json.dumps(report, indent=2))


if __name__ == '__main__':
    main()
