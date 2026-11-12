import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).with_name('check-fix-evidence.py')
spec = importlib.util.spec_from_file_location('fix_evidence', SCRIPT)
evidence = importlib.util.module_from_spec(spec)
spec.loader.exec_module(evidence)


def declaration():
    return {
        'kind': 'bugfix',
        'acceptance': 'Rejected output preserves completed batches and source data.',
        'cause_status': 'confirmed',
        'cause': 'The repair request omitted the failing constraint.',
        'cause_evidence': ['redacted request/response artifact'],
        'regressions': [{'path': 'internal/example_test.go', 'scenario': 'invalid output then repair',
                         'before': 'baseline rejection artifact', 'after': 'fixed integration artifact'}],
        'checks': [{'command': 'test fixture', 'result': 'passed', 'evidence': 'test output artifact'}],
        'workflow': {'level': 'offline', 'claim': 'source_fix', 'scope': 'scripted provider fixture',
                     'evidence': ['fixture results'], 'limitations': 'Live target was not executed.'},
    }


def body(value):
    return '```hufu-fix-evidence\n' + json.dumps(value) + '\n```'


class FixEvidenceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / 'internal').mkdir()
        (self.root / 'internal/example_test.go').write_text('package example\n')

    def test_completion_claims_require_matching_evidence(self):
        cases = [
            ('offline source fix', 'offline', 'source_fix', 'confirmed', 'passed', True),
            ('probe source fix', 'bounded_probe', 'source_fix', 'confirmed', 'passed', True),
            ('offline full claim', 'offline', 'workflow_complete', 'confirmed', 'passed', False),
            ('probe full claim', 'bounded_probe', 'workflow_complete', 'confirmed', 'passed', False),
            ('full target', 'full_target', 'workflow_complete', 'confirmed', 'passed', True),
            ('unresolved fix', 'offline', 'source_fix', 'unresolved', 'passed', False),
            ('honest partial', 'offline', 'partial', 'unresolved', 'not_run', True),
            ('failed completion', 'full_target', 'workflow_complete', 'confirmed', 'failed', False),
        ]
        for name, level, claim, cause, result, accepted in cases:
            with self.subTest(name=name):
                value = declaration()
                value['workflow'].update(level=level, claim=claim)
                value['cause_status'] = cause
                value['checks'][0]['result'] = result
                if accepted:
                    evidence.validate(value, self.root)
                else:
                    with self.assertRaises(evidence.EvidenceError):
                        evidence.validate(value, self.root)

    def test_incomplete_or_unsafe_evidence_is_rejected(self):
        for field, replacement in [('acceptance', ''), ('regressions', []),
                                   ('cause_evidence', []), ('checks', []), ('kind', [])]:
            with self.subTest(field=field):
                value = declaration()
                value[field] = replacement
                with self.assertRaises(evidence.EvidenceError):
                    evidence.validate(value, self.root)
        for path in ['../outside_test.go', '/outside_test.go', 'internal/missing_test.go',
                     'internal/example.go']:
            with self.subTest(path=path):
                value = declaration()
                value['regressions'][0]['path'] = path
                with self.assertRaises(evidence.EvidenceError):
                    evidence.validate(value, self.root)
        outside = self.root.parent / (self.root.name + '-outside_test.go')
        outside.write_text('private fixture')
        self.addCleanup(outside.unlink)
        (self.root / 'internal/link_test.go').symlink_to(outside)
        value = declaration()
        value['regressions'][0]['path'] = 'internal/link_test.go'
        with self.assertRaises(evidence.EvidenceError):
            evidence.validate(value, self.root)

    def test_block_parser_does_not_guess_from_prose_or_duplicate_members(self):
        valid = body(declaration())
        self.assertEqual(evidence.parse_block(valid), declaration())
        for invalid in ['', 'tests passed; the workflow is fixed', valid + '\n' + valid,
                        '```hufu-fix-evidence\n{"kind":"bugfix","kind":"maintenance"}\n```',
                        '```hufu-fix-evidence\n{"checks":NaN}\n```',
                        '```hufu-fix-evidence\n{}', '```hufu-fix-evidence\n[]\n```']:
            with self.subTest(invalid=invalid[:40]):
                with self.assertRaises(evidence.EvidenceError):
                    evidence.parse_block(invalid)

    def test_description_refresh_preserves_revision_and_fails_closed(self):
        pr = {'number': 12, 'base': {'sha': 'a' * 40}, 'head': {'sha': 'b' * 40}, 'body': 'stale'}
        event = {'pull_request': pr, 'repository': {'full_name': 'owner/repository'}}
        current = {**pr, 'body': body(declaration())}
        with mock.patch.object(evidence.urllib.request, 'urlopen',
                               return_value=io.BytesIO(json.dumps(current).encode())) as request:
            refreshed = evidence.refresh_description(event, 'fixture-token')
        self.assertEqual(refreshed['body'], current['body'])
        self.assertEqual(refreshed['head'], pr['head'])
        self.assertEqual(pr['body'], 'stale')
        self.assertEqual(request.call_args.kwargs['timeout'], 15)
        evidence.validate(evidence.parse_block(refreshed['body']), self.root)
        current['head'] = {'sha': 'c' * 40}
        with mock.patch.object(evidence.urllib.request, 'urlopen',
                               return_value=io.BytesIO(json.dumps(current).encode())):
            with self.assertRaises(evidence.EvidenceError):
                evidence.refresh_description(event, 'fixture-token')
        with mock.patch.object(evidence.urllib.request, 'urlopen', side_effect=OSError('offline')):
            with self.assertRaises(OSError):
                evidence.refresh_description(event, 'fixture-token')
        with self.assertRaises(evidence.EvidenceError):
            evidence.refresh_description(event, '')

    def git(self, *args):
        return subprocess.check_output(
            ['git', '-c', 'user.name=fixture', '-c', 'user.email=fixture@example.invalid',
             '-c', 'commit.gpgsign=false', '-c', 'core.hooksPath=/dev/null', *args],
            cwd=self.root, stderr=subprocess.DEVNULL, text=True,
        ).strip()

    def run_check(self, base, head, description):
        event = self.root / 'event.json'
        event.write_text(json.dumps({'pull_request': {
            'base': {'sha': base}, 'head': {'sha': head}, 'body': description,
        }}))
        return subprocess.run([sys.executable, str(SCRIPT), '--event', str(event),
                               '--root', str(self.root)], capture_output=True, text=True)

    def test_cli_checks_real_diff_without_executing_pr_commands(self):
        self.git('init', '-q')
        source = self.root / 'internal/example.go'
        source.write_text('package example\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'baseline')
        base = self.git('rev-parse', 'HEAD')
        source.write_text('package example\n// change\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'change')
        head = self.git('rev-parse', 'HEAD')
        self.assertNotEqual(self.run_check(base, head, '').returncode, 0)
        value = declaration()
        marker = self.root / 'must-not-execute'
        value['checks'][0]['command'] = f'$(touch {marker})'
        self.assertEqual(self.run_check(base, head, body(value)).returncode, 0)
        self.assertFalse(marker.exists())
        value['workflow']['claim'] = 'workflow_complete'
        self.assertNotEqual(self.run_check(base, head, body(value)).returncode, 0)

    def test_pr_gate_cannot_treat_missing_payload_as_push_exemption(self):
        event = self.root / 'event.json'
        event.write_text('{}')
        args = [sys.executable, str(SCRIPT), '--event', str(event), '--root', str(self.root)]
        self.assertEqual(subprocess.run(args, capture_output=True).returncode, 0)
        self.assertNotEqual(subprocess.run(args + ['--refresh-pr'], capture_output=True).returncode, 0)

    def test_doc_exemption_cannot_hide_source_moves_or_prompt_changes(self):
        self.assertTrue(evidence.documentation_only(['AGENTS.md', 'docs/reference/guide.md']))
        for paths in [['.agent-teams/team/worker.md'], ['.agents/skills/example/SKILL.md'],
                      ['.github/workflows/ci.yml'], ['docs/schema.json']]:
            self.assertFalse(evidence.documentation_only(paths))
        self.git('init', '-q')
        source = self.root / 'internal/example.go'
        source.write_text('package example\n')
        self.git('add', '.')
        self.git('commit', '-qm', 'baseline')
        base = self.git('rev-parse', 'HEAD')
        (self.root / 'docs').mkdir()
        source.rename(self.root / 'docs/example.md')
        self.git('add', '.')
        self.git('commit', '-qm', 'move')
        head = self.git('rev-parse', 'HEAD')
        self.assertNotEqual(self.run_check(base, head, '').returncode, 0)


if __name__ == '__main__':
    unittest.main()
