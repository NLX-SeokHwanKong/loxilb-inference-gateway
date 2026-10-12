#!/usr/bin/env python3
"""Review regressions: unrelated long headers, valid wide/large bodies, denial identity."""
import hashlib
import json
import uuid
from client import connection, headers_for, messages, request_body
from verify_h2 import curl_requests, parse_payload


def replay(url,journals,require_h2=False):
    checks=[]
    for transport in ('h1','h2') if require_h2 else ('h1',):
        for case,arguments in [('wide-valid-object',{'message':'wide','keys':{'key%04d'%i:0 for i in range(2000)}}),
                               ('large-valid-body',{'message':'large','blob':'z'*(700*1024)}),
                               ('long-unrelated-headers',{'message':'long-headers'})]:
            body=request_body('tools/call',uuid.uuid4().hex,name='echo',arguments=arguments)
            raw=json.dumps(body).encode(); expected=hashlib.sha256(raw).hexdigest()
            headers=headers_for(body)
            if case=='long-unrelated-headers':
                headers.update({'Authorization':'Bearer '+'a'*2048,'Cookie':'cookie='+'b'*30000})
                headers.update({'X-Unrelated-%02d'%i:'value' for i in range(60)})
            if transport=='h1':
                conn,path=connection(url)
                try:
                    conn.request('POST',path,raw,headers);response=conn.getresponse()
                    status=response.status;events=list(messages(response)) if status==200 else []
                finally: conn.close()
            else:
                extra=[]
                for key,value in headers.items():
                    if key not in headers_for(body): extra.extend(['-H',key+': '+value])
                response=curl_requests(url,None,[body],extra)[0]
                status=response['status'];events=parse_payload(response['payload']) if status==200 else []
            if status!=200: raise AssertionError(case+' '+transport+': expected 200, got '+str(status))
            final=[event for event in events if event.get('id')==body['id']]
            if len(final)!=1: raise AssertionError('Missing matching final response')
            rows=[json.loads(line) for journal in journals for line in journal.read_text().splitlines()]
            arrived=[row for row in rows if row.get('request_id')==body['id'] and row.get('event')=='received']
            executed=[row for row in rows if row.get('request_id')==body['id'] and row.get('event')=='executed']
            receipt=final[0]['result']['_meta']['com.loxilb.fixture/receipt']
            if len(arrived)!=1 or executed!=[receipt] or arrived[0]['body_sha256']!=expected:
                raise AssertionError(case+': raw-body receipt/unique execution mismatch')
            checks.append({'case':case,'transport':transport,'status':status,'body_bytes':len(raw),
                'body_sha256':expected,'arrival':arrived[0],'receipt':receipt})
        body=request_body('tools/call',uuid.uuid4().hex,name='echo',arguments={'message':'denial-id'})
        headers=headers_for(body);headers['MCP-Protocol-Version']='2026-11-25'
        before=sum(len(j.read_text().splitlines()) for j in journals)
        if transport=='h1':
            conn,path=connection(url)
            try:
                conn.request('POST',path,json.dumps(body).encode(),headers);response=conn.getresponse()
                status=response.status;denial=json.loads(response.read())
            finally: conn.close()
        else:
            body['params']['_meta']['io.modelcontextprotocol/protocolVersion']='2026-11-25'
            response=curl_requests(url,None,[body])[0]
            status=response['status'];denial=json.loads(response['payload'])
        if status!=400 or denial.get('id')!=body['id'] or denial.get('error',{}).get('code')!=-32022 or denial['error'].get('data',{}).get('supported')!=['2026-07-28']:
            raise AssertionError('Unsupported-version denial contract mismatch ('+transport+'): '+repr(denial))
        if sum(len(j.read_text().splitlines()) for j in journals)!=before:
            raise AssertionError('Refused future-version request reached backend')
        checks.append({'case':'future-version-denial','transport':transport,'status':status,'denial':denial,'arrival_delta':0})
    return {'scope':'actual L7 policy with wide/large requests and unrelated long headers','checks':checks}
