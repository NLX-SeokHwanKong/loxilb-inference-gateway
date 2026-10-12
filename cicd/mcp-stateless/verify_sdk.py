#!/usr/bin/env python3
"""Official modern SDK client -> Gateway -> official SDK backend (JSON and SSE)."""
import argparse
import asyncio
import hashlib
import importlib.metadata
import json
from pathlib import Path
import uuid
from mcp import Client

async def verify(url,journal):
    if importlib.metadata.version('mcp')!='2.3.0': raise RuntimeError('Expected mcp==2.3.0')
    records=[]
    async with Client(url,mode='2026-07-28') as client:
        if client.protocol_version!='2026-07-28': raise AssertionError('SDK adopted wrong version')
        tools=await client.list_tools()
        if 'echo' not in [tool.name for tool in tools.tools]: raise AssertionError('SDK tools/list failed')
        for payload in ({'small':'sdk'}, {'keys':{'key%04d'%i:0 for i in range(2000)}}, {'blob':'z'*(700*1024)}):
            token=uuid.uuid4().hex
            result=await client.call_tool('echo',{'message':token,'payload':payload})
            wire=result.model_dump(by_alias=True)
            if wire.get('isError'): raise AssertionError('Independent SDK tool rejected valid input')
            receipt=wire.get('structuredContent')
            if receipt is None: receipt=json.loads(wire['content'][0]['text'])
            expected=hashlib.sha256(json.dumps(payload,sort_keys=True,ensure_ascii=False).encode()).hexdigest()
            rows=[json.loads(line) for line in journal.read_text().splitlines()]
            arrived=[row for row in rows if row.get('event')=='sdk-received' and row.get('token')==token]
            executed=[row for row in rows if row.get('event')=='sdk-executed' and row.get('token')==token]
            if len(arrived)!=1 or executed!=[receipt] or receipt.get('payload_sha256')!=expected:
                raise AssertionError('SDK response lacks unique raw arrival/execution evidence')
            records.append({'receipt':receipt,'arrival':arrived[0]})
    return {'scope':'official SDK 2.3.0 modern JSON/SSE tool slice; not OAuth/MRTR qualification',
            'mcp':importlib.metadata.version('mcp'),'protocol_version':'2026-07-28','url':url,'checks':records}

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--url',required=True);p.add_argument('--journal',type=Path,required=True)
    a=p.parse_args();print(json.dumps(asyncio.run(verify(a.url,a.journal)),indent=2))
