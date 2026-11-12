#!/usr/bin/env python3
"""Validate PR evidence declarations, never execute their commands."""

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import sys
import urllib.parse
import urllib.request


class EvidenceError(ValueError):
    pass


def text(value):
    return isinstance(value, str) and bool(value.strip())


def references(value):
    return isinstance(value, list) and bool(value) and all(text(v) for v in value)


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def unique_object(pairs):
    result = {}
    for name, value in pairs:
        require(name not in result, 'duplicate JSON member')
        result[name] = value
    return result


def reject_constant(_):
    raise EvidenceError('non-finite JSON constants are forbidden')


def parse_block(body):
    require(body is None or isinstance(body, str), 'PR body must be text')
    blocks, current = [], None
    for line in (body or '').splitlines():
        if line.strip() == '```hufu-fix-evidence':
            require(current is None, 'nested evidence block')
            current = []
        elif line.strip() == '```' and current is not None:
            blocks.append('\n'.join(current))
            current = None
        elif current is not None:
            current.append(line)
    require(current is None and len(blocks) == 1,
            'include exactly one closed hufu-fix-evidence JSON block')
    try:
        value = json.loads(blocks[0], object_pairs_hook=unique_object,
                           parse_constant=reject_constant)
    except (ValueError, RecursionError) as exc:
        raise EvidenceError('evidence block must be valid JSON without duplicate members') from exc
    require(isinstance(value, dict), 'evidence must be an object')
    return value


def documentation_only(paths):
    return all(
        path in {'AGENTS.md', 'README.md', 'CONTRIBUTING.md'}
        or (path.startswith('docs/') and path.endswith(('.md', '.rst')))
        for path in paths
    )


def validate(value, root):
    kind = value.get('kind')
    require(isinstance(kind, str) and kind in {'bugfix', 'behavior', 'maintenance'}, 'invalid change kind')
    require(text(value.get('acceptance')), 'acceptance must describe an observable outcome')
    cause = value.get('cause_status', 'not_applicable')
    require(isinstance(cause, str) and cause in {'confirmed', 'hypothesis', 'unresolved', 'not_applicable'},
            'invalid cause_status')
    if kind == 'bugfix':
        require(cause != 'not_applicable' and text(value.get('cause')),
                'bug fixes must explain the cause or missing evidence')
    if cause == 'confirmed':
        require(text(value.get('cause')) and references(value.get('cause_evidence')),
                'confirmed causes require an explanation and evidence')

    regressions = value.get('regressions', [])
    require(isinstance(regressions, list), 'regressions must be an array')
    if kind in {'bugfix', 'behavior'}:
        require(bool(regressions), 'bug fixes and behavior changes need regression evidence')
    for regression in regressions:
        require(isinstance(regression, dict), 'regression must be an object')
        for field in ('path', 'scenario', 'before', 'after'):
            require(text(regression.get(field)), 'regression needs path, scenario, before and after')
        path = PurePosixPath(regression['path'])
        require(not path.is_absolute() and '..' not in path.parts, 'test path must stay in the repository')
        target = (root / str(path)).resolve()
        require(target.is_relative_to(root.resolve()) and target.is_file(),
                'regression must reference an existing repository file')
        require(path.name.endswith(('_test.go', '_test.py'))
                or (path.name.startswith('test_') and path.suffix == '.py'),
                'regression must reference a Go or Python test')

    checks = value.get('checks')
    require(isinstance(checks, list) and bool(checks), 'source checks are required')
    for check in checks:
        require(isinstance(check, dict) and text(check.get('command'))
                and text(check.get('evidence')), 'checks need command and evidence')
        require(isinstance(check.get('result'), str)
                and check['result'] in {'passed', 'failed', 'not_run'}, 'invalid check result')

    workflow = value.get('workflow')
    require(isinstance(workflow, dict), 'workflow declaration is required')
    level, claim = workflow.get('level'), workflow.get('claim')
    require(isinstance(level, str) and level in {'offline', 'bounded_probe', 'full_target', 'not_applicable'},
            'invalid workflow level')
    require(isinstance(claim, str) and claim in {'partial', 'source_fix', 'workflow_complete'}, 'invalid completion claim')
    require(text(workflow.get('scope')), 'workflow scope must identify the tested boundary')
    if level != 'not_applicable':
        require(references(workflow.get('evidence')), 'workflow evidence is required')
    if level != 'full_target' or claim != 'workflow_complete':
        require(text(workflow.get('limitations')), 'state workflow limitations or why it does not apply')
    if claim != 'partial':
        require(all(check['result'] == 'passed' for check in checks),
                'completion claims require passed source checks')
        require(kind != 'bugfix' or cause == 'confirmed',
                'a source fix cannot be claimed for an unresolved bug cause')
    if claim == 'workflow_complete':
        require(level == 'full_target', 'offline tests or probes do not prove full-target completion')


