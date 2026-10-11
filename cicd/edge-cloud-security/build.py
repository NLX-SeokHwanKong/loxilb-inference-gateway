#!/usr/bin/env python3
"""Build portable fixture images, then resolve every tag to an immutable image ID."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

HERE=Path(__file__).resolve().parent
SOURCE=HERE.parents[1]
STATE=Path(os.environ.get('EDGE_STATE','/var/tmp/loxilb-edge-security')).resolve()


def execute(args):
    subprocess.run(args,check=True)


def main():
    if sys.platform!='linux' or os.geteuid()!=0:
        raise RuntimeError('Build on a Linux Docker host as root (sudo -E).')
    os.umask(0o077)
    STATE.mkdir(mode=0o700,parents=True,exist_ok=True)
    if (STATE/'active.json').exists() and json.loads((STATE/'active.json').read_text())['status']!='cleaned':
        raise RuntimeError('Finish/clean the active run before replacing image locks.')
    if not (SOURCE/'loxilb-ebpf/kernel/llb_xdp_main.c').is_file():
        raise RuntimeError('Missing eBPF submodule; git submodule update --init --recursive')
    execute(['docker','info'])
    # Install only into an isolated venv; never pip --break-system-packages.
    if not (STATE/'venv/bin/python').exists():
        execute(['python3','-m','venv',str(STATE/'venv')])
    pip_probe=subprocess.run([str(STATE/'venv/bin/python'),'-m','pip','--version'],capture_output=True)
    if pip_probe.returncode:
        raise RuntimeError('Incomplete private venv (pip missing). Install python3-venv and recreate only '+str(STATE/'venv'))
    execute([str(STATE/'venv/bin/python'),'-m','pip','install','-r',str(HERE/'requirements.txt')])
    frozen=subprocess.check_output([str(STATE/'venv/bin/python'),'-m','pip','freeze'],text=True)
    (STATE/'publisher-dependencies.txt').write_text(frozen)
    gateway=os.environ.get('EDGE_GATEWAY_IMAGE','')
    if not gateway:
        gateway='edge-security-gateway:local'
        execute(['docker','build','--build-arg','VERSION=edge-security-local','-t',gateway,str(SOURCE)])
    client=os.environ.get('EDGE_CLIENT_IMAGE','')
    if not client:
        client='edge-security-client:local'
        execute(['docker','build','-f',str(HERE/'Dockerfile.client'),'-t',client,str(HERE)])
    backend=os.environ.get('EDGE_BACKEND_IMAGE','')
    if not backend:
        backend='edge-security-backend:local'
        execute(['docker','build','-f',str(HERE/'backend/Dockerfile'),'-t',backend,str(SOURCE)])
    postgres=os.environ.get('EDGE_POSTGRES_IMAGE','postgres:16-bookworm')
    # An explicit existing digest/ID is supported offline; default tag is pulled then locked.
    if not os.environ.get('EDGE_POSTGRES_IMAGE'):
        execute(['docker','pull',postgres])
    identities={}
    for key,ref in [('gateway',gateway),('client',client),('backend',backend),('postgres',postgres)]:
        d=json.loads(subprocess.check_output(['docker','image','inspect',ref],text=True))[0]
        identities[key]={'reference':ref,'id':d['Id'],'repo_digests':d.get('RepoDigests') or [],'architecture':d['Architecture']}
    revision=subprocess.run(['git','-C',str(SOURCE),'rev-parse','HEAD'],capture_output=True,text=True)
    identities['source']={'git_head':revision.stdout.strip() if revision.returncode==0 else 'unavailable; exported source',
        'scenario_commands_sha256':hashlib.sha256((HERE/'commands.json').read_bytes()).hexdigest(),
        'dockerfile_sha256':hashlib.sha256((SOURCE/'Dockerfile').read_bytes()).hexdigest(),
        'publisher_dependencies_sha256':hashlib.sha256(frozen.encode()).hexdigest()}
    (STATE/'images.json').write_text(json.dumps(identities,indent=2)+'\n')
    print('BUILD PASS; image IDs locked in '+str(STATE/'images.json'))


if __name__=='__main__':
    try:
        main()
    except (OSError,RuntimeError,subprocess.CalledProcessError) as exc:
        print('ERROR: '+str(exc),file=sys.stderr)
        sys.exit(1)
