#!/usr/bin/env python3
"""Isolated Linux Docker regression campaign against an exact candidate image ID."""
import argparse
import copy
import hashlib
import ipaddress
import importlib.util
import json
import os
import platform
import sys
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import uuid
from verify_api import replay as api_replay
from verify_gateway import journal_size, replay as gateway_replay
from verify_h2 import replay as h2_replay
from verify_l7 import management, policy_document, replay as l7_replay
from verify_wire import replay as wire_replay
from verify_recovery import invoke
from verify_weighted import replay as weighted_replay


class Resources:
    def __init__(self, owner):
        self.owner = owner
        self.container = None
        self.network = None
        self.processes = []

    def cleanup(self):
        errors = []
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        for kind, name in [('container', self.container), ('network', self.network)]:
            if name is None:
                continue
            try:
                data = json.loads(subprocess.check_output(['docker', kind, 'inspect', name], text=True))[0]
                labels = data.get('Config', {}).get('Labels', {}) if kind == 'container' else data.get('Labels', {})
                if labels.get('uc8.owner') != self.owner:
                    raise RuntimeError('Resource owner mismatch; refusing cleanup: ' + name)
                command = ['docker', 'rm', '-f', name] if kind == 'container' else ['docker', 'network', 'rm', name]
                subprocess.run(command, check=True, stdout=subprocess.DEVNULL)
            except Exception as error:
                errors.append(str(error))
        if errors:
            raise RuntimeError('Scoped cleanup failed: ' + '; '.join(errors))


