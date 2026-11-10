#!/usr/bin/env python3
"""Report Go package elapsed/deadline ratios, including incomplete executions."""

import argparse
import json
import os
from pathlib import Path
import re


def timeout_seconds(value):
    parts = re.findall(r'(\d+(?:\.\d+)?)(h|m|s)', value)
    if not parts or ''.join(number + unit for number, unit in parts) != value:
        raise argparse.ArgumentTypeError('timeout must be a positive h/m/s duration')
    seconds = sum(float(number) * {'h': 3600, 'm': 60, 's': 1}[unit]
                  for number, unit in parts)
    if seconds <= 0:
        raise argparse.ArgumentTypeError('timeout must be positive')
    return seconds


def packages_from_jsonl(lines, deadline, tests=None):
    packages = {}
    malformed = 0
    for line in lines:
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            malformed += 1
            continue
        if not isinstance(event, dict) or not event.get('Package'):
            continue
        if event.get('Test'):
            if tests is not None and '/' not in event['Test']:
                record_test(tests, event)
            continue
        name = event['Package']
        if event.get('Action') == 'start':
            packages[name] = {'package': name, 'status': 'incomplete', 'elapsed_seconds': None,
                              'timeout_ratio': None}
        elif event.get('Action') in ('pass', 'fail', 'skip'):
            elapsed = event.get('Elapsed')
            packages[name] = {'package': name, 'status': event['Action'],
                              'elapsed_seconds': elapsed,
                              'timeout_ratio': elapsed / deadline if elapsed is not None else None}
    return sorted(packages.values(), key=lambda row: row['package']), malformed


def record_test(tests, event):
    key = (event['Package'], event['Test'])
    action = event.get('Action')
    if action not in ('run', 'pass', 'fail', 'skip'):
        return
    row = tests.setdefault(key, {'package': key[0], 'test': key[1], 'samples': [],
                                 'runs': 0, 'failed': False, 'status': 'incomplete'})
    if action == 'run':
        row['status'] = 'incomplete'
        return
    row['runs'] += 1
    row['failed'] |= action == 'fail'
    row['status'] = action
    elapsed = event.get('Elapsed')
    if isinstance(elapsed, (int, float)) and elapsed >= 0:
        row['samples'].append(elapsed)


def test_timings(tests, baseline=None):
    previous = {(row['package'], row['test']): row for row in (baseline or {}).get('tests', [])}
    rows = []
    for key, sample in tests.items():
        elapsed = sum(sample['samples']) / len(sample['samples']) if sample['samples'] else None
        before = previous.get(key, {}).get('elapsed_seconds')
        delta = elapsed - before if elapsed is not None and before is not None else None
        rows.append({'package': key[0], 'test': key[1],
                     'status': 'fail' if sample['failed'] else sample['status'],
                     'runs': sample['runs'], 'elapsed_seconds': elapsed,
                     'delta_seconds': delta,
                     'delta_ratio': delta / before if delta is not None and before > 0 else None})
    return sorted(rows, key=lambda row: (row['elapsed_seconds'] is None,
                                        -(row['elapsed_seconds'] or 0), row['package'], row['test']))


def render_tests(rows, top):
    output = ['', '### Slowest top-level tests', '',
              'Elapsed is the mean of completed runs; parent tests include their subtests. '
              'Deltas require an explicitly supplied report from a comparable run.', '',
              '| Package / Test | Result | Runs | Elapsed (s) | Change (s) |',
              '| --- | --- | ---: | ---: | ---: |']
    for row in rows[:top]:
        elapsed, delta = row['elapsed_seconds'], row['delta_seconds']
        seconds = f'{elapsed:.3f}' if elapsed is not None else 'unavailable'
        change = f'{delta:+.3f}' if delta is not None else 'unavailable'
        output.append(f'| `{row["package"]} / {row["test"]}` | {row["status"]} | '
                      f'{row["runs"]} | {seconds} | {change} |')
    if not rows:
        output.append('No top-level test results available.')
    return '\n'.join(output) + '\n'


def render(rows, deadline, malformed, cache_hit):
    output = ['### Package timeout budget', '',
              f'Package deadline: {deadline:g}s; warning above 75%.', '',
              f'setup-go cache-hit: {cache_hit or "unavailable"}. '
              'Cache KiB and command wall/user/system seconds are recorded above; '
              'restore/save transfers remain in setup-go logs.', '',
              '| Package | Result | Elapsed (s) | Timeout used |',
              '| --- | --- | ---: | ---: |']
    warnings = []
    for row in rows:
        elapsed, ratio = row['elapsed_seconds'], row['timeout_ratio']
        used = f'{ratio:.1%}' if ratio is not None else 'unavailable'
        seconds = f'{elapsed:.3f}' if elapsed is not None else 'unavailable'
        output.append(f'| `{row["package"]}` | {row["status"]} | {seconds} | {used} |')
        if ratio is not None and ratio > 0.75:
            warnings.append(f'{row["package"]}: {elapsed:.3f}s / {deadline:g}s '
                            f'({ratio:.1%}) exceeds 75% of the package timeout')
        elif ratio is None:
            warnings.append(f'{row["package"]}: package elapsed unavailable ({row["status"]})')
    if not rows:
        warnings.append('Package elapsed unavailable: no package results (discovery/build may have failed)')
    if malformed:
        warnings.append(f'Test evidence contains {malformed} malformed JSONL lines; timings may be incomplete')
    if warnings:
        output.extend(['', *['- Warning: ' + warning for warning in warnings]])
    return '\n'.join(output) + '\n', warnings


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--timeout', type=timeout_seconds, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    parser.add_argument('--summary', type=Path, required=True)
    parser.add_argument('--baseline', type=Path, help='previous package-budget.json from a comparable run')
    parser.add_argument('--top', type=int, default=20, help='number of slowest top-level tests to display')
    args = parser.parse_args()
    if args.top < 1:
        parser.error('--top must be positive')
    baseline = json.loads(args.baseline.read_text()) if args.baseline else None
    tests = {}
    args.evidence.mkdir(parents=True, exist_ok=True)
    source = args.evidence / 'tests.jsonl'
    with source.open() if source.exists() else open(os.devnull) as stream:
        rows, malformed = packages_from_jsonl(stream, args.timeout, tests)
    timings = test_timings(tests, baseline)
    cache_hit = os.environ.get('GO_CACHE_HIT', '')
    result = {'timeout_seconds': args.timeout, 'warning_ratio': 0.75,
              'setup_go_cache_hit': cache_hit or None, 'packages': rows,
              'malformed_lines': malformed, 'tests': timings,
              'baseline': str(args.baseline) if args.baseline else None}
    (args.evidence / 'package-budget.json').write_text(json.dumps(result, indent=2) + '\n')
    summary, warnings = render(rows, args.timeout, malformed, cache_hit)
    with args.summary.open('a') as stream:
        stream.write(summary)
        stream.write(render_tests(timings, args.top))
    for warning in warnings:
        # Escape workflow command data even when a package path is unusual.
        escaped = warning.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')
        print('::warning::' + escaped)


if __name__ == '__main__':
    main()
