"""Verify immutable measurement variants and fail-closed experiment admission."""

import os
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import issue26_acceptance as acceptance


class VariantTests(unittest.TestCase):
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
