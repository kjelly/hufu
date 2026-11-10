"""Verify co-located sampling, seed evidence and full-suite admission."""

import json
from pathlib import Path
import tempfile
import unittest
import zipfile

import issue26_group_report as report
from issue26_acceptance import NATIVE_GREP


class GroupReportTests(unittest.TestCase):
    def archive(self, path, mode, state, corrupt=None):
        flags = ['-p', '2', '-json', '-timeout', '30m'] + (['-race'] if mode == 'race' else [])
        manifest = {'experiment': 'issue26-co-located-v2', 'mode': mode, 'cache_state': state,
                    'samples_required': 3, 'timeout_seconds': 1800, 'full_suite': True,
                    'controller_revision': 'controller', 'harness_sha256': 'harness',
                    'run_id': 'run', 'run_attempt': '1', 'sources': {},
                    'source_order': {str(i): ['before', 'after-32'] if i % 2 else ['after-32', 'before']
                                     for i in (1, 2, 3)}}
        members = {}
        for label, original in report.SOURCES.items():
            source = {'original_revision': original, 'fixture_fix': report.FIX,
                      'measured_revision': report.VARIANTS[label], 'patch_sha256': report.PATCH_SHA256,
                      'samples': {str(i): {'exit_code': 0, 'native_grep': {'passed': True}} for i in (1, 2, 3)}}
            manifest['sources'][label] = source
            for sample in (1, 2, 3):
                base = f'{label}/sample-{sample}/'
                env = {'commit': report.VARIANTS[label], 'dirty': False, 'sample_index': sample,
                       'mode': mode, 'repetitions_required': 3, 'flags': flags,
                       'seed_protocol': {'cold': 'full_cold_warm', 'warm': 'compile_only_warm',
                                         'result': 'direct_full_result_seed'}[state],
                       'packages': report.PACKAGES, 'gomaxprocs_configured': '2', 'gomaxprocs_effective': 2,
                       'go': {'GOVERSION': 'go1.27.2', 'GOTOOLCHAIN': 'local', 'GOFLAGS': '-mod=readonly',
                              'CGO_ENABLED': '0' if mode == 'cgo-free' else '1'},
                       'cpu': 'same CPU', 'os': 'same OS', 'machine': 'x86_64', 'hostname': 'same host',
                       'logical_cpus': 4, 'cpu_quota': None}
                members[base + 'environment.json'] = json.dumps(env)
                commands = []
                if state == 'warm':
                    commands = [(f'build-seed-tests-{sample}', ['go', 'test', *flags, '-count=1', '-run', '^$', *report.PACKAGES], False),
                                (f'build-seed-cli-{sample}', ['go', 'build', '-p', '2', '-o', '/dev/null', './cmd/hufu'], False)]
                elif state == 'result':
                    commands = [(f'result-seed-{sample}', ['go', 'test', *flags, *report.PACKAGES], True)]
                command = ['go', 'test', *flags] + (['-count=1'] if state != 'result' else []) + report.PACKAGES
                commands.append((f'{state}-{sample}', command, True))
                for name, command, full in commands:
                    members[base + name + '.json'] = json.dumps({
                        'exit_code': 0, 'name': name, 'command': command,
                        'wall_seconds': sample, 'cpu_user_seconds': sample, 'cpu_system_seconds': sample / 2})
                    events = [{'Package': 'github.com/kjelly/hufu/internal/tools', 'Test': test, 'Action': 'pass'}
                              for test in sorted(NATIVE_GREP)] if full else []
                    members[base + name + '.jsonl'] = ''.join(json.dumps(event) + '\n' for event in events)
                    members[base + name + '.stderr'] = ''
        members['capture.json'] = json.dumps(manifest)
        if corrupt:
            corrupt(members)
        with zipfile.ZipFile(path, 'w') as archive:
            for name, value in members.items():
                archive.writestr(name, value)

    def test_all_cohorts_have_three_same_runner_samples_but_do_not_auto_accept(self):
        with tempfile.TemporaryDirectory() as temporary:
            groups = []
            for mode in ('normal', 'race', 'cgo-free'):
                for state in ('cold', 'warm', 'result'):
                    path = Path(temporary) / f'{mode}-{state}.zip'
                    self.archive(path, mode, state)
                    groups.append(report.inspect_group(path))
            result = report.aggregate(groups)
            self.assertTrue(result['capture_matrix_passed'])
            self.assertFalse(result['acceptance_complete'])
            self.assertEqual(groups[0]['sources']['before']['statistics']['wall_seconds']['samples'], 3)
            with self.assertRaises(ValueError):
                report.aggregate(groups[:-1])

    def test_missing_samples_changed_hardware_and_failed_warm_seeds_are_rejected(self):
        def missing_sample(members):
            manifest = json.loads(members['capture.json'])
            del manifest['sources']['before']['samples']['3']
            members['capture.json'] = json.dumps(manifest)

        def changed_hardware(members):
            key = 'before/sample-3/environment.json'
            env = json.loads(members[key])
            env['cpu'] = 'another CPU'
            members[key] = json.dumps(env)

        def failed_seed(members):
            key = 'before/sample-1/build-seed-cli-1.json'
            row = json.loads(members[key])
            row['exit_code'] = 7
            members[key] = json.dumps(row)

        def filtered_measurement(members):
            key = 'before/sample-1/warm-1.json'
            row = json.loads(members[key])
            row['command'] += ['-run', '^$']
            members[key] = json.dumps(row)

        for mutation in (missing_sample, changed_hardware, failed_seed, filtered_measurement):
            with self.subTest(mutation=mutation.__name__), tempfile.TemporaryDirectory() as temporary:
                path = Path(temporary) / 'warm.zip'
                self.archive(path, 'normal', 'warm', mutation)
                with self.assertRaises(ValueError):
                    report.inspect_group(path)


if __name__ == '__main__':
    unittest.main()
