#!/usr/bin/env python3
"""Portable UC-7 topology. State/logs are private; Docker cleanup is owner checked."""
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import subprocess
import sys

HERE = Path(__file__).resolve().parent
SOURCE = HERE.parents[1]
STATE = Path(os.environ.get('EDGE_STATE', '/var/tmp/loxilb-edge-security')).resolve()
NAMES = ['uc7-gw', 'uc7-pg', 'uc7-ep', 'uc7-a', 'uc7-b', 'uc7-c', 'uc7-d']
NETWORK = 'uc7-security'
SUBNET = ipaddress.ip_network('172.30.97.0/24')
ENDS = {'SET-36':'01-setup','ACL-07':'02-listener-contract','XDP-06':'03-xdp','RATE-13':'04-securityrate','FW-06':'05-firewall','CONN-01':'06-connection','KEY-19':'07-key-source-rps','KV-09':'08-kv','ALL-19':'09-all-on','PROTO-09':'10-h2-sse','REC-15':'11-restore-store','ALL-29':'12-lift-peer-delete','CLI-03':'13-cli','END-09':'14-cleanup'}
TIMEOUTS = {'XDP-04','FW-04','ACL-09','ALL-10','ALL-11','ALL-27','REC-07'}


def run(*args, check=True):
    return subprocess.run(args, check=check, text=True, capture_output=True)


def inspect(kind, name):
    # Missing is distinguished from daemon failure: daemon was checked first.
    result = run('docker', kind, 'inspect', name, check=False)
    if result.returncode:
        if 'No such' in result.stderr or 'not found' in result.stderr:
            return None
        raise RuntimeError(result.stderr.strip())
    return json.loads(result.stdout)[0]


def inventory():
    return sorted(run('docker','ps','-a','--no-trunc','--format','{{.ID}} {{.Names}} {{.State}}').stdout.splitlines())


def require_host():
    if sys.platform != 'linux' or os.geteuid() != 0:
        raise RuntimeError('Run as root on a dedicated Linux Docker host (sudo -E).')
    for tool in ['docker','bash','jq','openssl','nsenter','tcpdump','ip']:
        if not shutil.which(tool):
            raise RuntimeError('Missing host tool: '+tool)
    run('docker','info')
    security = json.loads(run('docker','info','--format','{{json .SecurityOptions}}').stdout)
    if any('rootless' in option for option in security):
        raise RuntimeError('Rootless Docker cannot run this XDP/namespace topology.')


def preflight():
    for name in NAMES:
        if inspect('container', name):
            raise RuntimeError('Container name collision: '+name+'; no resource deleted.')
    if inspect('network', NETWORK):
        raise RuntimeError('Network name collision; no resource deleted.')
    for line in json.loads(run('ip','-j','route','show','table','all').stdout):
        dst = line.get('dst', 'default')
        if dst != 'default' and ipaddress.ip_network(dst, strict=False).overlaps(SUBNET):
            raise RuntimeError('Host route overlaps '+str(SUBNET)+': '+dst)
    ids = run('docker','network','ls','-q').stdout.split()
    for id in ids:
        network = inspect('network', id)
        for item in network.get('IPAM', {}).get('Config') or []:
            if item.get('Subnet') and ipaddress.ip_network(item['Subnet']).version == 4 and ipaddress.ip_network(item['Subnet']).overlaps(SUBNET):
                raise RuntimeError('Docker subnet overlaps '+str(SUBNET))
    images = json.loads((STATE/'images.json').read_text())
    if images.get('source',{}).get('scenario_commands_sha256') != hashlib.sha256((HERE/'commands.json').read_bytes()).hexdigest():
        raise RuntimeError('Scenario command source changed since image locking; rerun build.sh.')
    for key in ['gateway','client','backend','postgres']:
        identity = inspect('image', images[key]['id'])
        if not identity or identity['Id'] != images[key]['id']:
            raise RuntimeError('Missing locked image '+key+'; run build.sh first.')
    venv = STATE/'venv/bin/python'
    run(str(venv), '-c', 'import zmq,cbor2,xxhash,transformers,msgpack')
    return images


def write(path, data):
    path.write_text(json.dumps(data, indent=2)+'\n')
    path.chmod(0o600)


