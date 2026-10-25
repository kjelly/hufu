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
        self.assertIn('needs: [lint, vet, test, integration, build, cgo-free, docs, eval-core]', workflow)

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