def owned_outage(resources, url, rr_url, journals, folder):
    process = resources.processes[0]
    status, receipt = invoke(url, 'resources/read', uri='fixture://status')
    if status != 200 or receipt is None or receipt['backend_id'] != 'backend-a':
        raise AssertionError('Outage prerequisite single-backend route failed')
    process.terminate()
    process.wait(timeout=5)
    checks = []
    try:
        before = journal_size(journals)
        status, receipt = invoke(url, 'resources/read', uri='fixture://status')
        if status != 502 or receipt is not None or journal_size(journals) != before:
            raise AssertionError('Unavailable explicit route: expected502/no-arrival; actual=' + json.dumps({'status': status, 'receipt': receipt, 'journal_delta': journal_size(journals)-before}))
        checks.append({'case': 'single-endpoint-connect-refused', 'status': status, 'backend_journal_delta': 0})
        distribution = []
        for _ in range(4):
            status, receipt = invoke(rr_url, 'tools/call', name='echo', arguments={'message': 'survivor'})
            if status not in (200, 502) or (receipt is not None and receipt['backend_id'] != 'backend-b'):
                raise AssertionError('Default RR outage returned unexpected response')
            distribution.append({'status': status, 'backend_id': receipt['backend_id'] if receipt else None})
        if not any(row['status'] == 200 for row in distribution):
            raise AssertionError('Default RR failed to reach surviving backend')
        checks.append({'case': 'default-rr-survivor', 'monitoring': 'not configured; no health failover qualification',
                       'requests': distribution})
    finally:
        with (folder / 'runtime.log').open('a') as log:
            replacement = subprocess.Popen(process.args, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        resources.processes[0] = replacement
        time.sleep(.2)
        if replacement.poll() is not None:
            raise RuntimeError('Owned backend recovery failed')
    status, receipt = invoke(url, 'resources/read', uri='fixture://status')
    if status != 200 or receipt is None or receipt['backend_id'] != 'backend-a':
        raise AssertionError('Recovered explicit route failed')
    if receipt not in [json.loads(line) for journal in journals for line in journal.read_text().splitlines()]:
        raise AssertionError('Recovered response lacks independent backend receipt')
    checks.append({'case': 'single-endpoint-recovered', 'receipt': receipt})
    return {'checks': checks}


def checked(command, log):
    subprocess.run(command, check=True, stdout=log, stderr=subprocess.STDOUT)


def campaign(image, folder, subnet, require_h2, require_weighted=False, require_fc=False, require_fc_warmup=False, require_cb=False, require_sdk=False):
    if os.name != 'posix' or not Path('/proc').is_dir():
        raise RuntimeError('Runner requires a Linux Docker host')
    if not image.startswith('sha256:') or len(image) != 71:
        raise ValueError('--image must be an exact locally built candidate sha256 image ID')
    inspect = json.loads(subprocess.check_output(['docker', 'image', 'inspect', image], text=True))[0]
    if inspect['Id'] != image:
        raise RuntimeError('Candidate image identity mismatch')
    net = ipaddress.ip_network(subnet, strict=True)
    if net.version != 4 or net.prefixlen != 24:
        raise ValueError('Use a dedicated IPv4 /24 subnet')
    networks = [json.loads(line) for line in subprocess.check_output(
        ['docker', 'network', 'ls', '--format', '{{json .}}'], text=True).splitlines() if line]
    for existing in networks:
        data = json.loads(subprocess.check_output(['docker', 'network', 'inspect', existing['ID']], text=True))[0]
        for config in (data.get('IPAM', {}).get('Config') or []):
            if config.get('Subnet') and ipaddress.ip_network(config['Subnet']).version == net.version and net.overlaps(ipaddress.ip_network(config['Subnet'])):
                raise RuntimeError('Requested subnet overlaps Docker network ' + data['Name'])
    routes = json.loads(subprocess.check_output(['ip', '-json', 'route'], text=True))
    for route in routes:
        if route.get('dst', 'default') != 'default' and net.overlaps(ipaddress.ip_network(route['dst'])):
            raise RuntimeError('Requested subnet overlaps host route')
    if require_h2 and 'HTTP2' not in subprocess.check_output(['curl', '--version'], text=True):
        raise RuntimeError('Required real HTTP/2 client capability is absent')
    if require_h2 and importlib.util.find_spec('h2') is None:
        raise RuntimeError('Required concurrent HTTP/2 client package h2 is absent')
    owner = 'uc8-' + uuid.uuid4().hex[:12]
    resources = Resources(owner)
    gateway, backend = str(net.network_address + 10), str(net.network_address + 1)
    for port in ((8181, 8182, 8191) if require_h2 else (8181, 8182)) + ((8201, 8202) if require_fc or require_fc_warmup else ()) + ((8211,) if require_cb else ()) + ((8231,8232) if require_sdk else ()):
        probe = socket.socket()
        probe.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
        try:
            probe.bind(('0.0.0.0', port))
        finally:
            probe.close()
    report = {'scope': 'isolated author-candidate regression; not released image qualification',
              'fixture_sha256': {p.name: hashlib.sha256(p.read_bytes()).hexdigest() for p in Path(__file__).resolve().parent.glob('*.py')},
              'image': image, 'owner': owner, 'subnet': str(net), 'checks': {}, 'cleanup': 'pending'}
    if folder.exists() and any(folder.iterdir()):
        raise RuntimeError('Evidence directory is nonempty; preserve prior campaign evidence and use a fresh directory')
    report['client_versions'] = {'python': platform.python_version(),
                                 'curl': subprocess.check_output(['curl', '--version'], text=True).splitlines()[0]}
    if require_h2:
        import h2
        report['client_versions']['python_h2'] = h2.__version__
    folder.mkdir(parents=True, exist_ok=True)
    folder.chmod(0o700)
    report_path = folder / 'report.json'
    try:
        with (folder / 'runtime.log').open('w') as log:
            network = owner + '-net'
            resources.network = network
            checked(['docker', 'network', 'create', '--label', 'uc8.owner=' + owner, '--subnet', str(net), network], log)
            cert = folder / 'cert'; cert.mkdir(); config = folder / 'config'; config.mkdir()
            ext = cert / 'server.ext'; ext.write_text('subjectAltName=IP:' + gateway + '\n')
            checked(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '2', '-subj', '/CN=UC8 CI CA',
                     '-keyout', str(cert / 'ca.key'), '-out', str(cert / 'rootCA.crt')], log)
            checked(['openssl', 'req', '-newkey', 'rsa:2048', '-nodes', '-subj', '/CN=UC8 CI Gateway',
                     '-keyout', str(cert / 'server.key'), '-out', str(cert / 'server.csr')], log)
            checked(['openssl', 'x509', '-req', '-days', '2', '-in', str(cert / 'server.csr'), '-CA', str(cert / 'rootCA.crt'),
                     '-CAkey', str(cert / 'ca.key'), '-CAcreateserial', '-extfile', str(ext), '-out', str(cert / 'server.crt')], log)
            journals = []
            fixture = Path(__file__).resolve().parent / 'fixture.py'
            for identity, port, mode in [('backend-a', 8181, 'json'), ('backend-b', 8182, 'sse')]:
                journal = folder / (identity + '.jsonl'); journals.append(journal)
                process = subprocess.Popen(['python3', '-B', str(fixture), '--bind', backend, '--port', str(port),
                                            '--backend-id', identity, '--journal', str(journal), '--response-mode', mode],
                                           stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                resources.processes.append(process)
            if require_sdk:
                sdk_journals=[]
                for port,mode in [(8231,'json'),(8232,'sse')]:
                    journal=folder/('sdk-'+mode+'.jsonl');sdk_journals.append(journal)
                    resources.processes.append(subprocess.Popen([sys.executable,'-B',str(fixture.parent/'sdk_backend.py'),
                        '--bind',backend,'--port',str(port),'--journal',str(journal),'--mode',mode],
                        stdin=subprocess.DEVNULL,stdout=log,stderr=log))
            if require_fc or require_fc_warmup:
                fc_journals = []
                for identity, port in [('fc-backend-a', 8201), ('fc-backend-b', 8202)]:
                    journal = folder / (identity + '.jsonl'); fc_journals.append(journal)
                    process = subprocess.Popen(['python3', '-B', str(fixture), '--bind', backend, '--port', str(port),
                                                '--backend-id', identity, '--journal', str(journal),
                                                '--response-mode', 'sse', '--progress-delay', '1.5'],
                                               stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                    resources.processes.append(process)
            if require_h2:
                fault_journal = folder / 'fault-backend.jsonl'
                process = subprocess.Popen(['python3', '-B', str(fixture.parent / 'fault_backend.py'), '--bind', backend,
                                            '--port', '8191', '--journal', str(fault_journal)],
                                           stdin=subprocess.DEVNULL, stdout=log, stderr=log)
                resources.processes.append(process)
            if require_cb:
                cb_journal = folder / 'cb-backend.jsonl'
                cb_args = ['python3', '-B', str(fixture), '--bind', backend, '--port', '8211',
                           '--backend-id', 'cb-backend', '--journal', str(cb_journal), '--response-mode', 'sse']
                cb_index = len(resources.processes)
                resources.processes.append(subprocess.Popen(cb_args, stdin=subprocess.DEVNULL, stdout=log, stderr=log))
            name = owner + '-gw'
            resources.container = name
            checked(['docker', 'run', '-d', '--name', name, '--label', 'uc8.owner=' + owner, '--privileged',
                     '--network', network, '--ip', gateway, '-v', str(cert) + ':/opt/loxilb/cert:ro',
                     '-v', str(config) + ':/etc/loxilb', image, '-p'], log)
        api = 'http://' + gateway + ':11111/netlox/v1'
        document = {'serviceArguments': {'externalIP': gateway, 'port': 2028, 'protocol': 'tcp', 'mode': 4,
                                        'sel': 0, 'security': 0, 'mcp_protocol_version': '2026-07-28'},
                    'endpoints': [{'endpointIP': backend, 'targetPort': p, 'weight': 1} for p in (8181, 8182)]}
        deadline = time.monotonic() + 60
        while True:
            try:
                management(api + '/config/loadbalancer', 'POST', document)
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError('Gateway rule mutation readiness timed out')
                time.sleep(.5)
        for port, security in [(2029, 1), (2030, 0)]:
            changed = copy.deepcopy(document); changed['serviceArguments'].update(port=port, security=security)
            management(api + '/config/loadbalancer', 'POST', changed)
        if require_h2:
            fault_rule = copy.deepcopy(document)
            fault_rule['serviceArguments']['port'] = 2034
            fault_rule['endpoints'] = [{'endpointIP': backend, 'targetPort': 8191, 'weight': 1}]
            management(api + '/config/loadbalancer', 'POST', fault_rule)
            fault_rule['serviceArguments']['port'] = 2035
            fault_rule['endpoints'] = [{'endpointIP': backend, 'targetPort': 8182, 'weight': 1},
                                       {'endpointIP': backend, 'targetPort': 8191, 'weight': 1}]
            management(api + '/config/loadbalancer', 'POST', fault_rule)
            fault_detail = management(api + '/config/loadbalancer/externalipaddress/' + gateway + '/port/2035/protocol/tcp')[1]
            fault_policy = policy_document(fault_detail['serviceArguments']['id'], str(uuid.uuid4()))
            fault_policy['rules'] = [
                {'position': 1, 'matchSets': [{'conditions': [{'field': 'HEADER', 'op': 'EQUAL_TO',
                 'key': 'mcp-param-region', 'value': 'fault'}]}],
                 'action': {'kind': 'FORWARD', 'forward': {'backendRefs': [{'ep': 1, 'weight': 1}]}}},
                {'position': 99, 'matchSets': [{'conditions': [{'field': 'PATH', 'op': 'STARTS_WITH', 'value': '/'}]}], 'action': {'kind': 'FORWARD',
                 'forward': {'backendRefs': [{'ep': 0, 'weight': 1}]}}}]
            management(api + '/config/l7policy', 'POST', fault_policy)
        if require_weighted:
            for port in (2031, 2033):
                weighted = copy.deepcopy(document)
                weighted['serviceArguments'].update(port=port, sel=2)
                weighted['endpoints'][0]['weight'] = 3
                management(api + '/config/loadbalancer', 'POST', weighted)
            weighted_rule = management(api + '/config/loadbalancer/externalipaddress/' + gateway + '/port/2033/protocol/tcp')[1]
            weighted_policy = policy_document(weighted_rule['serviceArguments']['id'], str(uuid.uuid4()))
            weighted_policy['rules'][0]['action']['forward']['backendRefs'][0]['weight'] = 1
            weighted_policy['rules'][0]['action']['forward']['backendRefs'][1]['weight'] = 3
            management(api + '/config/l7policy', 'POST', weighted_policy)
            report['weighted_policy_readback'] = management(api + '/config/l7policy/id/' + weighted_policy['id'])[1]
        if require_sdk:
            for port,target in [(2048,8231),(2049,8232)]:
                sdk_rule=copy.deepcopy(document);sdk_rule['serviceArguments']['port']=port
                sdk_rule['endpoints']=[{'endpointIP':backend,'targetPort':target,'weight':1}]
                management(api+'/config/loadbalancer','POST',sdk_rule)
        report['rule_readback'] = management(api + '/config/loadbalancer/all')[1]
        backends = ['backend-a', 'backend-b']; ca = cert / 'rootCA.crt'
        report['checks']['h1'] = gateway_replay('http://' + gateway + ':2028/mcp', journals, backends)
        report['checks']['tls_h1'] = gateway_replay('https://' + gateway + ':2029/mcp', journals, backends, ca)
        report['checks']['wire'] = wire_replay('http://' + gateway + ':2028/mcp', journals)
        l7_rule = management(api + '/config/loadbalancer/externalipaddress/' + gateway + '/port/2030/protocol/tcp')[1]
        # The detail response carries the stable opaque rule identity.
        policy = policy_document(l7_rule['serviceArguments']['id'], str(uuid.uuid4()))
        management(api + '/config/l7policy', 'POST', policy)
        report['checks']['l7'] = l7_replay('http://' + gateway + ':2030/mcp', api, journals, *backends, policy)
        from verify_review import replay as review_replay
        report['checks']['review-adversarial']=review_replay('http://'+gateway+':2030/mcp',journals,require_h2)
        report['checks']['api'] = api_replay(api, document, 2032)
        report['checks']['outage'] = owned_outage(resources, 'http://' + gateway + ':2030/mcp',
                                                   'http://' + gateway + ':2028/mcp', journals, folder)
        from verify_body_integrity import replay as body_replay
        report['checks']['body-byte-preservation'] = body_replay('http://' + gateway + ':2037/mcp', api, journals, document, require_h2)
        if require_h2:
            for scheme, port in [('http', 2028), ('https', 2029)]:
                report['checks'][scheme + '_h2'] = h2_replay(scheme + '://' + gateway + ':' + str(port) + '/mcp', ca, journals, backends)
        if require_h2:
            from verify_h2_multiplex import replay as multiplex_replay
            report['checks']['h2-concurrent'] = multiplex_replay('http://' + gateway + ':2028/mcp', journals)
            report['checks']['h2-cancel-sibling'] = multiplex_replay('http://' + gateway + ':2030/mcp', journals, cancel=True)
            from verify_h2_faults import replay as faults_replay
            report['checks']['h2-upstream-faults'] = faults_replay('http://' + gateway + ':2034/mcp', fault_journal)
            report['checks']['h2-fault-sibling-isolation'] = [
                multiplex_replay('http://' + gateway + ':2035/mcp', journals, fault_case=case['case'], fault_journal=fault_journal)
                for case in report['checks']['h2-upstream-faults']['checks'] if case['case']!='reset-during-upload']
        if require_weighted:
            for port, heavy, light in [(2031, 'backend-a', 'backend-b'), (2033, 'backend-b', 'backend-a')]:
                address = 'http://' + gateway + ':' + str(port) + '/mcp'
                report['checks']['weighted-h1-%d' % port] = weighted_replay(address, journals, heavy, light)
                if require_h2:
                    report['checks']['weighted-h2-%d' % port] = weighted_replay(address, journals, heavy, light, http2=True)
        if require_fc:
            from verify_fc_route import replay as fc_replay
            fc_template = copy.deepcopy(document)
            fc_template['endpoints'] = [{'endpointIP': backend, 'targetPort': p, 'weight': 1} for p in (8201, 8202)]
            report['checks']['fc-route-isolation'] = fc_replay('http://' + gateway + ':2036/mcp', api, fc_journals, fc_template,
                                                              port=2036, backend_a='fc-backend-a', backend_b='fc-backend-b', require_h2=require_h2)
        if require_fc:
            from verify_fc_retry import replay as retry_replay
            report['checks']['fc-connect-retry'] = retry_replay('http://' + gateway + ':2042/mcp', api,
                                                               fc_journals, fc_template, port=2042)
        if require_fc_warmup:
            from verify_fc_warmup import replay as warmup_replay
            warmup_template = copy.deepcopy(document)
            warmup_template['endpoints'] = [{'endpointIP': backend, 'targetPort': p, 'weight': 1} for p in (8201, 8202)]
            report['checks']['fc-return-warmup'] = warmup_replay('http://' + gateway + ':2040/mcp', api, fc_journals,
                                                                 warmup_template, port=2040, backend_a='fc-backend-a',
                                                                 backend_b='fc-backend-b', require_h2=require_h2)
        if require_fc_warmup:
            from verify_health_scope import replay as health_replay
            report['checks']['endpoint-health-port-scope'] = health_replay('http://' + gateway + ':2046/mcp',
                api, fc_journals, warmup_template, require_h2=require_h2, port=2046)
        if require_cb:
            from verify_cb import replay as cb_replay
            def cb_control(enabled):
                process = resources.processes[cb_index]
                if not enabled:
                    if process.poll() is None:
                        process.terminate(); process.wait(timeout=5)
                elif process.poll() is not None:
                    with (folder / 'cb-fixture.log').open('a') as output:
                        replacement = subprocess.Popen(cb_args, stdin=subprocess.DEVNULL, stdout=output, stderr=output)
                    resources.processes[cb_index] = replacement
                    deadline = time.monotonic() + 5
                    while True:
                        try:
                            with socket.create_connection((backend, 8211), timeout=.2):
                                break
                        except OSError:
                            if replacement.poll() is not None or time.monotonic() >= deadline:
                                raise RuntimeError('Runner-owned CB fixture did not restore listening readiness')
                            time.sleep(.05)
            cb_template = copy.deepcopy(document)
            cb_template['endpoints'] = [{'endpointIP': backend, 'targetPort': 8211, 'weight': 1}]
            report['checks']['circuit-breaker'] = cb_replay('http://' + gateway + ':2044/mcp', api,
                [cb_journal], cb_template, cb_control,
                lambda: subprocess.check_output(['docker', 'logs', name], stderr=subprocess.STDOUT, text=True), require_h2=require_h2)
        if require_sdk:
            sdk_checks=[]
            for port,journal in zip((2048,2049),sdk_journals):
                command=[sys.executable,'-B',str(fixture.parent/'verify_sdk.py'),'--url',
                    'http://'+gateway+':'+str(port)+'/mcp','--journal',str(journal)]
                result=subprocess.run(command,capture_output=True,text=True)
                (folder/('sdk-client-'+str(port)+'.log')).write_text(result.stdout+result.stderr)
                if result.returncode: raise RuntimeError('Independent SDK failed: '+result.stderr[-2000:])
                sdk_checks.append(json.loads(result.stdout))
            report['checks']['independent-sdk']=sdk_checks
            if require_h2:
                from verify_backpressure import replay as backpressure_replay
                def gateway_logs():
                    return subprocess.check_output(['docker','logs',resources.container],stderr=subprocess.STDOUT,text=True)
                report['checks']['h2-backpressure-sdk-cancellation']=backpressure_replay(
                    'http://'+gateway+':2049/mcp',sdk_journals[1],gateway_logs)
        report['outcome'] = 'PASS'
    except Exception as error:
        report['outcome'] = 'FAIL'; report['error'] = str(error)
        raise
    finally:
        try:
            if resources.container:
                report['gateway_state_before_cleanup'] = json.loads(subprocess.check_output(
                    ['docker', 'inspect', resources.container], text=True))[0]['State']
                with (folder / 'gateway.log').open('w') as log:
                    subprocess.run(['docker', 'logs', resources.container], stdout=log, stderr=subprocess.STDOUT)
        except Exception as error:
            report['gateway_diagnostics_error'] = str(error)
        try:
            resources.cleanup(); report['cleanup'] = 'PASS'
        except Exception as error:
            report['cleanup'] = 'FAIL'; report['cleanup_error'] = str(error)
            raise
        finally:
            report_path.write_text(json.dumps(report, indent=2) + '\n')
    return report


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--image', required=True)
    parser.add_argument('--subnet', default='172.30.99.0/24')
    parser.add_argument('--evidence-dir', type=Path)
    parser.add_argument('--require-sdk',action='store_true')
    parser.add_argument('--require-h2', action='store_true')
    parser.add_argument('--require-weighted', action='store_true')
    parser.add_argument('--require-fc', action='store_true')
    parser.add_argument('--require-fc-warmup', action='store_true')
    parser.add_argument('--require-cb', action='store_true')
    args = parser.parse_args()
    evidence = args.evidence_dir or Path(tempfile.mkdtemp(prefix='uc8-mcp-evidence-'))
    print(json.dumps(campaign(args.image, evidence.resolve(), args.subnet, args.require_h2, args.require_weighted, args.require_fc, args.require_fc_warmup, args.require_cb, args.require_sdk), indent=2))
