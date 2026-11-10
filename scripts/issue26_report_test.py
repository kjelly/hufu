"""Adversarial checks for incomplete captures and misleading terminal outcomes."""

import io
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import issue26_report as report


class ReportTests(unittest.TestCase):
    def test_only_audited_ephemeral_names_are_normalized_and_collisions_fail(self):
        family = 'TestWarmModelProfilesNoNetAllowsLoopbackAndRejectsRemote/'
        a, b = family + 'http://127.0.0.1:12345/v1', family + 'http://127.0.0.1:54321/v1'
        self.assertEqual(report.normalize_case(a), report.normalize_case(b))
        unrelated = 'TestAnother/http://127.0.0.1:12345/v1'
        self.assertEqual(report.normalize_case(unrelated), unrelated)
        suffix = 'a' * 64
        name = 'TestReviewBundleIndependentReadOnlyRoundTrip/tamper_objects/' + suffix
        self.assertTrue(report.normalize_case(name).endswith('/<object-hash>'))
        unrelated_hash = 'TestAnother/' + suffix
        self.assertEqual(report.normalize_case(unrelated_hash), unrelated_hash)
        rows = [dict(Action='pass', Package='p', Test=name) for name in (a, b)]
        with self.assertRaises(ValueError):
            report.inventory(io.BytesIO(''.join(json.dumps(row) + '\n' for row in rows).encode()))

    def archive(self, path, mode='normal', sample=1, corrupt=None):
        flags = ['-p', '2', '-json', '-timeout', '30m'] + (['-race'] if mode == 'race' else [])
        manifest = {'experiment': 'issue26-fixture-remediated-v1', 'mode': mode, 'sample_index': sample,
                    'samples_required': 3, 'timeout_seconds': 1800, 'full_suite': True,
                    'controller_revision': 'controller', 'harness_sha256': 'harness',
                    'run_id': 'run', 'run_attempt': '1', 'sources': {}}
        members = {}
        for label, original in report.SOURCES.items():
            measured = report.VARIANTS[label]
            manifest['sources'][label] = {'original_revision': original, 'fixture_fix': report.FIX,
                                         'measured_revision': measured, 'patch_sha256': report.PATCH_SHA256,
                                         'exit_code': 0,
                                         'native_grep': {'passed': True}}
            env = {'commit': measured, 'dirty': False, 'sample_index': sample, 'mode': mode,
                   'repetitions_required': 3, 'flags': flags, 'gomaxprocs_configured': '2',
                   'gomaxprocs_effective': 2, 'cpu': 'same CPU', 'os': 'same OS', 'machine': 'x86_64',
                   'logical_cpus': 4, 'cpu_quota': None,
                   'packages': report.PACKAGES, 'go': {'GOVERSION': 'go1.27.2', 'GOTOOLCHAIN': 'local',
                                               'GOFLAGS': '-mod=readonly',
                                               'CGO_ENABLED': '0' if mode == 'cgo-free' else '1'}}
            members[label + '/environment.json'] = json.dumps(env)
            for state in ('cold', 'warm', 'result-seed', 'result'):
                name = f'{state}-{sample}'
                command = ['go', 'test', *flags] + (['-count=1'] if state in ('cold', 'warm') else []) + report.PACKAGES
                observation = {'exit_code': 0, 'name': name, 'command': command,
                               'wall_seconds': sample, 'cpu_user_seconds': sample * 2,
                               'cpu_system_seconds': sample / 2}
                members[f'{label}/{name}.json'] = json.dumps(observation)
                events = [{'Package': 'github.com/kjelly/hufu/internal/tools', 'Test': test, 'Action': 'pass'}
                          for test in sorted(report.NATIVE_GREP)]
                observation['case_set_sha256'] = hashlib.sha256(json.dumps(sorted(
                    (event['Package'], event['Test']) for event in events)).encode()).hexdigest()
                observation['test_counts'] = {'pass': len(events), 'fail': 0, 'skip': 0}
                members[f'{label}/{name}.json'] = json.dumps(observation)
                members[f'{label}/{name}.jsonl'] = ''.join(json.dumps(event) + '\n' for event in events)
                members[f'{label}/{name}.stderr'] = ''
        members['capture.json'] = json.dumps(manifest)
        if corrupt:
            corrupt(members)
        with zipfile.ZipFile(path, 'w') as archive:
            for name, value in members.items():
                archive.writestr(name, value)

    def test_complete_matrix_does_not_automatically_claim_semantic_acceptance(self):
        with tempfile.TemporaryDirectory() as temporary:
            captures = []
            for mode in ('normal', 'race', 'cgo-free'):
                for sample in (1, 2, 3):
                    path = Path(temporary) / f'{mode}-{sample}.zip'
                    self.archive(path, mode, sample)
                    captures.append(report.inspect_archive(path))
            result = report.aggregate(captures)
            self.assertTrue(result['capture_matrix_passed'])
            self.assertFalse(result['acceptance_complete'])
            self.assertEqual(result['groups']['before/race/warm']['wall_seconds'],
                             {'samples': 3, 'median': 2, 'min': 1, 'max': 3})
            for field in ('run_id', 'controller_revision', 'harness_sha256'):
                changed = copy.deepcopy(captures)
                changed[0]['manifest'][field] = 'substitute'
                with self.subTest(field=field), self.assertRaises(ValueError):
                    report.aggregate(changed)
            changed = copy.deepcopy(captures)
            changed[0]['sources']['before']['environment']['cpu'] = 'different CPU'
            with self.assertRaises(ValueError):
                report.aggregate(changed)

    def test_archive_gate_rejects_substituted_identity_and_missing_raw_data(self):
        def wrong_head(members):
            env = json.loads(members['before/environment.json'])
            env['commit'] = 'another-commit'
            members['before/environment.json'] = json.dumps(env)

        def missing_log(members):
            del members['before/warm-1.jsonl']

        def substituted_variant(members):
            manifest = json.loads(members['capture.json'])
            manifest['sources']['before']['measured_revision'] = 'unreviewed-variant'
            members['capture.json'] = json.dumps(manifest)
            env = json.loads(members['before/environment.json'])
            env['commit'] = 'unreviewed-variant'
            members['before/environment.json'] = json.dumps(env)

        def substituted_patch(members):
            manifest = json.loads(members['capture.json'])
            manifest['sources']['before']['patch_sha256'] = 'unreviewed-patch'
            members['capture.json'] = json.dumps(manifest)

        def zero_exit_failed_package(members):
            members['before/warm-1.jsonl'] += json.dumps({'Package': 'p', 'Action': 'fail'}) + '\n'

        def narrowed_packages(members):
            env = json.loads(members['before/environment.json'])
            env['packages'] = ['./internal/tools']
            members['before/environment.json'] = json.dumps(env)

        def unbound_extra_case(members):
            members['before/warm-1.jsonl'] += json.dumps(
                {'Action': 'pass', 'Package': 'p', 'Test': 'TestNotInRecordedObservation'}) + '\n'

        for mutation in (wrong_head, substituted_variant, substituted_patch, missing_log,
                         zero_exit_failed_package, narrowed_packages, unbound_extra_case):
            with self.subTest(mutation=mutation.__name__), tempfile.TemporaryDirectory() as temporary:
                path = Path(temporary) / 'capture.zip'
                self.archive(path, corrupt=mutation)
                with self.assertRaises((ValueError, KeyError)):
                    report.inspect_archive(path)

    def test_incomplete_or_duplicate_planned_samples_cannot_pass(self):
        for captures in ([], [{'manifest': {'mode': 'race', 'sample_index': 1}}] * 9):
            with self.subTest(samples=len(captures)), self.assertRaises(ValueError):
                report.aggregate(captures)

    def test_package_failure_is_preserved_even_without_a_failed_test(self):
        rows = [dict(Action='pass', Package='p', Test='TestPassed'), dict(Action='fail', Package='p')]
        result = report.inventory(io.BytesIO(''.join(json.dumps(row) + '\n' for row in rows).encode()))
        self.assertEqual(result['failed_packages'], ['p'])
        self.assertEqual(result['failed_tests'], [])
        self.assertFalse(result['native_grep_passed'])

    def test_duplicate_terminal_event_is_not_silently_overwritten(self):
        rows = [dict(Action='pass', Package='p', Test='TestRepeated')] * 2
        with self.assertRaises(ValueError):
            report.inventory(io.BytesIO(''.join(json.dumps(row) + '\n' for row in rows).encode()))

    def test_parent_and_subtest_are_separate_outcomes_without_added_timings(self):
        rows = [dict(Action='pass', Package='p', Test='TestParent/child', Elapsed=4),
                dict(Action='pass', Package='p', Test='TestParent', Elapsed=5)]
        result = report.inventory(io.BytesIO(''.join(json.dumps(row) + '\n' for row in rows).encode()))
        self.assertEqual(result['top_level'], [['p', 'TestParent', 'pass']])
        self.assertEqual(result['all_case_count'], 2)


if __name__ == '__main__':
    unittest.main()
