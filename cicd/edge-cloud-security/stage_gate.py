import json,pathlib,re,sys,os
r=pathlib.Path(os.environ['EDGE_REPORT']);lab=pathlib.Path(os.environ['EDGE_LAB']);stage=sys.argv[1];checks={}
def chk(n,v):checks[n]=bool(v)
def http(id,want):chk(id+'_http',re.findall(r'HTTP:(\d{3})',(r/(id+'.out')).read_text(errors='replace'))==[str(want)])
def metric(p,name):return sum(float(l.split()[-1]) for l in p.read_text().splitlines() if l.startswith(name))
def delta(n,a,b):chk(n+'_delta',metric(lab/a,n)>metric(lab/b,n))
if stage=='01-setup':
 http('SET-27',200);http('SET-29',200);chk('pg_ready','accepting connections' in (r/'SET-16.out').read_text());chk('management_token',bool(json.loads((lab/'login.private.json').read_text()).get('token')))
elif stage=='02-listener-contract':
 for id in ['ACL-03','ACL-04','ACL-05']:http(id,200)
 http('ACL-07',400)
elif stage=='03-xdp':
 http('XDP-04','000');chk('timeout',int((r/'XDP-04.exit').read_text())==28);chk('blacklist_counter',metric(lab/'xdp.prom','loxilb_ipfilter_blacklist_packets_total')>0)
elif stage=='04-securityrate':
 http('RATE-02',200);http('RATE-03',200);chk('metrics_collected','loxilb_' in (lab/'rate.prom').read_text())
elif stage=='05-firewall':
 http('FW-04','000');chk('timeout',int((r/'FW-04.exit').read_text())==28);chk('drop_counter',metric(lab/'firewall.prom','loxilb_fw_rule_drop_packets_total')>0)
elif stage=='06-connection':
 chk('reset',int((r/'CONN-EXCESS.exit').read_text()) in [7,52,55,56]);http('CONN-01',200)
elif stage=='07-key-source-rps':
 for id in ['KEY-07','ACL-08','KEY-19']:http(id,200);chk(id+'_receipt',id in (r/(id+'.out')).read_text())
 http('KEY-06',401);http('ACL-09','000');statuses=[(lab/f'burst-{i}.txt').read_text().strip() for i in range(6)];chk('429',statuses.count('429')>0);chk('responses',all(s in ['200','429'] for s in statuses))
elif stage=='08-kv':
 http('KV-06',200);delta('loxilb_pd_kv_tier15_hits_total','kv-after.prom','kv-before.prom')
elif stage=='09-all-on':
 for id,status in [('ALL-10','000'),('ALL-11','000'),('ALL-12',401),('ALL-14',200),('ALL-FLOOD-03',200)]:http(id,status)
 for n in ['loxilb_ipfilter_blacklist_packets_total','loxilb_fw_rule_drop_packets_total','loxilb_security_conn_blocked_total','loxilb_pd_kv_tier15_hits_total','loxilb_ai_rate_limit_hits_total']:delta(n,'all-after.prom','all-before.prom')
 cap=(lab/'receipt.private.log').read_text(errors='replace');chk('normal_receipt','ALL-14' in cap);chk('denied_absent',all(n not in cap for n in ['ALL-10','ALL-11','ALL-12']))
elif stage=='10-h2-sse':
 for id in ['PROTO-06','PROTO-08']:http(id,200);t=(r/(id+'.out')).read_text();chk(id+'_done','data: [DONE]' in t);chk(id+'_usage','"usage"' in t)
 http('PROTO-09',401);chk('h2','VERSION:2' in (r/'PROTO-08.out').read_text())
elif stage=='11-restore-store':
 for id in ['REC-01','REC-04','REC-06','REC-15']:http(id,200)
 http('REC-07','000');http('REC-12',503);chk('exact_fenced','REQUIRES_MIGRATION' in (r/'REC-08.out').read_text())
elif stage=='12-lift-peer-delete':
 http('ALL-25',200);http('ALL-27','000');http('ALL-29',200)
elif stage=='13-cli':
 raw=json.loads((lab/'key.private.json').read_text());keys=json.loads((lab/'cli-key-metadata.json').read_text());lbs=json.loads((lab/'cli-lb.json').read_text())['lbAttr']
 chk('key_handle_matches',len(keys)==1 and keys[0].get('key_id')==raw['key_id']);chk('key_policy_matches',keys[0].get('tenant_id')=='uc7-tenant' and keys[0].get('allowed_models')==['Qwen/Qwen3-0.6B'] and keys[0].get('rate_limit_rps')==1 and keys[0].get('burst_size')==1 and keys[0].get('enabled') is True)
 chk('survivor_model',len(lbs)==1 and lbs[0]['serviceArguments'].get('model_name')=='Qwen/Qwen3-0.6B');a=lbs[0]['serviceArguments'];chk('lb_gate_tuple',a.get('externalIP')=='172.30.97.10' and a.get('port')==8080 and a.get('protocol')=='tcp' and a.get('api_key_auth')=='required');chk('source_policy',set(x['prefix'] for x in lbs[0]['allowedSources'])=={'172.30.97.50/32','172.30.97.52/32','172.30.97.53/32'});chk('no_secret_values',raw['raw_key'] not in json.dumps(keys));chk('no_secret_fields',all('raw_key' not in k and 'key_hash' not in k for k in keys))
elif stage=='14-cleanup':chk('empty_owner_inventory',(r/'END-09.out').read_text().strip()=='')
report={'stage':stage,'state':'PASS' if all(checks.values()) else 'FAIL','checks':checks};(r/(stage+'.json')).write_text(json.dumps(report,indent=2));print(json.dumps(report));sys.exit(0 if report['state']=='PASS' else 1)
