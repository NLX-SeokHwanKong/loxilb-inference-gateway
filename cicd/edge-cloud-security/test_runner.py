"""Destructive-boundary and partial-preflight regressions; no Docker daemon needed."""
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import runner
from wait_metric import positive_counter, metric_sum
from password import generate_password
import re


class SafetyTests(unittest.TestCase):
    def test_name_collision_does_not_delete(self):
        with patch.object(runner,'inspect',return_value={'Id':'foreign'}), patch.object(runner,'run') as call:
            with self.assertRaisesRegex(RuntimeError,'collision'):
                runner.preflight()
            call.assert_not_called()

    def test_network_collision_does_not_delete(self):
        with patch.object(runner,'inspect',side_effect=[None]*7+[{'Id':'foreign'}]),patch.object(runner,'run') as call:
            with self.assertRaisesRegex(RuntimeError,'collision'):
                runner.preflight()
            call.assert_not_called()

    def test_mixed_container_ownership_aborts_all_cleanup(self):
        data=[{'Id':'owned','Config':{'Labels':{'owner':'mine'}}},{'Id':'foreign','Config':{'Labels':{'owner':'other'}}}]
        with patch.object(runner,'inspect',side_effect=data),patch.object(runner,'run') as call:
            with self.assertRaisesRegex(RuntimeError,'foreign container'):
                runner.cleanup({'owner':'mine'})
            call.assert_not_called()

    def test_foreign_network_aborts_all_cleanup(self):
        with patch.object(runner,'inspect',side_effect=[None]*7+[{'Labels':{'owner':'other'}}]),patch.object(runner,'run') as call:
            with self.assertRaisesRegex(RuntimeError,'foreign network'):
                runner.cleanup({'owner':'mine'})
            call.assert_not_called()

    def test_foreign_endpoint_aborts_all_cleanup(self):
        items=[{'Id':'owned','Config':{'Labels':{'owner':'mine'}}}]+[None]*6+[{'Labels':{'owner':'mine'},'Containers':{'foreign':{}}}]+[{'Id':'owned'}]
        with patch.object(runner,'inspect',side_effect=items),patch.object(runner,'run') as call:
            with self.assertRaisesRegex(RuntimeError,'Foreign endpoint'):
                runner.cleanup({'owner':'mine'})
            call.assert_not_called()

    def test_absent_cleanup_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d,patch.object(runner,'STATE',Path(d)),patch.object(runner,'inspect',return_value=None),patch.object(runner,'run') as call:
            runner.cleanup({'owner':'mine'})
            call.assert_not_called()
            self.assertEqual(json.loads((Path(d)/'active.json').read_text())['status'],'cleaned')

    def test_host_route_overlap_rejected_before_build_lock(self):
        from subprocess import CompletedProcess
        routes=CompletedProcess([],0,json.dumps([{'dst':'172.30.97.50/32'}]),'')
        with patch.object(runner,'inspect',return_value=None),patch.object(runner,'run',return_value=routes):
            with self.assertRaisesRegex(RuntimeError,'overlaps'):
                runner.preflight()


    def test_changed_command_source_rejected_before_any_image_use(self):
        from subprocess import CompletedProcess
        with tempfile.TemporaryDirectory() as d:
            base=Path(d)
            (base/'commands.json').write_text('[]')
            (base/'images.json').write_text(json.dumps({'source':{'scenario_commands_sha256':'stale'}}))
            outputs=[CompletedProcess([],0,'[]',''),CompletedProcess([],0,'','')]
            with patch.object(runner,'STATE',base),patch.object(runner,'HERE',base),patch.object(runner,'inspect',return_value=None),patch.object(runner,'run',side_effect=outputs):
                with self.assertRaisesRegex(RuntimeError,'source changed'):
                    runner.preflight()

    def test_changed_commands_after_configuration_preserve_resources(self):
        with tempfile.TemporaryDirectory() as d:
            base=Path(d)
            (base/'commands.json').write_text('[]')
            (base/'active.json').write_text(json.dumps({'status':'configured','images':{'source':{'scenario_commands_sha256':'stale'}}}))
            with patch.object(runner,'STATE',base),patch.object(runner,'HERE',base),patch.object(runner,'require_host'),patch.object(runner.sys,'argv',['runner.py','validation']),patch.object(runner,'ownership_guard') as guard:
                with self.assertRaisesRegex(RuntimeError,'changed after configuration'):
                    runner.main()
                guard.assert_not_called()
                self.assertEqual(json.loads((base/'active.json').read_text())['status'],'configured')

