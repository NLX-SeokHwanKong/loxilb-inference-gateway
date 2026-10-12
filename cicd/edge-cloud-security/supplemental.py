import json
import os
from pathlib import Path
import re
def require(value,message):
    if not value:raise SystemExit(message)

s=Path(os.environ['EDGE_REPORT'])/'supplemental'
for n in ['AGG-03','AGG-06']:
    require(re.findall(r'HTTP:(\d{3})',(s/(n+'.out')).read_text())==['200'],n+' HTTP failed')
for n in ['AGG-04','AGG-05']:
    d=json.loads((s/(n+'.out')).read_text())['securityrateAttr'][0]
    require(d['aggregateSynThreshold']==120 and d['aggregateUdpPktThreshold']==120,n+' budget mismatch')
    require(not d.get('synEnabled',False) and not d.get('udpEnabled',False),n+' source policy mismatch')
exits={f.stem:int(f.read_text()) for f in s.glob('*.exit')}
require(len(exits)==6 and set(exits.values())=={0},'Supplemental command exits failed')
print('Supplemental aggregate config/readback/delete:6 PASS')