def changed_paths(root, pull_request):
    commits = [pull_request[side]['sha'] for side in ('base', 'head')]
    require(all(isinstance(sha, str) and len(sha) in (40, 64)
                and all(c in '0123456789abcdef' for c in sha) for sha in commits),
            'base and head must be commit SHAs')
    # Disable rename detection: moving source into docs must still count as a
    # source deletion rather than becoming a documentation-only exemption.
    result = subprocess.run(
        ['git', 'diff', '--no-renames', '--name-only', '-z',
         f'{commits[0]}...{commits[1]}', '--'],
        cwd=root, check=True, capture_output=True,
    )
    return [p.decode('utf-8') for p in result.stdout.split(b'\0') if p]


def refresh_description(event, token):
    require(text(token), 'a read-only GitHub token is required to refresh PR evidence')
    pr = event['pull_request']
    number = pr['number']
    require(type(number) is int and number > 0, 'invalid PR number')
    repo = event['repository']['full_name']
    require(isinstance(repo, str) and len(repo.split('/')) == 2
            and all(part and part not in {'.', '..'} for part in repo.split('/')),
            'invalid repository identity')
    request = urllib.request.Request(
        'https://api.github.com/repos/' + urllib.parse.quote(repo, safe='/') + f'/pulls/{number}',
        headers={'Accept': 'application/vnd.github+json', 'Authorization': 'Bearer ' + token},
    )
    with urllib.request.urlopen(request, timeout=15) as response:
        raw = response.read((1 << 20) + 1)
    require(len(raw) <= 1 << 20, 'PR metadata exceeds the read budget')
    current = json.loads(raw)
    require(isinstance(current, dict), 'invalid current PR metadata')
    require(all(current[side]['sha'] == pr[side]['sha'] for side in ('base', 'head')),
            'PR source revision changed; validate the current revision')
    # Refresh only prose. Source selection stays bound to the CI event's SHAs.
    return {**pr, 'body': current.get('body')}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--event', type=Path, default=os.environ.get('GITHUB_EVENT_PATH'))
    parser.add_argument('--root', type=Path, default=Path.cwd())
    parser.add_argument('--refresh-pr', action='store_true',
                        help='read the current PR description, bound to the event revision')
    args = parser.parse_args()
    try:
        require(args.event is not None, '--event or GITHUB_EVENT_PATH is required')
        event = json.loads(Path(args.event).read_text())
        require(isinstance(event, dict), 'event must be an object')
        pr = event.get('pull_request')
        if pr is None:
            require(not args.refresh_pr, 'PR metadata is required for the PR evidence gate')
            print('No PR payload: description evidence does not apply to this event.')
            return 0
        require(isinstance(pr, dict), 'pull_request must be an object')
        if documentation_only(changed_paths(args.root, pr)):
            print('Documentation-only diff: no source-fix declaration required.')
            return 0
        if args.refresh_pr:
            pr = refresh_description(event, os.environ.get('GH_TOKEN'))
        validate(parse_block(pr.get('body')), args.root)
    except (EvidenceError, OSError, ValueError, KeyError, TypeError,
            subprocess.CalledProcessError, RecursionError) as exc:
        # Do not echo PR contents, private artifact URLs or git command output.
        print(f'Fix evidence rejected: {exc if isinstance(exc, EvidenceError) else "invalid event or unavailable repository diff"}', file=sys.stderr)
        return 1
    print('Evidence declarations are consistent; RCA truth and workflow success still require review.')
    return 0


if __name__ == '__main__':
    sys.exit(main())
