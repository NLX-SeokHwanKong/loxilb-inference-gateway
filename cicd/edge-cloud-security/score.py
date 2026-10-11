import json,pathlib,re,os
r=pathlib.Path(os.environ['EDGE_REPORT']);lab=pathlib.Path(os.environ['EDGE_LAB'])
results={}
for f in r.glob('*.out'):
 s=f.read_text(errors='replace');statuses=re.findall(r'HTTP:(\d{3})',s);versions=re.findall(r'VERSION:([^\s]+)',s)
 if statuses or versions:results[f.stem]={'http':statuses,'version':versions}
checks={}
def check(name,value):checks[name]=bool(value)
def status(id,want):check(id+'_http',results.get(id,{}).get('http',[])==[str(want)])
for id in ['ACL-03','ACL-04','FW-02','KEY-07','ACL-08','KEY-19','KV-06','ALL-14','ALL-KEY-19','ALL-FLOOD-03','PROTO-03','PROTO-06','PROTO-08','ALL-25','ALL-29']:status(id,200)
status('ACL-07',400)
for id in ['KEY-06','ALL-12','PROTO-09']:status(id,401)
for id in ['REC-01','REC-04','REC-06','REC-15']:status(id,200)
status('REC-12',503)
status('REC-07','000')
check('restored_exact_fenced','REQUIRES_MIGRATION' in (r/'REC-08.out').read_text())
for id in ['XDP-04','FW-04','ACL-09','ALL-10','ALL-11','ALL-27']:
 status(id,'000');check(id+'_timeout',int((r/(id+'.exit')).read_text())==28)
for id in ['KEY-07','ACL-08','KEY-19','KV-06','ALL-14','ALL-KEY-19','ALL-FLOOD-03','ALL-25','ALL-29']:check(id+'_receipt',id in (r/(id+'.out')).read_text())
for id in ['PROTO-06','PROTO-08']:check(id+'_done','data: [DONE]' in (r/(id+'.out')).read_text());check(id+'_usage','"usage"' in (r/(id+'.out')).read_text())
check('h2_negotiated',results.get('PROTO-08',{}).get('version')==['2']);check('h2_denial',results.get('PROTO-09',{}).get('version')==['2'])
capture=(lab/'receipt.private.log').read_text(errors='replace')
journal=[]
for line in (lab/'backend-journal.jsonl').read_text(errors='replace').splitlines():
 try:
  d=json.loads(line)
  if d.get('event')=='uc7_receipt':journal.append(d)
 except ValueError:pass
for id in ['ALL-10','ALL-11','ALL-12','PROTO-09','REC-12']:check(id+'_no_backend',id not in capture)
for id in ['ALL-14','ALL-FLOOD-03','PROTO-06','PROTO-08','REC-06','REC-15']:check(id+'_journal',any(d.get('nonce')==id for d in journal))
for id in ['ALL-10','ALL-11','ALL-12','PROTO-09','REC-12']:check(id+'_no_journal',not any(d.get('nonce')==id for d in journal))
check('journal_key_stripped',bool(journal) and all(not d.get('key_present') for d in journal))
check('connection_limit_reset',int((r/'CONN-EXCESS.exit').read_text()) in [7,52,55,56])
key=json.loads((lab/'key.private.json').read_text())['raw_key']
check('key_not_forwarded',key not in capture and all(key not in (r/(id+'.out')).read_text() for id in ['KEY-07','PROTO-06','PROTO-08']))
def metric(path,prefix):
 s=path.read_text()
 if 'loxilb_' not in s:raise ValueError('Missing metric evidence')
 return sum(float(line.split()[-1]) for line in s.splitlines() if line.startswith(prefix))
deltas={}
for prefix in ['loxilb_ipfilter_blacklist_packets_total','loxilb_fw_rule_drop_packets_total','loxilb_security_conn_blocked_total','loxilb_pd_kv_tier15_hits_total','loxilb_ai_rate_limit_hits_total']:
 deltas[prefix]=metric(lab/'all-after.prom',prefix)-metric(lab/'all-before.prom',prefix);check(prefix+'_delta',deltas[prefix]>0)
for prefix,fileprefix in [('KEY','burst-'),('ALL-KEY','all-burst-')]:
 statuses=[(lab/(fileprefix+str(i)+'.txt')).read_text().strip() for i in range(6)]
 check(prefix+'_429',statuses.count('429')>0);check(prefix+'_no_lost_response',all(s in ['200','429'] for s in statuses))
for id in ['CLI-01','CLI-02','CLI-03']:check(id+'_exit',int((r/(id+'.exit')).read_text())==0)
check('cli_key_metadata',bool(json.loads((lab/'cli-key-metadata.json').read_text())))
check('cli_lb_readback',bool(json.loads((lab/'cli-lb.json').read_text())))
check('cleanup_empty',(r/'END-09.out').read_text().strip()=='')
check('rate_counter_matches_429',deltas['loxilb_ai_rate_limit_hits_total']==sum((lab/('all-burst-'+str(i)+'.txt')).read_text().strip()=='429' for i in range(6)))
for f in sorted(r.glob('[0-9][0-9]-*.json')):
 d=json.loads(f.read_text())
 for k,v in d['checks'].items():check(d['stage']+'_'+k,v)
check('all_14_stages',len(list(r.glob('[0-9][0-9]-*.json')))==14)
check('all_191_commands',len(list(r.glob('*.exit')))==191)
report={'state':'PASS' if all(checks.values()) else 'FAIL','checks':checks,'http_results':results,'metric_deltas':deltas}
(r/'score.json').write_text(json.dumps(report,indent=2));print(json.dumps({'state':report['state'],'assertions':len(checks),'failed':[k for k,v in checks.items() if not v],'metric_deltas':deltas}))

if report['state']!='PASS':raise SystemExit('Failed checks: '+str([k for k,v in checks.items() if not v]))
