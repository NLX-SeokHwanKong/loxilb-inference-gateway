#!/usr/bin/env python3
"""Independent request/receipt helper for scoped outage verifiers."""
import json
import uuid
from client import connection, headers_for, messages, request_body


def invoke(url, method, **params):
    body = request_body(method, uuid.uuid4().hex, **params)
    conn, path = connection(url)
    try:
        conn.request('POST', path, json.dumps(body).encode(), headers_for(body))
        response = conn.getresponse()
        events = list(messages(response))
        receipt = next((event['result']['_meta']['com.loxilb.fixture/receipt']
                        for event in events if 'result' in event), None)
        return response.status, receipt
    finally:
        conn.close()
