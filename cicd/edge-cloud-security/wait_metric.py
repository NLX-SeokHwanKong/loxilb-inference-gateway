"""Wait for a positive exported counter before deleting its rule (10s collector)."""
import math
import os
import subprocess
import sys
import time


def metric_sum(text, metric):
    import re
    pattern=re.compile(r'^'+re.escape(metric)+r'(?:\{.*\})?\s+(\S+)(?:\s+\d+)?\s*$')
    values=[]
    for line in text.splitlines():
        if not (line.startswith(metric+'{') or line.startswith(metric+' ')):
            continue
        match=pattern.fullmatch(line)
        if not match:
            raise ValueError('Malformed expected metric '+metric)
        value=float(match.group(1))
        if not math.isfinite(value) or value<0:
            raise ValueError('Invalid counter sample '+metric)
        values.append(value)
    total=sum(values)
    if not values or not math.isfinite(total):
        raise ValueError('Missing or overflowing counter '+metric)
    return total


def positive_counter(text, metric):
    try:
        return metric_sum(text,metric)>0
    except ValueError:
        return False


def main():
    metric=sys.argv[1]
    if metric not in ['loxilb_ipfilter_blacklist_packets_total','loxilb_fw_rule_drop_packets_total','loxilb_pd_kv_tier15_hits_total']:
        raise SystemExit('Unsupported counter')
    deadline=time.monotonic()+30
    while True:
        p=subprocess.run(['docker','exec','uc7-gw','curl','-fsS','--max-time','3','-H',
                          'Authorization: Bearer '+os.environ['TOKEN'],
                          'http://127.0.0.1:11111/netlox/v1/metrics'],capture_output=True,text=True)
        if p.returncode==0 and positive_counter(p.stdout,metric):
            print(metric+' positive sample received; traffic/counter gate still required')
            return
        if time.monotonic()>=deadline:
            raise SystemExit('FAIL: '+metric+' has no positive valid sample within 30s')
        time.sleep(1)


if __name__=='__main__':
    main()
