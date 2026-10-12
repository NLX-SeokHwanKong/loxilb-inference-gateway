#!/usr/bin/env python3
"""Safety checks for campaign-scoped Docker cleanup without real Docker mutations."""
import json
import subprocess
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import Mock, patch
from run_gateway import Resources, owned_outage


class CleanupTests(unittest.TestCase):
    @patch('run_gateway.subprocess.run')
    @patch('run_gateway.subprocess.check_output')
    def test_only_exact_owned_resources(self, inspect, run):
        resources = Resources('uc8-test')
        resources.container = 'uc8-test-gw'
        resources.network = 'uc8-test-net'
        inspect.side_effect = [json.dumps([{'Config': {'Labels': {'uc8.owner': 'uc8-test'}}}]),
                               json.dumps([{'Labels': {'uc8.owner': 'uc8-test'}}])]
        resources.cleanup()
        self.assertEqual([call.args[0] for call in run.call_args_list],
                         [['docker', 'rm', '-f', 'uc8-test-gw'], ['docker', 'network', 'rm', 'uc8-test-net']])

    @patch('run_gateway.subprocess.run')
    @patch('run_gateway.subprocess.check_output')
    def test_foreign_owner_never_removed(self, inspect, run):
        resources = Resources('uc8-test')
        resources.container = 'foreign'
        inspect.return_value = json.dumps([{'Config': {'Labels': {'uc8.owner': 'someone-else'}}}])
        with self.assertRaisesRegex(RuntimeError, 'owner mismatch'):
            resources.cleanup()
        run.assert_not_called()

    def test_only_recorded_child_process_stopped(self):
        process = Mock()
        process.poll.return_value = None
        process.wait.side_effect = [subprocess.TimeoutExpired('fixture', 5), 0]
        resources = Resources('uc8-test')
        resources.processes = [process]
        resources.cleanup()
        process.terminate.assert_called_once()
        process.kill.assert_called_once()
        self.assertEqual(process.wait.call_count, 2)


class OutageSafetyTests(unittest.TestCase):
    @patch('run_gateway.time.sleep')
    @patch('run_gateway.subprocess.Popen')
    @patch('run_gateway.invoke')
    def test_failed_route_escape_still_restores_only_owned_child(self, invoke, spawn, sleep):
        original = Mock(args=['python3', 'owned-fixture.py'])
        unrelated = Mock()
        replacement = Mock()
        replacement.poll.return_value = None
        spawn.return_value = replacement
        resources = Resources('uc8-test')
        resources.processes = [original, unrelated]
        invoke.side_effect = [(200, {'backend_id': 'backend-a'}), (200, {'backend_id': 'backend-b'})]
        with tempfile.TemporaryDirectory() as temporary:
            folder = Path(temporary)
            journal = folder / 'receipt.jsonl'
            journal.write_text('')
            with self.assertRaisesRegex(AssertionError, 'expected502/no-arrival'):
                owned_outage(resources, 'http://owned/mcp', 'http://owned/rr', [journal], folder)
        original.terminate.assert_called_once()
        original.wait.assert_called_once_with(timeout=5)
        spawn.assert_called_once()
        self.assertEqual(spawn.call_args.args[0], original.args)
        self.assertIs(resources.processes[0], replacement)
        self.assertIs(resources.processes[1], unrelated)
        unrelated.terminate.assert_not_called()


class RetryReservationSafetyTests(unittest.TestCase):
    @patch('verify_fc_retry.socket.socket')
    @patch('verify_fc_retry.management')
    def test_occupied_port_closes_reservation_without_api_mutation(self, management, create_socket):
        from verify_fc_retry import replay
        management.side_effect = urllib.error.HTTPError('http://owned/rule', 404, 'absent', {}, None)
        reservation = Mock()
        reservation.bind.side_effect = OSError('owned retry port occupied')
        create_socket.return_value = reservation
        template = {'serviceArguments': {'externalIP': '192.0.2.10', 'port': 2028},
                    'endpoints': [{'endpointIP': '192.0.2.1', 'targetPort': 8201, 'weight': 1}]}
        with self.assertRaisesRegex(OSError, 'occupied'):
            replay('http://192.0.2.10:2042/mcp', 'http://owned', [], template)
        reservation.bind.assert_called_once_with(('192.0.2.1', 8299))
        reservation.close.assert_called_once()
        self.assertEqual(management.call_count, 1)
        self.assertEqual(len(management.call_args.args), 1)  # read-only GET


if __name__ == '__main__':
    unittest.main()
