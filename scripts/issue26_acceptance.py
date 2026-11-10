"""Run explicitly remediated historical sources; never rewrite the original evidence."""

import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
SOURCES = {
    'before': '8f65a57cca7470aa773a28f7c70bfaa5c6b4f550',
    'after-32': 'b3360c9a5804e83934204d8118c5c322e50a94e9',
}
FIX = '300041b84e45e7c53549bd53c753b6b0ac6ad860'
FIX_PATH = 'internal/team/dag_test.go'
NATIVE_GREP = {
    'TestGrepPolicySubtractsFromNativeRgSelection',
    'TestGrepNoPolicyPreservesNativeDirectorySelection',
    'TestGrepExplicitIgnoredFilePreservesDirectOperand',
    'TestGrepExplicitIgnoredDirectorySubtractsBlockedDescendants',
    'TestGrepLeadingDashPatternIsDataForNativeAndFallbackBackends/native',
    'TestGrepLeadingDashPatternSurvivesPolicyCandidateSelection/native',
}


def git(*args, cwd=None, **kwargs):
    return subprocess.check_output(['git', *args], cwd=cwd or ROOT, text=True, **kwargs).strip()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + '\n')


def native_grep_coverage(directory):
    """Installation alone does not prove execution of the six native contracts."""
    checked = {}
    for path in sorted(directory.glob('*.jsonl')):
        outcomes = {}
        with path.open() as stream:
            for line in stream:
                event = json.loads(line)
                if event.get('Package') == 'github.com/kjelly/hufu/internal/tools' \
                        and event.get('Test') in NATIVE_GREP \
                        and event.get('Action') in ('pass', 'fail', 'skip'):
                    outcomes[event['Test']] = event['Action']
        missing = sorted(name for name in NATIVE_GREP if outcomes.get(name) != 'pass')
        checked[path.name] = {'passed': not missing, 'missing_or_not_passed': missing}
    return {'passed': len(checked) == 4 and all(row['passed'] for row in checked.values()),
            'commands': checked}


def remediate(source, sha):
    """Apply the exact already-reviewed fixture fix to a new, distinct commit."""
    patch = git('diff', FIX + '^', FIX, '--', FIX_PATH) + '\n'
    subprocess.run(['git', 'worktree', 'add', '--detach', str(source), sha], cwd=ROOT, check=True)
    subprocess.run(['git', 'apply', '--check', '-'], cwd=source, input=patch, text=True, check=True)
    subprocess.run(['git', 'apply', '-'], cwd=source, input=patch, text=True, check=True)
    subprocess.run(['git', 'add', '--', FIX_PATH], cwd=source, check=True)
    changed = git('diff', '--cached', '--name-only', cwd=source).splitlines()
    if changed != [FIX_PATH]:
        raise ValueError('fixture remediation must change exactly the reviewed test file')
    tree = git('write-tree', cwd=source)
    env = {**os.environ,
           'GIT_AUTHOR_NAME': 'Hufu measurement fixture', 'GIT_AUTHOR_EMAIL': 'measurement@example.invalid',
           'GIT_COMMITTER_NAME': 'Hufu measurement fixture', 'GIT_COMMITTER_EMAIL': 'measurement@example.invalid',
           'GIT_AUTHOR_DATE': '2026-10-25T00:52:56+00:00',
           'GIT_COMMITTER_DATE': '2026-10-25T00:52:56+00:00'}
    revision = git('commit-tree', tree, '-p', sha, env=env,
                   input=f'Measurement variant: {sha} plus fixture drain from {FIX}\n')
    subprocess.run(['git', 'checkout', '--detach', revision], cwd=source, check=True)
    if git('status', '--porcelain', cwd=source):
        raise ValueError('measurement variant is not clean')
    return {'original_revision': sha, 'measured_revision': revision,
            'fixture_fix': FIX, 'fixture_path': FIX_PATH,
            'patch_sha256': hashlib.sha256(patch.encode()).hexdigest(), 'tree': tree}


def run_capture(args):
    if not shutil.which('rg'):
        raise ValueError('ripgrep is required; native grep contracts must not silently skip')
    # The private cold caches are an intentional experiment on a hosted disk.
    # Never place those caches on this Codex machine's tmpfs or alter shared caches.
    if not os.environ.get('GITHUB_ACTIONS') == 'true':
        raise ValueError('full captures require the dedicated hosted workflow')
    disk = subprocess.check_output(['stat', '-f', '-c', '%T', str(args.output.parent)], text=True).strip()
    if disk in ('tmpfs', 'ramfs'):
        raise ValueError('measurement build caches require disk-backed storage')
    args.output.mkdir(parents=True, exist_ok=False)
    started = time.time()
    manifest = {'experiment': 'issue26-fixture-remediated-v1', 'mode': args.mode,
                'sample_index': args.sample, 'samples_required': 3,
                'controller_revision': git('rev-parse', 'HEAD'),
                'harness_sha256': hashlib.sha256((ROOT / 'scripts/test-baseline.py').read_bytes()).hexdigest(),
                'timeout_seconds': 1800, 'full_suite': True, 'sources': {},
                'run_id': os.environ['GITHUB_RUN_ID'], 'run_attempt': os.environ['GITHUB_RUN_ATTEMPT'],
                'ripgrep': subprocess.check_output(['rg', '--version'], text=True).splitlines()[0],
                'order': ['before', 'after-32'] if args.sample % 2 else ['after-32', 'before']}
    write_json(args.output / 'capture.json', manifest)
    for label in manifest['order']:
        source = args.output.parent / f'source-{args.mode}-{args.sample}-{label}'
        try:
            identity = remediate(source, SOURCES[label])
            manifest['sources'][label] = identity
            write_json(args.output / 'capture.json', manifest)
            with (args.output / f'{label}-modules.log').open('w') as log:
                subprocess.run(['mise', 'exec', '--', 'go', 'mod', 'download'], cwd=source,
                               stdout=log, stderr=subprocess.STDOUT, check=True)
            spec = importlib.util.spec_from_file_location('baseline', ROOT / 'scripts/test-baseline.py')
            baseline = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(baseline)
            baseline.ROOT = source
            sys.argv = [str(ROOT / 'scripts/test-baseline.py'), '--output', str(args.output / label),
                        '--mode', args.mode, '--cache-state', 'all', '--full',
                        '--sample-index', str(args.sample), '--timeout-seconds', '1800']
            identity['exit_code'] = baseline.main()
            identity['native_grep'] = native_grep_coverage(args.output / label)
            write_json(args.output / 'capture.json', manifest)
            if identity['exit_code'] or not identity['native_grep']['passed']:
                # No retry, no replacement sample and no continuing with a passing subset.
                return 1
        except Exception as error:
            manifest['error'] = {'source': label, 'type': type(error).__name__, 'detail': str(error)}
            write_json(args.output / 'capture.json', manifest)
            raise
        finally:
            if source.exists():
                subprocess.run(['git', 'worktree', 'remove', str(source)], cwd=ROOT, check=True)
            manifest['elapsed_seconds'] = time.time() - started
            write_json(args.output / 'capture.json', manifest)
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--mode', choices=('normal', 'race', 'cgo-free'), required=True)
    parser.add_argument('--sample', type=int, choices=(1, 2, 3), required=True)
    return run_capture(parser.parse_args())


if __name__ == '__main__':
    raise SystemExit(main())
