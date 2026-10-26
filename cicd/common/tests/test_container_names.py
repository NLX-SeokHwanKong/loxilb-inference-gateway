#!/usr/bin/env python3
"""Deterministic Docker name/ID collision regression; no Docker daemon needed."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

COMMON = Path(__file__).resolve().parents[2] / 'common.sh'
GATEWAY_ID = 'c1' + '0' * 62
STALE_ID = 'a' * 64
SIDECAR_ID = 'b' * 64
DOCKER = '''#!/usr/bin/env python3
import json,os,sys
from pathlib import Path
p=Path(os.environ['DOCKER_FIXTURE']);state=json.loads(p.read_text());a=sys.argv[1:];state['calls'].append(a)
def save():p.write_text(json.dumps(state))
def resolve(q):
 if q in state['containers']:return q
 matches=[n for n,i in state['containers'].items()if i.startswith(q)]
 return matches[0]if len(matches)==1 else None
if a[:2]==['container','inspect'] or a[0]=='inspect':
 n=resolve(a[-1]);save()
 if n is None:sys.exit(1)
 print('/'+n+' '+state['containers'][n]);sys.exit(0)
if a[0]=='rm':
 n=resolve(a[-1])
 if n is not None:del state['containers'][n]
elif a[0]=='run':
 n=a[a.index('--name')+1];state['containers'][n]='d'*64;print('d'*64)
elif a[0]not in ['kill','stop']:raise RuntimeError('unexpected Docker command '+str(a))
save()
'''

class ContainerNameTests(unittest.TestCase):
    def run_case(self, command, extra=None):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / 'loxilb.io').mkdir()  # source common.sh without certificate bootstrapping
            state = {'containers': {'llb1': GATEWAY_ID, **(extra or {})}, 'calls': []}
            (root / 'state.json').write_text(json.dumps(state))
            (root / 'docker').write_text(DOCKER)
            (root / 'docker').chmod(0o700)
            script = '''lsb_release(){ echo 22.04; }
source "$COMMON"
sudo(){ :; }
sleep(){ :; }
get_docker_pid(){ pid=123; }
probe_loxilb_rest(){ :; }
hexec=true
hns=true
hexist="$FIXTURE_ROOT/netns"
''' + command
            result = subprocess.run(['bash', '-c', script], env={**os.environ, 'PATH': d + os.pathsep + os.environ['PATH'], 'COMMON': str(COMMON), 'FIXTURE_ROOT': d, 'DOCKER_FIXTURE': str(root / 'state.json')}, text=True, capture_output=True, cwd=root)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            return json.loads((root / 'state.json').read_text())

    def test_spawn_missing_hex_name_does_not_remove_id_prefix_match(self):
        s = self.run_case('spawn_docker_host --dock-type host --dock-name c1')
        self.assertEqual(s['containers']['llb1'], GATEWAY_ID)
        self.assertIn('c1', s['containers'])
        self.assertFalse(any(c[0] == 'rm' for c in s['calls']))

    def test_spawn_exact_stale_name_removes_only_its_full_id(self):
        s = self.run_case('spawn_docker_host --dock-type host --dock-name c1', {'c1': STALE_ID})
        self.assertEqual(s['containers']['llb1'], GATEWAY_ID)
        self.assertIn(['rm', '-f', STALE_ID], s['calls'])
        self.assertEqual(s['containers']['c1'], 'd' * 64)

    def test_delete_missing_hex_name_preserves_prefix_match(self):
        s = self.run_case('delete_docker_host c1')
        self.assertEqual(s['containers'], {'llb1': GATEWAY_ID})
        self.assertFalse(any(c[0] in ('rm', 'kill', 'stop') for c in s['calls']))

    def test_delete_exact_name_and_sidecar_use_full_ids(self):
        s = self.run_case('delete_docker_host c1', {'c1': STALE_ID, 'ka_c1': SIDECAR_ID})
        self.assertEqual(s['containers'], {'llb1': GATEWAY_ID})
        self.assertIn(['kill', STALE_ID], s['calls'])
        self.assertIn(['rm', STALE_ID], s['calls'])
        self.assertIn(['kill', SIDECAR_ID], s['calls'])
        self.assertIn(['rm', SIDECAR_ID], s['calls'])

    def test_exact_gateway_delete_stops_instead_of_killing(self):
        s = self.run_case('delete_docker_host llb1')
        self.assertEqual(s['containers'], {})
        self.assertIn(['stop', GATEWAY_ID], s['calls'])

if __name__ == '__main__':
    unittest.main()
