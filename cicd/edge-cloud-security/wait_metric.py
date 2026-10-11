"""Wait for a positive exported counter before deleting its rule (10s collector)."""
import math
import os
import subprocess
import sys
import time


def positive_counter(text, metric):
    try:
        values=[float(line.split()[-1]) for line in text.splitlines()
                if line.startswith(metric+'{') or line.startswith(metric+' ')]
    except (ValueError,IndexError):
        return False
    return bool(values) and all(math.isfinite(v) and v>=0 for v in values) and sum(values)>0


def main():
    metric=sys.argv[1]
    if metric not in ['loxilb_ipfilter_blacklist_packets_total','loxilb_fw_rule_drop_packets_total']:
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