def execution(state, phase):
    records = json.loads((HERE/'commands.json').read_text())
    core, extra = records[:191], records[191:]
    commands = core[:36] if phase == 'config' else core[36:]
    report = Path(state['report']); lab = Path(state['lab'])
    env = dict(os.environ, EDGE_SOURCE=str(SOURCE), EDGE_LAB=str(lab), EDGE_REPORT=str(report),
               EDGE_VENV=str(STATE/'venv'), EDGE_OWNER=state['owner'],
               EDGE_GATEWAY_IMAGE=state['images']['gateway']['id'],
               PG_IMAGE=state['images']['postgres']['id'], CLIENT_IMAGE=state['images']['client']['id'],
               BACKEND_IMAGE=state['images']['backend']['id'])
    lines = ['#!/bin/bash','set +x','umask 077',
             'owned_stop() { local pid="$1" tick="$2"; test -n "$pid" && test -n "$tick" && test "$(awk \'{print $22}\' /proc/"$pid"/stat 2>/dev/null)" = "$tick" && kill "$pid" 2>/dev/null; true; }',
             'trap \'owned_stop "${PUB_PID:-}" "${PUB_TICK:-}"; owned_stop "${CAP_PID:-}" "${CAP_TICK:-}"\' EXIT']
    if phase == 'config':
        lines.append('EDGE_ADMIN_PASSWORD=$(python3 "$EDGE_SOURCE/cicd/edge-cloud-security/password.py") || exit 1')
    else:
        lines += ['export LAB="$EDGE_LAB" SRC="$EDGE_SOURCE" GW_IMAGE="$EDGE_GATEWAY_IMAGE"',
                  'export TOKEN=$(jq -er .token "$LAB/login.private.json")',
                  'test -n "$TOKEN" || exit 1']
    def emit(record, supplementary=False):
        id, command = record['id'], record['command']
        if id=='END-05':
            lines.append(shlex.quote(sys.executable)+' '+shlex.quote(str(HERE/'runner.py'))+' guard || exit 1')
        folder = report/'supplemental' if supplementary else report
        base = folder/id
        (folder/(id+'.command')).write_text(command+'\n')
        (folder/(id+'.command')).chmod(0o600)
        lines.extend([f"printf '%s\\n' {shlex.quote(id)}", '{',command,
                      '} < /dev/null > '+shlex.quote(str(base)+'.out')+' 2>&1', 'rc=$?',
                      'printf "%s\\n" "$rc" > '+shlex.quote(str(base)+'.exit')])
        allowed = [0,28] if id in TIMEOUTS else [0,7,52,55,56,28] if id=='CONN-EXCESS' else [0]
        condition = ' && '.join(f'test "$rc" != {c}' for c in allowed)
        lines.append(f'if {condition}; then printf "FAILED %s exit=%s; private output in report\\n" {shlex.quote(id)} "$rc"; exit 1; fi')
        if id=='KV-03':
            lines.append('PUB_TICK=$(awk \'{print $22}\' /proc/"$PUB_PID"/stat) || exit 1')
        if id=='ALL-02':
            lines.append('CAP_TICK=$(awk \'{print $22}\' /proc/"$CAP_PID"/stat) || exit 1')
        if id in ENDS:
            lines.append(shlex.quote(sys.executable)+' '+shlex.quote(str(HERE/'stage_gate.py'))+' '+ENDS[id]+' || exit 1')
        if id == 'CLI-03':
            for item in extra:
                emit(item, True)
            lines.append(shlex.quote(sys.executable)+' '+shlex.quote(str(HERE/'supplemental.py'))+' || exit 1')
    for record in commands:
        emit(record)
    script = report/(phase+'.private.sh')
    script.write_text('\n'.join(lines)+'\n');script.chmod(0o700)
    subprocess.run(['bash',str(script)],env=env,check=True)
    if phase == 'validation':
        subprocess.run([sys.executable,str(HERE/'score.py')],env=env,check=True)
        after = inventory()
        if after != state['before_inventory']:
            raise RuntimeError('Unrelated container inventory changed.')
        hashes = {f.stem:hashlib.sha256(f.read_bytes()).hexdigest() for f in report.glob('*.command')}
        if len(hashes)!=191:
            raise RuntimeError('Incomplete command evidence.')
        write(report/'execution-manifest.json', {'state':'PASS','core_commands':191,'supplemental_commands':6,
              'stages':14,'assertions':159,'recovery_validation':json.loads((report/'recovery-validation.json').read_text()),'executed_command_sha256':hashes,'owner':state['owner'],
              'unrelated_inventory_exact':True,'images':state['images'],'kernel':os.uname().release,
              'scenario_files_sha256':{str(f.relative_to(HERE)):hashlib.sha256(f.read_bytes()).hexdigest() for f in HERE.rglob('*') if f.is_file() and '__pycache__' not in f.parts},
              'scope':'portable core UC-7 replay; advanced fault/cookie/IPv6 matrix not included'})


