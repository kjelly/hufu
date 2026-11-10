"""Verify immutable measurement variants and fail-closed experiment admission."""

import os
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import issue26_acceptance as acceptance


class VariantTests(unittest.TestCase):
    def test_cohort_counterbalances_all_samples_and_stops_on_missing_native_evidence(self):
        for fail_native in (False, True):
            with self.subTest(fail_native=fail_native), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / 'capture'
                calls = []

                def remediate(source, sha):
                    source.mkdir()
                    label = next(name for name, value in acceptance.SOURCES.items() if value == sha)
                    return {'original_revision': sha, 'measured_revision': acceptance.VARIANTS[label],
                            'patch_sha256': acceptance.PATCH_SHA256, 'fixture_fix': acceptance.FIX}

                def main():
                    target = Path(sys.argv[sys.argv.index('--output') + 1])
                    sample = int(sys.argv[sys.argv.index('--sample-index') + 1])
                    calls.append((target.parent.name, sample))
                    self.assertIn('--direct-result-seed', sys.argv)
                    self.assertNotIn('--build-seed-only', sys.argv)
                    return 0

                module = SimpleNamespace(main=main)
                spec = SimpleNamespace(loader=SimpleNamespace(exec_module=lambda _: None))
                with patch.object(acceptance, 'admit_capture'), \
                     patch.object(acceptance, 'git', side_effect=lambda *args, **_: '' if args[:2] == ('status', '--porcelain') else 'controller'), \
                     patch.object(acceptance, 'remediate', side_effect=remediate), \
                     patch.object(acceptance.subprocess, 'run'), \
                     patch.object(acceptance.subprocess, 'check_output', return_value='ripgrep 14\n'), \
                     patch.object(acceptance.importlib.util, 'spec_from_file_location', return_value=spec), \
                     patch.object(acceptance.importlib.util, 'module_from_spec', return_value=module), \
                     patch.object(acceptance, 'native_grep_coverage', return_value={'passed': not fail_native}), \
                     patch.dict(os.environ, {'GITHUB_RUN_ID': 'run', 'GITHUB_RUN_ATTEMPT': '1'}), \
                     patch.object(sys, 'argv', list(sys.argv)):
                    result = acceptance.run_group(SimpleNamespace(output=output, mode='race', cache_state='result'))
                manifest = json.loads((output / 'capture.json').read_text())
                self.assertEqual(result, 1 if fail_native else 0)
                if fail_native:
                    self.assertEqual(calls, [('before', 1)])
                    self.assertFalse(manifest['sources']['before']['samples']['1']['native_grep']['passed'])
                else:
                    self.assertEqual(calls, [('before', 1), ('after-32', 1), ('after-32', 2),
                                             ('before', 2), ('before', 3), ('after-32', 3)])
                    self.assertEqual(set(manifest['sources']['after-32']['samples']), {'1', '2', '3'})

    def test_native_grep_gate_requires_actual_passes_in_every_command(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.assertFalse(acceptance.native_grep_coverage(directory)['passed'])
            events = [{'Package': 'github.com/kjelly/hufu/internal/tools', 'Test': name, 'Action': 'pass'}
                      for name in sorted(acceptance.NATIVE_GREP)]
            for state in ('cold', 'warm', 'result-seed', 'result'):
                (directory / f'{state}-1.jsonl').write_text(''.join(json.dumps(e) + '\n' for e in events))
            self.assertTrue(acceptance.native_grep_coverage(directory)['passed'])
            events[0]['Action'] = 'skip'
            (directory / 'warm-1.jsonl').write_text(''.join(json.dumps(e) + '\n' for e in events))
            result = acceptance.native_grep_coverage(directory)
            self.assertFalse(result['passed'])
            self.assertEqual(result['commands']['warm-1.jsonl']['missing_or_not_passed'], [events[0]['Test']])

    def test_fixture_variants_are_reproducible_and_leave_originals_unchanged(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repo = root / 'repo'
            repo.mkdir()
            env = {**os.environ, 'GIT_AUTHOR_NAME': 'fixture', 'GIT_AUTHOR_EMAIL': 'fixture@example.invalid',
                   'GIT_COMMITTER_NAME': 'fixture', 'GIT_COMMITTER_EMAIL': 'fixture@example.invalid'}

            def git(*args):
                return subprocess.check_output(['git', *args], cwd=repo, env=env, text=True,
                                               stderr=subprocess.DEVNULL).strip()

            git('init')
            target = repo / acceptance.FIX_PATH
            target.parent.mkdir(parents=True)
            target.write_text('original fixture\n')
            (repo / 'production.txt').write_text('unchanged production\n')
            git('add', '.')
            git('commit', '-m', 'original')
            original = git('rev-parse', 'HEAD')
            original_tree = git('rev-parse', 'HEAD^{tree}')
            target.write_text('drained fixture\n')
            git('add', '.')
            git('commit', '-m', 'fixture-only fix')
            fix = git('rev-parse', 'HEAD')
            with patch.object(acceptance, 'ROOT', repo), patch.object(acceptance, 'FIX', fix):
                first = acceptance.remediate(root / 'first', original)
                second = acceptance.remediate(root / 'second', original)
            self.assertEqual(first, second)
            self.assertNotEqual(first['measured_revision'], original)
            self.assertEqual(git('rev-parse', original + '^{tree}'), original_tree)
            self.assertEqual(git('show', original + ':' + acceptance.FIX_PATH), 'original fixture')
            self.assertEqual(git('diff', '--name-only', original, first['measured_revision']), acceptance.FIX_PATH)
            self.assertEqual(git('show', first['measured_revision'] + ':production.txt'), 'unchanged production')

    def test_incompatible_fixture_patch_fails_without_modifying_source(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / 'source'
            source.mkdir()
            with patch.object(acceptance, 'git', return_value='incompatible patch'), \
                 patch.object(acceptance.subprocess, 'run', side_effect=[
                     subprocess.CompletedProcess([], 0), subprocess.CalledProcessError(1, 'git apply')]) as run:
                with self.assertRaises(subprocess.CalledProcessError):
                    acceptance.remediate(source, 'original')
            self.assertEqual(run.call_count, 2)

    def test_capture_requires_ripgrep_and_hosted_disk_before_creating_outputs(self):
        for ripgrep, environment in [(None, {}), ('/usr/bin/rg', {})]:
            with self.subTest(ripgrep=ripgrep), tempfile.TemporaryDirectory() as temporary:
                output = Path(temporary) / 'capture'
                with patch.object(acceptance.shutil, 'which', return_value=ripgrep), \
                     patch.dict(os.environ, environment, clear=True):
                    with self.assertRaises(ValueError):
                        acceptance.run_capture(SimpleNamespace(output=output, mode='race', sample=1))
                self.assertFalse(output.exists())

    def test_capture_rejects_tmpfs_without_creating_outputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'capture'
            with patch.object(acceptance.shutil, 'which', return_value='/usr/bin/rg'), \
                 patch.dict(os.environ, {'GITHUB_ACTIONS': 'true'}), \
                 patch.object(acceptance.subprocess, 'check_output', return_value='tmpfs\n'):
                with self.assertRaises(ValueError):
                    acceptance.run_capture(SimpleNamespace(output=output, mode='race', sample=1))
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()
