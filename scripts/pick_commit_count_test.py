import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


SCRIPT = (Path(__file__).resolve().parents[1] / '.agents/skills/'
          'hufu-review-recent-commits/scripts/pick_commit_count.py')
spec = importlib.util.spec_from_file_location('pick_commit_count', SCRIPT)
picker = importlib.util.module_from_spec(spec)
spec.loader.exec_module(picker)


def accepted_workset():
    return subprocess.CompletedProcess([], 0, stdout=json.dumps({'outputs': {
        'general_item_count': 1,
        'item_count': 1,
        'scope': {'satisfied': True, 'observed_budget': {
            'changed_paths': 1, 'total_diff_lines': 2, 'total_diff_bytes': 30,
        }},
    }}), stderr='')


class CommitCountTests(unittest.TestCase):
    def test_probe_only_treats_native_capacity_codes_as_scope_rejections(self):
        for diagnostic, capacity in [
            ('scope_too_large\nobserved totals exceed the cap', True),
            ('workset_item_too_large: primary source exceeds the cap', True),
            ('documentation_verification_failed:\nmissing local reference', False),
            ('documentation_verification_failed:\nreference to scope_too_large is missing', False),
            ('execution_failed: command mentioned workset_item_too_large', False),
            ('scope_too_large_suffix: not a registered capacity code', False),
            ('', False),
        ]:
            with self.subTest(diagnostic=diagnostic), tempfile.TemporaryDirectory() as root:
                prober = picker.Prober('unused', '/unused', root, {'payload': {}})
                failed = subprocess.CompletedProcess([], 1, stdout='', stderr=diagnostic)
                with patch.object(picker.subprocess, 'run', return_value=failed), \
                        contextlib.redirect_stderr(io.StringIO()):
                    if capacity:
                        result = prober.probe(2)
                        self.assertFalse(result['ok'])
                        self.assertEqual(result['count'], 2)
                    else:
                        with self.assertRaises(SystemExit) as failure:
                            prober.probe(2)
                        self.assertIn('refusing to narrow', str(failure.exception))
                        self.assertIn(diagnostic, str(failure.exception))
                self.assertEqual(list(Path(root).iterdir()), [])

    def test_requested_count_is_lowered_only_for_capacity_failures(self):
        for diagnostic, expected in [
            ('', 2),
            ('scope_too_large\nobserved totals exceed the cap', 1),
            ('workset_item_too_large: primary source exceeds the cap', 1),
            ('documentation_verification_failed:\nmissing reference', None),
            ('documentation_verification_failed:\nreference to scope_too_large is missing', None),
            ('execution_failed: producer could not run', None),
        ]:
            with self.subTest(diagnostic=diagnostic), tempfile.TemporaryDirectory() as repo:
                counts = []

                def run_producer(_command, **kwargs):
                    request = json.loads(kwargs['input'])
                    count = json.loads(request['payload'])['scope']['count']
                    counts.append(count)
                    if diagnostic and count == 2:
                        return subprocess.CompletedProcess([], 1, stdout='', stderr=diagnostic)
                    return accepted_workset()

                limits = {'payload': {}, 'max_count': 10, 'workset_keys': 64, 'evidence_sources': 20}
                output = io.StringIO()
                with patch.object(picker.sys, 'argv', ['picker', '--repo', repo, '--count', '2']), \
                        patch.object(picker, 'git', side_effect=[repo, '5', 'head', '']), \
                        patch.object(picker, 'load_limits', return_value=limits), \
                        patch.object(picker, 'build_producer', return_value='unused'), \
                        patch.object(picker.subprocess, 'run', side_effect=run_producer), \
                        contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()):
                    if expected is None:
                        with self.assertRaises(SystemExit) as failure:
                            picker.main()
                        self.assertIn(diagnostic, str(failure.exception))
                    else:
                        picker.main()
                if expected is None:
                    self.assertEqual(counts, [2])
                    self.assertEqual(output.getvalue(), '')
                else:
                    result = json.loads(output.getvalue())
                    self.assertEqual(result['count'], expected)
                    self.assertTrue(result['selected']['ok'])
                    self.assertEqual(counts, [2, 1] if expected == 1 else [2])
                    self.assertEqual(bool(result['notes']), expected == 1)


if __name__ == '__main__':
    unittest.main()