def ownership_guard(state):
    # Verify ALL resources before mutating any: mixed ownership fails closed.
    found = []
    for name in NAMES:
        item = inspect('container', name)
        if item:
            if item.get('Config',{}).get('Labels',{}).get('owner') != state['owner']:
                raise RuntimeError('Refusing foreign container '+name)
            found.append(name)
    network = inspect('network', NETWORK)
    if network:
        if network.get('Labels',{}).get('owner') != state['owner']:
            raise RuntimeError('Refusing foreign network')
        allowed = {inspect('container',n)['Id'] for n in found}
        if set((network.get('Containers') or {}).keys()) - allowed:
            raise RuntimeError('Foreign endpoint attached; preserve network and containers.')
    return found, network


def cleanup(state):
    found, network = ownership_guard(state)
    for name in found:
        run('docker','rm','-f',name)
    if network:
        run('docker','network','rm',NETWORK)
    state['status']='cleaned';write(STATE/'active.json',state)
    print('Owned cleanup PASS; evidence/venv/images preserved; volumes not pruned.')


def main():
    phase = sys.argv[1]
    require_host()
    os.umask(0o077)
    STATE.mkdir(mode=0o700,parents=True,exist_ok=True)
    active = STATE/'active.json'
    if phase=='preflight':
        preflight();print('PREFLIGHT PASS; topology names/subnet free; locked images and publisher ready.');return
    if phase=='config':
        if active.exists() and json.loads(active.read_text())['status']!='cleaned':
            raise RuntimeError('Existing run record; inspect it and run rmconfig.sh before a new config.')
        images=preflight();nonce=secrets.token_hex(8)
        root=STATE/'runs'/nonce;root.mkdir(mode=0o700,parents=True)
        report=root/'report';report.mkdir(mode=0o700);(report/'supplemental').mkdir(mode=0o700)
        state={'owner':'uc7-edge-security-'+nonce,'lab':str(root/'lab'),'report':str(report),
               'images':images,'before_inventory':inventory(),'status':'config-started'}
        write(active,state)  # ownership survives a partial setup failure
        execution(state,'config');state['status']='configured';write(active,state)
        print('CONFIG PASS; topology running; next: validation.sh; private report: '+str(report))
    else:
        state=json.loads(active.read_text())
        if phase=='cleanup':cleanup(state);return
        if phase=='guard':ownership_guard(state);return
        if phase!='validation' or state['status']!='configured':
            raise RuntimeError('Validation requires a configured, not previously consumed run.')
        if state['images'].get('source',{}).get('scenario_commands_sha256') != hashlib.sha256((HERE/'commands.json').read_bytes()).hexdigest():
            raise RuntimeError('Scenario commands changed after configuration; clean and rebuild before replay.')
        owned,network=ownership_guard(state)
        if set(owned)!=set(NAMES) or not network:
            raise RuntimeError('Incomplete owned topology; clean and configure again.')
        state['status']='validation-started';write(active,state)
        execution(state,'validation');state['status']='cleaned';write(active,state)
        print('UC7 PORTABLE E2E PASS;191+6 commands/14 stages/159 assertions;owned cleanup complete.')


if __name__=='__main__':
    try:
        main()
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError, AssertionError) as exc:
        print('ERROR: '+str(exc),file=sys.stderr)
        sys.exit(1)
