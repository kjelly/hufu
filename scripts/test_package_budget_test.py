import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('test-package-budget.py')
spec = importlib.util.spec_from_file_location('package_budget', SCRIPT)
budget = importlib.util.module_from_spec(spec)
spec.loader.exec_module(budget)


class PackageBudgetTests(unittest.TestCase):
    def test_workflow_keeps_deadlines_and_required_execution_contracts(self):
        # Guard the effective CI wiring as well as the standalone reporter.
        workflow = (SCRIPT.parents[1] / '.github/workflows/ci.yml').read_text()
        test_job = workflow.split('\n  test:\n', 1)[1].split('\n  integration:\n', 1)[0]
        integration = workflow.split('\n  integration:\n', 1)[1].split('\n  build:\n', 1)[0]
        self.assertIn('scope: team\n            package-timeout: 30m\n            job-timeout: 40', test_job)
        self.assertIn('scope: improver\n            package-timeout: 30m\n            job-timeout: 40', test_job)
        self.assertEqual(test_job.count('package-timeout: 20m'), 2)
        self.assertIn('timeout-minutes: ${{ matrix.job-timeout }}', test_job)
        self.assertIn('-layer short -scope "$TEST_SCOPE" -- -race -json -timeout "$PACKAGE_TIMEOUT"', test_job)
        self.assertIn('-layer integration -- -race -json -timeout "$PACKAGE_TIMEOUT"', integration)
        for job in [test_job, integration]:
            self.assertIn('if: always()\n        env:\n          GO_CACHE_HIT:', job)
            self.assertIn('scripts/test-package-budget.py --timeout "$PACKAGE_TIMEOUT"', job)
            self.assertIn('path: work/ci-tests/', job)
        self.assertIn('CGO_ENABLED=0 go test ./... ./.agent-teams/hufu-dev/diagnostic-pilot/...', workflow)
        self.assertIn('needs: [lint, vet, test, integration, build, cgo-free, docs, eval-core, contracts]', workflow)

    def test_package_elapsed_not_sum_of_test_or_command_elapsed(self):
        events = [
            {'Action': 'start', 'Package': 'example/team'},
            {'Action': 'pass', 'Package': 'example/team', 'Test': 'TestSlow', 'Elapsed': 900},
            {'Action': 'pass', 'Package': 'example/team', 'Elapsed': 941.5},
            {'Action': 'fail', 'Package': 'example/other', 'Elapsed': 1600},
        ]
        rows, malformed = budget.packages_from_jsonl(map(json.dumps, events), 1800)
        self.assertEqual(malformed, 0)
        self.assertEqual(len(rows), 2)
        self.assertAlmostEqual(rows[1]['timeout_ratio'], 941.5 / 1800)
        self.assertEqual(rows[0]['status'], 'fail')
        summary, warnings = budget.render(rows, 1800, 0, 'true')
        self.assertIn('52.3%', summary)
        self.assertIn('88.9%', summary)
        self.assertEqual(len(warnings), 1)
        self.assertIn('example/other', warnings[0])

    def test_threshold_and_zero_elapsed(self):
        events = [
            {'Action': 'pass', 'Package': 'at', 'Elapsed': 1350},
            {'Action': 'pass', 'Package': 'above', 'Elapsed': 1350.1},
            {'Action': 'skip', 'Package': 'empty', 'Elapsed': 0},
        ]
        rows, _ = budget.packages_from_jsonl(map(json.dumps, events), 1800)
        _, warnings = budget.render(rows, 1800, 0, '')
        self.assertEqual(len(warnings), 1)
        self.assertTrue(warnings[0].startswith('above:'))

    def test_interrupted_json_and_missing_elapsed_are_visible(self):
        lines = ['{"Action":"start","Package":"aborted"}', '{"truncated',
                 '{"Action":"fail","Package":"build-failed"}', '', 'null']
        rows, malformed = budget.packages_from_jsonl(lines, 1200)
        summary, warnings = budget.render(rows, 1200, malformed, '')
        self.assertEqual(malformed, 1)
        self.assertIn('incomplete', summary)
        self.assertIn('build-failed', summary)
        self.assertEqual(len(warnings), 3)
        self.assertTrue(all(row['timeout_ratio'] is None for row in rows))

    def test_duration_validation(self):
        self.assertEqual(budget.timeout_seconds('30m'), 1800)
        self.assertEqual(budget.timeout_seconds('1h2m3.5s'), 3723.5)
        for value in ['0s', '-1m', '30', '20mgarbage', '']:
            with self.subTest(value=value), self.assertRaises(argparse.ArgumentTypeError):
                budget.timeout_seconds(value)

    def test_test_timings_rank_parents_without_double_counting_subtests(self):
        events = [
            {'Action': 'run', 'Package': 'p', 'Test': 'TestSlow'},
            {'Action': 'pass', 'Package': 'p', 'Test': 'TestSlow/child', 'Elapsed': 4},
            {'Action': 'fail', 'Package': 'p', 'Test': 'TestSlow', 'Elapsed': 6},
            {'Action': 'run', 'Package': 'p', 'Test': 'TestSlow'},
            {'Action': 'pass', 'Package': 'p', 'Test': 'TestSlow', 'Elapsed': 4},
            {'Action': 'skip', 'Package': 'p', 'Test': 'TestSkipped', 'Elapsed': 0},
            {'Action': 'run', 'Package': 'p', 'Test': 'TestInterrupted'},
            {'Action': 'pass', 'Package': 'p', 'Elapsed': 12},
        ]
        samples = {}
        packages, malformed = budget.packages_from_jsonl(map(json.dumps, events), 1200, samples)
        rows = budget.test_timings(samples, {'tests': [
            {'package': 'p', 'test': 'TestSlow', 'elapsed_seconds': 4},
            {'package': 'p', 'test': 'TestSkipped', 'elapsed_seconds': 0},
        ]})
        self.assertEqual(malformed, 0)
        self.assertEqual(packages[0]['elapsed_seconds'], 12)
        self.assertEqual([row['test'] for row in rows], ['TestSlow', 'TestSkipped', 'TestInterrupted'])
        self.assertEqual(rows[0]['status'], 'fail')
        self.assertEqual(rows[0]['runs'], 2)
        self.assertEqual(rows[0]['elapsed_seconds'], 5)
        self.assertEqual(rows[0]['delta_seconds'], 1)
        self.assertEqual(rows[0]['delta_ratio'], 0.25)
        self.assertIsNone(rows[1]['delta_ratio'])
        self.assertIsNone(rows[2]['elapsed_seconds'])
        summary = budget.render_tests(rows, 1)
        self.assertIn('TestSlow', summary)
        self.assertIn('+1.000', summary)
        self.assertNotIn('TestSkipped', summary)

    def test_cli_compares_an_explicit_baseline(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            event = {'Action': 'pass', 'Package': 'p', 'Test': 'TestRun', 'Elapsed': 2}
            (root / 'tests.jsonl').write_text(json.dumps(event) + '\n')
            baseline = root / 'previous.json'
            baseline.write_text(json.dumps({'tests': [
                {'package': 'p', 'test': 'TestRun', 'elapsed_seconds': 1}]}))
            result = subprocess.run(
                [sys.executable, str(SCRIPT), '--timeout', '30m', '--evidence', str(root),
                 '--summary', str(root / 'summary'), '--baseline', str(baseline)],
                capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            report = json.loads((root / 'package-budget.json').read_text())
            self.assertEqual(report['tests'][0]['delta_seconds'], 1)
            self.assertIn('Slowest top-level tests', (root / 'summary').read_text())

    def test_cli_preserves_summary_and_creates_evidence_even_before_discovery(self):
        for has_results in [False, True]:
            with self.subTest(has_results=has_results), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                evidence = root / 'work/ci-tests'
                summary = root / 'summary'
                summary.write_text('existing metrics\n')
                if has_results:
                    evidence.mkdir(parents=True)
                    (evidence / 'tests.jsonl').write_text(json.dumps({
                        'Action': 'pass', 'Package': 'example/team', 'Elapsed': 1400}) + '\n')
                result = subprocess.run(
                    [sys.executable, str(SCRIPT), '--timeout', '30m',
                     '--evidence', str(evidence), '--summary', str(summary)],
                    env={**os.environ, 'GO_CACHE_HIT': 'false'},
                    capture_output=True, text=True, check=False)
                self.assertEqual(result.returncode, 0, result.stderr)
                report = json.loads((evidence / 'package-budget.json').read_text())
                self.assertEqual(report['timeout_seconds'], 1800)
                self.assertEqual(report['setup_go_cache_hit'], 'false')
                self.assertEqual(len(report['packages']), int(has_results))
                self.assertTrue(summary.read_text().startswith('existing metrics\n'))
                self.assertIn('::warning::', result.stdout)
                self.assertIn('77.8%' if has_results else 'no package results', summary.read_text())


if __name__ == '__main__':
    unittest.main()