class OracleTests(unittest.TestCase):
    def test_optimized_python_rejects_nonzero_supplemental_exit(self):
        import os,subprocess,sys
        with tempfile.TemporaryDirectory() as d:
            folder=Path(d)/'supplemental';folder.mkdir()
            for n in ['AGG-03','AGG-06']:(folder/(n+'.out')).write_text('HTTP:200')
            for n in ['AGG-04','AGG-05']:(folder/(n+'.out')).write_text(json.dumps({'securityrateAttr':[{'aggregateSynThreshold':120,'aggregateUdpPktThreshold':120}]}))
            for i in range(1,7):(folder/('AGG-0'+str(i)+'.exit')).write_text('1' if i==1 else '0')
            p=subprocess.run([sys.executable,'-O',str(Path(__file__).parent/'supplemental.py')],env=dict(os.environ,EDGE_REPORT=d),capture_output=True,text=True)
            self.assertNotEqual(p.returncode,0)
            self.assertNotIn('PASS',p.stdout)
            self.assertIn('exits failed',p.stderr)

    def test_unknown_stage_cannot_pass_empty_checks(self):
        import os,subprocess,sys
        with tempfile.TemporaryDirectory() as d:
            p=subprocess.run([sys.executable,str(Path(__file__).parent/'stage_gate.py'),'unknown-stage'],env=dict(os.environ,EDGE_REPORT=d,EDGE_LAB=d),capture_output=True,text=True)
            self.assertNotEqual(p.returncode,0)
            self.assertEqual(json.loads((Path(d)/'unknown-stage.json').read_text())['state'],'FAIL')

class PasswordTests(unittest.TestCase):
    def test_forced_triple_entropy_is_rejected(self):
        with patch('password.secrets.token_hex',side_effect=['aaa'+'0123456789abcdef'*3,'0123456789abcdef'*3]) as entropy:
            value=generate_password()
            self.assertEqual(entropy.call_count,2)
            self.assertFalse(re.search(r'(.)\1\1',value))

    def test_random_password_policy_contract(self):
        for _ in range(1000):
            value=generate_password()
            self.assertGreaterEqual(len(value),9)
            self.assertTrue(any(c.isupper() for c in value))
            self.assertTrue(any(c.islower() for c in value))
            self.assertTrue(any(c.isdigit() for c in value))
            self.assertIn('!',value)
            self.assertFalse(re.search(r'(.)\1\1',value))


class CounterTests(unittest.TestCase):
    def test_empty_error_zero_and_nonfinite_are_not_positive(self):
        metric='loxilb_ipfilter_blacklist_packets_total'
        for body in ['', '{"error":"failed"}', metric+' 0', metric+' NaN', metric+' inf', metric+' -1', metric+' bad']:
            with self.subTest(body=body):
                self.assertFalse(positive_counter(body,metric))

    def test_counter_requires_finite_present_nonnegative_samples(self):
        metric='loxilb_security_conn_blocked_total'
        for body in ['',metric+' inf',metric+' NaN',metric+' -1',metric+' 1e308\n'+metric+' 1e308']:
            with self.subTest(body=body),self.assertRaises(ValueError):metric_sum(body,metric)

    def test_timestamp_is_not_the_counter_value(self):
        self.assertEqual(metric_sum('counter{source="x"} 2 1700000000000','counter'),2)

    def test_positive_expected_family_only(self):
        metric='loxilb_ipfilter_blacklist_packets_total'
        self.assertTrue(positive_counter(metric+'{cidr="172.30.97.51/32"} 2',metric))
        self.assertFalse(positive_counter(metric+'_other 2',metric))


if __name__=='__main__':
    unittest.main()
