#!/usr/bin/env python3
"""Independent official Python SDK v2.3.0 backend, with raw ASGI receipt evidence."""
import argparse
import asyncio
import hashlib
import importlib.metadata
import json
from pathlib import Path
import uvicorn
from mcp.server.mcpserver import MCPServer
from mcp.server.transport_security import TransportSecuritySettings
from mcp_types.version import SUPPORTED_PROTOCOL_VERSIONS


def serve(args):
    if importlib.metadata.version('mcp') != '2.3.0' or '2026-07-28' not in SUPPORTED_PROTOCOL_VERSIONS:
        raise RuntimeError('Expected official mcp==2.3.0 with explicit modern support')
    journal = Path(args.journal); journal.touch(exist_ok=True)
    def record(row):
        with journal.open('a') as out:
            out.write(json.dumps(row, ensure_ascii=False) + '\n'); out.flush()
    server = MCPServer('uc8-independent-sdk')
    @server.tool()
    def echo(message: str, payload: dict | None = None) -> dict:
        digest=hashlib.sha256(json.dumps(payload, sort_keys=True, ensure_ascii=False).encode()).hexdigest()
        row={'event':'sdk-executed','token':message,'payload_sha256':digest}
        record(row); return row
    @server.tool()
    async def wait(token: str) -> str:
        record({'event':'sdk-started','token':token})
        try:
            await asyncio.sleep(30)
        except asyncio.CancelledError:
            record({'event':'sdk-cancelled','token':token}); raise
        record({'event':'sdk-finished','token':token}); return token
    @server.tool()
    def blob(size: int) -> str:
        if not 0 <= size <= 80*1024*1024: raise ValueError('Test blob bound')
        record({'event':'sdk-blob','size':size}); return 'x'*size
    app=server.streamable_http_app(stateless_http=True,json_response=args.mode=='json',
        host=args.bind,transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False))
    async def observed(scope, receive, send):
        raw=bytearray()
        async def receipt_receive():
            event=await receive()
            if event['type']=='http.request':
                raw.extend(event.get('body',b''))
                if not event.get('more_body',False):
                    try: body=json.loads(raw)
                    except ValueError: body={}
                    record({'event':'sdk-received','id':body.get('id'),'method':body.get('method'),
                        'token':body.get('params',{}).get('arguments',{}).get('message'),
                        'body_bytes':len(raw),'body_sha256':hashlib.sha256(raw).hexdigest()})
            return event
        await app(scope,receipt_receive,send)
    record({'event':'sdk-version','mcp':importlib.metadata.version('mcp'),
        'mcp_types':importlib.metadata.version('mcp-types'),'supported':list(SUPPORTED_PROTOCOL_VERSIONS),'mode':args.mode})
    uvicorn.run(observed,host=args.bind,port=args.port,log_level='warning',lifespan='on')

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--bind',required=True);p.add_argument('--port',type=int,required=True)
    p.add_argument('--journal',required=True);p.add_argument('--mode',choices=['json','sse'],required=True)
    serve(p.parse_args())
