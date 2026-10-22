"""Regression checks for performance evidence, using overlapping Go test events."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('baseline', Path(__file__).with_name('test-baseline.py'))
baseline = importlib.util.module_from_spec(spec)
spec.loader.exec_module(baseline)


class EvidenceTests(unittest.TestCase):
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
