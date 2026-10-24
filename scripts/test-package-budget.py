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


def packages_from_jsonl(lines, deadline):
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
        if not isinstance(event, dict) or not event.get('Package') or event.get('Test'):
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
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    source = args.evidence / 'tests.jsonl'
    with source.open() if source.exists() else open(os.devnull) as stream:
        rows, malformed = packages_from_jsonl(stream, args.timeout)
    cache_hit = os.environ.get('GO_CACHE_HIT', '')
    result = {'timeout_seconds': args.timeout, 'warning_ratio': 0.75,
              'setup_go_cache_hit': cache_hit or None, 'packages': rows,
              'malformed_lines': malformed}
    (args.evidence / 'package-budget.json').write_text(json.dumps(result, indent=2) + '\n')
    summary, warnings = render(rows, args.timeout, malformed, cache_hit)
    with args.summary.open('a') as stream:
        stream.write(summary)
    for warning in warnings:
        # Escape workflow command data even when a package path is unusual.
        escaped = warning.replace('%', '%25').replace('\r', '%0D').replace('\n', '%0A')
        print('::warning::' + escaped)


if __name__ == '__main__':
    main()
