#!/usr/bin/env python3
"""Capture comparable go test observations without clearing the shared Go cache."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import platform
import resource
import shlex
import statistics
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_PACKAGES = ['./...', './.agent-teams/hufu-dev/diagnostic-pilot/...']


def output(command, env=None):
    return subprocess.check_output(command, cwd=ROOT, env=env, text=True).strip()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + '\n')


def summarize(path):
    packages, tests, skipped = {}, [], []
    cached = set()
    with path.open() as stream:
        for line in stream:
            event = json.loads(line)
            package, test = event.get('Package'), event.get('Test')
            if not test and '(cached)' in event.get('Output', ''):
                cached.add(package)
            if event.get('Action') not in ('pass', 'fail', 'skip'):
                continue
            row = {key: event[key] for key in ('Package', 'Test', 'Action', 'Elapsed') if key in event}
            if test:
                (skipped if event['Action'] == 'skip' else tests).append(row)
            elif package:
                packages[package] = row
    return {'packages': list(packages.values()),
            'cached_packages': sorted(cached), 'skipped_tests': skipped,
            'case_set_sha256': hashlib.sha256(json.dumps(sorted((row['Package'], row['Test'])
                                               for row in tests + skipped)).encode()).hexdigest(),
            'slowest_tests': sorted(tests, key=lambda row: row.get('Elapsed', 0), reverse=True)[:30],
            'test_counts': {action: sum(row['Action'] == action for row in tests + skipped)
                            for action in ('pass', 'fail', 'skip')}}


def measure(directory, name, command, env):
    print(f'{name}: {shlex.join(command)}', flush=True)
    cpu_before = resource.getrusage(resource.RUSAGE_CHILDREN)
    before = time.perf_counter()
    with (directory / f'{name}.jsonl').open('w') as stdout, (directory / f'{name}.stderr').open('w') as stderr:
        result = subprocess.run(command, cwd=ROOT, env=env, stdout=stdout, stderr=stderr, check=False)
    cpu_after = resource.getrusage(resource.RUSAGE_CHILDREN)
    row = {'cpu_user_seconds': cpu_after.ru_utime - cpu_before.ru_utime,
           'cpu_system_seconds': cpu_after.ru_stime - cpu_before.ru_stime,
           'name': name, 'command': command, 'exit_code': result.returncode,
           'wall_seconds': time.perf_counter() - before,
           **summarize(directory / f'{name}.jsonl')}
    write_json(directory / f'{name}.json', row)
    print(f"{name}: exit={result.returncode}, wall={row['wall_seconds']:.3f}s", flush=True)
    return row


def aggregate(rows):
    values = [row['wall_seconds'] for row in rows]
    package_names = sorted({p['Package'] for row in rows for p in row['packages']})
    return {'samples': len(rows), 'successful': all(row['exit_code'] == 0 for row in rows),
            'wall_seconds': {'median': statistics.median(values), 'min': min(values), 'max': max(values)},
            'cpu_user_seconds': stats([row['cpu_user_seconds'] for row in rows if 'cpu_user_seconds' in row]),
            'cpu_system_seconds': stats([row['cpu_system_seconds'] for row in rows if 'cpu_system_seconds' in row]),
            'case_sets': sorted({row['case_set_sha256'] for row in rows if 'case_set_sha256' in row}),
            'packages': {name: stats([p['Elapsed'] for row in rows for p in row['packages']
                                     if p['Package'] == name and 'Elapsed' in p])
                         for name in package_names}}


def stats(values):
    return {'samples': len(values), 'median': statistics.median(values), 'min': min(values), 'max': max(values)} if values else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True, help='new artifact directory (normally under work/)')
    parser.add_argument('--mode', choices=('normal', 'race', 'cgo-free'), default='normal')
    parser.add_argument('--cache-state', choices=('cold', 'warm', 'result', 'all'), default='warm')
    parser.add_argument('--repetitions', type=int, default=3)
    parser.add_argument('--package', action='append', help='repeat to replace the default full suite')
    parser.add_argument('--run', help='optional Go test filter; changes the measured case set')
    parser.add_argument('--full', action='store_true', help='omit -short (required for full release-contract validation)')
    parser.add_argument('--profile', action='store_true', help='capture a separate team CPU/allocation profile')
    args = parser.parse_args()
    if args.repetitions < 3:
        parser.error('at least three repetitions are required')
    env = os.environ.copy()
    if args.mode == 'cgo-free':
        env['CGO_ENABLED'] = '0'
    goenv = json.loads(output(['go', 'env', '-json', 'GOVERSION', 'GOOS', 'GOARCH', 'CGO_ENABLED', 'GOFLAGS', 'GOTOOLCHAIN', 'GOTMPDIR'], env))
    if goenv['GOVERSION'] != 'go1.27.2':
        parser.error(f"requires go1.27.2, found {goenv['GOVERSION']}")
    if shlex.split(goenv['GOFLAGS']) not in ([], ['-mod=readonly']):
        parser.error('GOFLAGS may only contain -mod=readonly; test flags must be explicit')
    if args.mode == 'race' and goenv['CGO_ENABLED'] != '1':
        parser.error('race measurement requires CGO_ENABLED=1')
    directory = args.output.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    packages = args.package or DEFAULT_PACKAGES
    flags = ['-json', '-timeout', '20m']
    if not args.full:
        flags += ['-short']
    if args.mode == 'race':
        flags += ['-race']
    if args.run:
        flags += ['-run', args.run]
    command = ['go', 'test', *flags]
    metadata = {'started_utc': datetime.datetime.now(datetime.timezone.utc).isoformat(),
                'commit': output(['git', 'rev-parse', 'HEAD']),
                'dirty': bool(output(['git', 'status', '--porcelain'])),
                'os': platform.platform(), 'machine': platform.machine(), 'logical_cpus': os.cpu_count(),
                'cpu': next((line.split(':', 1)[1].strip() for line in Path('/proc/cpuinfo').read_text().splitlines()
                             if line.startswith('model name')), 'unknown') if Path('/proc/cpuinfo').exists() else platform.processor(),
                'cpu_quota': Path('/sys/fs/cgroup/cpu.max').read_text().strip() if Path('/sys/fs/cgroup/cpu.max').exists() else None,
                'runner': {key: env.get(key) for key in ('CI', 'RUNNER_OS', 'RUNNER_ARCH')},
                'gomaxprocs_configured': env.get('GOMAXPROCS', 'runtime default'), 'go': goenv,
                'mode': args.mode, 'packages': packages, 'flags': flags,
                'cache_scope': 'private build cache; module, OS page and filesystem caches remain warm'}
    # Ask the selected toolchain for its container-aware runtime default.
    with tempfile.TemporaryDirectory(prefix='gomaxprocs-', dir=directory) as temporary:
        probe = Path(temporary) / 'main.go'
        probe.write_text('package main\nimport ("fmt"; "runtime")\nfunc main() { fmt.Println(runtime.GOMAXPROCS(0)) }\n')
        metadata['gomaxprocs_effective'] = int(output(['go', 'run', str(probe)], env))
    write_json(directory / 'environment.json', metadata)
    groups, failed = {}, False
    for iteration in range(1, args.repetitions + 1):
        with tempfile.TemporaryDirectory(prefix='build-cache-', dir=directory) as cache:
            runenv = {**env, 'GOCACHE': cache}
            # One private cache per repetition. Always execute the preceding states
            # so that warm/result observations have an explicit, reproducible seed.
            for state in ('cold', 'warm', 'result-seed', 'result'):
                name = f'{state}-{iteration}'
                count = [] if state.startswith('result') else ['-count=1']
                row = measure(directory, name, [*command, *count, *packages], runenv)
                failed |= row['exit_code'] != 0
                if state != 'result-seed':
                    groups.setdefault(state, []).append(row)
                write_json(directory / 'summary.json', {key: aggregate(rows) for key, rows in groups.items()})
                if failed:
                    return 1  # Preserve failing evidence; never retry or call a failure a speedup.
                if state == args.cache_state:
                    break
    if args.profile:
        profile_flags = ['-count=1', '-timeout', '20m'] + (['-short'] if not args.full else [])
        if args.mode == 'race':
            profile_flags += ['-race']
        if args.run:
            profile_flags += ['-run', args.run]
        profile_command = ['go', 'test', '-json', *profile_flags,
                           f'-cpuprofile={directory / "team.cpu"}', f'-memprofile={directory / "team.mem"}',
                           '-o', str(directory / 'team.test'), './internal/team']
        row = measure(directory, 'profile', profile_command, env)
        if row['exit_code']:
            return 1
        for kind in ('cpu', 'mem'):
            with (directory / f'team.{kind}.top.txt').open('w') as stream:
                subprocess.run(['go', 'tool', 'pprof', '-top', str(directory / 'team.test'), str(directory / f'team.{kind}')],
                               cwd=ROOT, env=env, stdout=stream, check=True)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
