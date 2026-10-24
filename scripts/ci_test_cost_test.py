import datetime
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest

spec = importlib.util.spec_from_file_location('ci_cost', Path(__file__).with_name('ci-test-cost.py'))
cost = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cost)


class CostTests(unittest.TestCase):
    def test_workflow_summary_handles_discovery_failure(self):
        workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/ci.yml').read_text()
        blocks = re.findall(
            r'      - name: Record timing and cache cost\n        if: always\(\)\n        run: \|\n((?:          [^\n]*\n)+)',
            workflow)
        self.assertEqual(len(blocks), 2)
        self.assertEqual(blocks[0], blocks[1])
        for has_timing in (False, True):
            with self.subTest(has_timing=has_timing), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                artifacts = root / 'work/ci-tests'
                artifacts.mkdir(parents=True)
                cache = root / 'cache'
                cache.mkdir()
                executable = root / 'bin/go'
                executable.parent.mkdir()
                executable.write_text('#!/bin/sh\nprintf "%s\\n" "$TEST_CACHE_DIR"\n')
                executable.chmod(0o700)
                summary = root / 'summary'
                if has_timing:
                    (artifacts / 'time.txt').write_text('wall_seconds=1.25\n')
                    (artifacts / 'cache-before.txt').write_text('123\t/cache\n')
                result = subprocess.run(
                    ['bash', '-e', '-c', textwrap.dedent(blocks[0])], cwd=root,
                    env={**os.environ, 'PATH': str(executable.parent) + os.pathsep + os.environ.get('PATH', ''),
                         'TEST_CACHE_DIR': str(cache), 'GITHUB_STEP_SUMMARY': str(summary)},
                    capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)
                contents = summary.read_text()
                if has_timing:
                    self.assertIn('wall_seconds=1.25', contents)
                    self.assertIn('cache_before_kib=123', contents)
                    self.assertNotIn('unavailable', contents)
                else:
                    self.assertIn('test_command_timing=unavailable', contents)
                    self.assertIn('cache_before_kib=unavailable', contents)

    def test_overlapping_jobs_sum_runner_time_without_summing_wall(self):
        run = {'created_at': '2026-10-24T10:00:00Z', 'run_attempt': 2}
        first = {'id': 1, 'started_at': '2026-10-24T10:00:10Z',
                 'completed_at': '2026-10-24T10:00:40Z', 'status': 'completed'}
        jobs = [first, first,
                {'id': 2, 'started_at': '2026-10-24T10:00:20Z',
                 'completed_at': '2026-10-24T10:00:50Z', 'status': 'completed'},
                {'id': 3, 'started_at': '2026-10-24T10:01:50Z',
                 'completed_at': None, 'status': 'in_progress'},
                {'id': 4, 'started_at': None, 'completed_at': None, 'status': 'queued'}]
        result = cost.summarize(run, jobs, cost.timestamp('2026-10-24T10:02:00Z'))
        self.assertEqual(result, {'workflow_wall_seconds_so_far': 120,
                                'runner_seconds_so_far': 70, 'job_records': 4,
                                'in_progress_jobs': 1, 'attempts': 2})

    def test_all_attempts_and_pages_are_collected(self):
        calls = []

        def get(path):
            calls.append(path)
            if path == '/run':
                return {'run_attempt': 2}
            if path == '/run/attempts/1/jobs?per_page=100&page=1':
                return {'jobs': [{'id': i} for i in range(100)]}
            if path == '/run/attempts/1/jobs?per_page=100&page=2':
                return {'jobs': [{'id': 100}]}
            if path == '/run/attempts/2/jobs?per_page=100&page=1':
                return {'jobs': [{'id': 101}]}
            self.fail(path)

        run, jobs = cost.collect(get, '/run')
        self.assertEqual(run['run_attempt'], 2)
        self.assertEqual(len(jobs), 102)
        self.assertEqual(len(calls), 4)

    def test_no_negative_time_for_clock_rounding(self):
        run = {'created_at': '2026-10-24T10:00:00Z', 'run_attempt': 1}
        jobs = [{'id': 1, 'started_at': '2026-10-24T10:00:01Z',
                 'completed_at': None, 'status': 'in_progress'}]
        now = datetime.datetime(2026, 10, 24, 10, 0, tzinfo=datetime.timezone.utc)
        self.assertEqual(cost.summarize(run, jobs, now)['runner_seconds_so_far'], 0)


if __name__ == '__main__':
    unittest.main()
