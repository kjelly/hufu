import datetime
import importlib.util
import contextlib
import io
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import textwrap
import unittest
from unittest import mock
import urllib.error

spec = importlib.util.spec_from_file_location('ci_cost', Path(__file__).with_name('ci-test-cost.py'))
cost = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cost)


class CostTests(unittest.TestCase):
    def test_api_retry_policy(self):
        cases = [
            ('5xx recovery', [urllib.error.HTTPError('url', 503, 'busy', {}, None),
                              io.BytesIO(b'{"ok": true}')], None, 2),
            ('5xx exhausted', [urllib.error.HTTPError('url', 502, 'busy', {}, None)] * 2,
             urllib.error.HTTPError, 2),
            ('connection', [urllib.error.URLError('offline')] * 2, urllib.error.URLError, 2),
            ('timeout', [TimeoutError('slow')] * 2, TimeoutError, 2),
            ('rate limit', [urllib.error.HTTPError('url', 429, 'limited', {}, None)],
             urllib.error.HTTPError, 1),
            ('forbidden', [urllib.error.HTTPError('url', 403, 'limited', {}, None)],
             urllib.error.HTTPError, 1),
            ('invalid JSON', [io.BytesIO(b'not json')], ValueError, 1),
        ]
        for name, responses, error, calls in cases:
            with self.subTest(name=name), \
                    mock.patch.object(cost.urllib.request, 'urlopen', side_effect=responses) as api, \
                    mock.patch.object(cost.time, 'sleep') as sleep:
                if error:
                    with self.assertRaises(error):
                        cost.get_json('https://api.github.com', 'secret', '/run')
                else:
                    self.assertEqual(cost.get_json('https://api.github.com', 'secret', '/run'), {'ok': True})
                self.assertEqual(api.call_count, calls)
                self.assertEqual(sleep.call_count, calls - 1)
                for call in api.call_args_list:
                    self.assertEqual(call.kwargs['timeout'], 10)

    def test_main_reports_success_or_unavailable(self):
        run = {'created_at': '2026-10-24T10:00:00Z', 'run_attempt': 1}
        cases = [
            ('normal', [run, {'jobs': []}], 0, 'Total runner time so far:'),
            ('server', [urllib.error.HTTPError('private-url', 503, 'secret', {}, None)] * 2,
             1, 'HTTP 503'),
            ('rate limit', [urllib.error.HTTPError('private-url', 403, 'secret', {}, None)],
             1, 'HTTP 403'),
            ('timeout', [TimeoutError('secret')] * 2, 1, 'network error or timeout'),
            ('connection', [urllib.error.URLError('secret')] * 2, 1, 'network error or timeout'),
            ('json', [b'not JSON'], 1, 'JSONDecodeError'),
            ('run shape', [[]], 1, 'TypeError'),
            ('attempt', [{**run, 'run_attempt': 0}], 1, 'ValueError'),
            ('jobs shape', [run, {'jobs': None}], 1, 'ValueError'),
            ('job shape', [run, {'jobs': [None]}], 1, 'TypeError'),
            ('timestamp', [{**run, 'created_at': 'invalid'}, {'jobs': []}], 1, 'ValueError'),
            ('missing jobs', [run, {}], 1, 'KeyError'),
        ]
        for name, payloads, status, expected in cases:
            responses = [item if isinstance(item, Exception) else
                         io.BytesIO(item if isinstance(item, bytes) else json.dumps(item).encode())
                         for item in payloads]
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                summary = Path(directory) / 'summary'
                summary.write_text('existing summary\n')
                env = {'GITHUB_API_URL': 'https://api.github.com', 'GH_TOKEN': 'secret',
                       'GITHUB_REPOSITORY': 'kjelly/hufu', 'GITHUB_RUN_ID': '123',
                       'GITHUB_STEP_SUMMARY': str(summary)}
                with mock.patch.dict(os.environ, env), \
                        mock.patch.object(cost.urllib.request, 'urlopen', side_effect=responses), \
                        mock.patch.object(cost.time, 'sleep'), \
                        contextlib.redirect_stdout(io.StringIO()), \
                        contextlib.redirect_stderr(io.StringIO()) as stderr:
                    self.assertEqual(cost.main() or 0, status)
                contents = summary.read_text()
                self.assertTrue(contents.startswith('existing summary\n'))
                self.assertIn(expected, contents)
                self.assertEqual('ci_cost=unavailable' in contents, status == 1)
                self.assertNotIn('secret', contents + stderr.getvalue())
                self.assertNotIn('private-url', contents + stderr.getvalue())
                self.assertNotIn('Traceback', stderr.getvalue())

    def test_workflow_isolates_reporting_and_preserves_required_gate(self):
        workflow = (Path(__file__).resolve().parents[1] / '.github/workflows/ci.yml').read_text()
        aggregate = workflow.split('  ci-success:\n', 1)[1]
        self.assertIn('needs: [lint, vet, test, integration, build, cgo-free, docs, eval-core]', aggregate)
        self.assertIn('permissions:\n      actions: read\n      contents: read', aggregate)
        self.assertIn('    if: always()', aggregate)
        gate, report = aggregate.split('      - name: Report workflow and runner cost\n')
        self.assertIn('      - name: Fail if any required check failed', gate)
        self.assertIn('        if: always()\n', report)
        self.assertIn('        continue-on-error: true\n', report)
        self.assertIn('        timeout-minutes: 1\n', report)
        self.assertIn('        run: python3 scripts/ci-test-cost.py', report)
        self.assertNotIn('continue-on-error:', gate)
        script = textwrap.dedent(gate.split('        run: |\n', 1)[1])
        # Execute the actual gate shell with the two GitHub expressions rendered.
        # A failed/cancelled dependency must remain red even when reporting fails.
        for failed, cancelled, expected in [(False, False, 0), (True, False, 1),
                                            (False, True, 1), (True, True, 1)]:
            rendered = script.replace("${{ contains(needs.*.result, 'failure') }}", str(failed).lower())
            rendered = rendered.replace("${{ contains(needs.*.result, 'cancelled') }}", str(cancelled).lower())
            result = subprocess.run(['bash', '-e', '-c', rendered], capture_output=True, text=True)
            self.assertEqual(result.returncode, expected, result.stderr)

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
