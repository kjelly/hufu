"""Regression checks for performance evidence, using overlapping Go test events."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('baseline', Path(__file__).with_name('test-baseline.py'))
baseline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(baseline)


class EvidenceTests(unittest.TestCase):
    def test_indexed_sample_keeps_fresh_and_result_cache_commands_distinct(self):
        def output(command, env=None):
            if command[:2] == ['go', 'env']:
                return json.dumps({'GOVERSION': 'go1.27.2', 'GOFLAGS': '', 'CGO_ENABLED': '1'})
            if command[:2] == ['go', 'run']:
                return '2'
            return '' if command[:2] == ['git', 'status'] else 'source-sha'

        def measure(directory, name, command, env):
            return {'wall_seconds': 1, 'exit_code': 0, 'packages': []}

        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary) / 'capture'
            argv = ['baseline', '--output', str(target), '--full', '--mode', 'race',
                    '--cache-state', 'all', '--sample-index', '2', '--timeout-seconds', '1800']
            with patch('sys.argv', argv), patch.object(baseline, 'output', side_effect=output), \
                 patch.object(baseline, 'measure', side_effect=measure) as measured:
                self.assertEqual(baseline.main(), 0)
            calls = measured.call_args_list
            self.assertEqual([c.args[1] for c in calls], ['cold-2', 'warm-2', 'result-seed-2', 'result-2'])
            self.assertTrue(all('-race' in c.args[2] and '-short' not in c.args[2] for c in calls))
            self.assertTrue(all('-count=1' in c.args[2] for c in calls[:2]))
            self.assertTrue(all('-count=1' not in c.args[2] for c in calls[2:]))
            metadata = json.loads((target / 'environment.json').read_text())
            self.assertEqual(metadata['sample_index'], 2)
            self.assertEqual(metadata['repetitions_required'], 3)
            self.assertEqual(metadata['flags'][:5], ['-p', '2', '-json', '-timeout', '30m'])
            self.assertEqual(json.loads((target / 'summary.json').read_text())['cold']['samples'], 1)

    def test_invalid_sampling_or_shortened_deadline_fails_before_measurement(self):
        for args in (['--sample-index', '0'], ['--sample-index', '4'], ['--timeout-seconds', '1199']):
            with self.subTest(args=args), patch('sys.argv', ['baseline', '--output', 'unused', *args]), \
                 patch.object(baseline, 'output') as output:
                with self.assertRaises(SystemExit) as failure:
                    baseline.main()
                self.assertEqual(failure.exception.code, 2)
                output.assert_not_called()

    def test_overlapping_parents_and_subtests_are_not_added_to_package_time(self):
        events = [
            {'Action': 'pass', 'Package': 'p', 'Test': 'TestParent/child', 'Elapsed': 4},
            {'Action': 'pass', 'Package': 'p', 'Test': 'TestParent', 'Elapsed': 5},
            {'Action': 'skip', 'Package': 'p', 'Test': 'TestLive', 'Elapsed': 0},
            {'Action': 'output', 'Package': 'p', 'Output': 'ok p (cached)\n'},
            {'Action': 'pass', 'Package': 'p', 'Elapsed': 6},
        ]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events.jsonl'
            path.write_text(''.join(json.dumps(event) + '\n' for event in events))
            result = baseline.summarize(path)
        self.assertEqual(result['packages'][0]['Elapsed'], 6)
        self.assertEqual([row['Elapsed'] for row in result['slowest_tests']], [5, 4])
        self.assertEqual(result['cached_packages'], ['p'])
        self.assertEqual(result['test_counts'], {'pass': 2, 'fail': 0, 'skip': 1})
        self.assertEqual(result['skipped_tests'][0]['Test'], 'TestLive')

    def test_failed_observations_cannot_be_reported_as_successful(self):
        rows = [{'wall_seconds': elapsed, 'exit_code': code, 'packages': [
            {'Package': 'p', 'Elapsed': elapsed / 2}]} for elapsed, code in ((3, 0), (9, 1), (6, 0))]
        result = baseline.aggregate(rows)
        self.assertFalse(result['successful'])
        self.assertEqual(result['wall_seconds'], {'median': 6, 'min': 3, 'max': 9})
        self.assertEqual(result['packages']['p']['median'], 3)

    def test_measure_preserves_nonzero_exit_and_stderr(self):
        import sys
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            row = baseline.measure(path, 'failure', [sys.executable, '-c',
                                   'import sys; print("build failed", file=sys.stderr); sys.exit(7)'], None)
            self.assertEqual(row['exit_code'], 7)
            self.assertEqual(row['packages'], [])
            self.assertIn('build failed', (path / 'failure.stderr').read_text())
            self.assertEqual(json.loads((path / 'failure.json').read_text())['exit_code'], 7)


if __name__ == '__main__':
    unittest.main()
