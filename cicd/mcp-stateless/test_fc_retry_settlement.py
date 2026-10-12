"""Deferred local-error teardown must settle; leaks and wrong policy still fail."""
import unittest
from unittest.mock import patch
from verify_fc_retry import _settle_closed_counter

class SettlementTests(unittest.TestCase):
    def run_probe(self, counters, **changes):
        clock=[0.0]
        def observe(_):
            inflight=counters.pop(0) if len(counters)>1 else counters[0]
            return 200,{'serviceArguments':{'fc_effective':{'mode':'enforce','ep_max_inflight':1,'queue_depth':0,'queued':0,'inflight':inflight,**changes}}}
        with patch('verify_fc_retry.management',side_effect=observe),patch('verify_fc_retry.time.monotonic',side_effect=lambda:clock[0]),patch('verify_fc_retry.time.sleep',side_effect=lambda seconds:clock.__setitem__(0,clock[0]+seconds)):
            return _settle_closed_counter('fixture',timeout=.03)
    def test_deferred_close_observed_then_exact_zero(self):
        effective,observations=self.run_probe([1,0])
        self.assertEqual(effective['inflight'],0)
        self.assertEqual([x['effective']['inflight'] for x in observations],[1,0])
    def test_permanent_permit_leak_fails(self):
        with self.assertRaisesRegex(AssertionError,'did not return to zero'):self.run_probe([1])
    def test_zero_observed_after_deadline_still_fails(self):
        effective={'mode':'enforce','ep_max_inflight':1,'queue_depth':0,'queued':0,'inflight':0}
        with patch('verify_fc_retry.management',return_value=(200,{'serviceArguments':{'fc_effective':effective}})),patch('verify_fc_retry.time.monotonic',side_effect=[0.0,.04]):
            with self.assertRaisesRegex(AssertionError,'did not return to zero'):_settle_closed_counter('fixture',timeout=.03)
    def test_extra_permits_fail_immediately(self):
        with self.assertRaisesRegex(AssertionError,'did not return to zero'):self.run_probe([2,0])
    def test_queue_is_never_tolerated(self):
        with self.assertRaisesRegex(AssertionError,'contract incorrect'):self.run_probe([0],queued=1)
    def test_wrong_policy_is_never_tolerated(self):
        with self.assertRaisesRegex(AssertionError,'contract incorrect'):self.run_probe([0],mode='observe')

if __name__=='__main__':unittest.main()
