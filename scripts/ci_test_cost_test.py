import datetime
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('ci_cost', Path(__file__).with_name('ci-test-cost.py'))
cost = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cost)


class CostTests(unittest.TestCase):
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
