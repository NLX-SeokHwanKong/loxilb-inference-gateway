#!/usr/bin/env python3
"""Require persistent DB state and a fresh-cache-miss rejection after restart."""
import json
import os
from pathlib import Path
import subprocess


def main():
    sql = "SELECT count(*) FROM pg_roles WHERE rolname IN ('aigwuser','aigw_mgmt_user'); SELECT count(*) FROM pg_namespace WHERE nspname IN ('aigw','aigw_mgmt');"
    result = subprocess.run(['docker','exec','uc7-pg','psql','-h','127.0.0.1','-U','uc7owner','-d','uc7','-At','-c',sql],capture_output=True,text=True)
    counts = result.stdout.splitlines()
    lab = Path(os.environ['EDGE_LAB'])
    request = subprocess.run(['docker','exec','uc7-a','curl','-sS','--max-time','4','-o','/dev/null','-w','%{http_code}', '-H','X-Api-Key: '+os.environ['OUTAGE_KEY'], '-H','X-Edge-Nonce: REC-FRESH-KEY','-H','Content-Type: application/json','--data-binary',(lab/'request.json').read_text(),'http://172.30.97.10:8080/v1/completions'],capture_output=True,text=True)
    logs = subprocess.run(['docker','logs','uc7-ep'],capture_output=True,text=True)
    checks = {'roles_survive_restart':result.returncode == 0 and counts[:1] == ['2'], 'schemas_survive_restart':result.returncode == 0 and counts[1:2] == ['2'], 'fresh_key_rejected_without_backend_receipt':request.returncode == 0 and request.stdout == '401' and logs.returncode == 0 and 'REC-FRESH-KEY' not in logs.stdout+logs.stderr}
    proof = {'state':'PASS' if all(checks.values()) else 'FAIL','checks':checks,'fresh_key_http':request.stdout}
    (Path(os.environ['EDGE_REPORT'])/'recovery-validation.json').write_text(json.dumps(proof,indent=2)+'\n')
    print(json.dumps(proof))
    if not all(checks.values()):
        raise SystemExit(1)


if __name__ == '__main__':
    main()
