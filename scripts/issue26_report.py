"""Audit all nine planned capture ZIPs without extracting or replacing raw evidence."""

import argparse
import hashlib
import json
from pathlib import Path
import statistics
import zipfile

from issue26_acceptance import FIX, NATIVE_GREP, SOURCES

PACKAGES = ['./...', './.agent-teams/hufu-dev/diagnostic-pilot/...']


def digest_file(path):
    with path.open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def inventory(stream):
    terminal = {}
    failed_packages = set()
    for line in stream:
        event = json.loads(line)
        if event.get('Action') not in ('pass', 'fail', 'skip'):
            continue
        if event.get('Test'):
            key = (event['Package'], event['Test'])
            if key in terminal:
                raise ValueError('duplicate terminal test event')
            terminal[key] = event['Action']
        elif event['Action'] == 'fail':
            failed_packages.add(event['Package'])
    top = sorted([package, name, outcome] for (package, name), outcome in terminal.items() if '/' not in name)
    nested = sorted([package, name, outcome] for (package, name), outcome in terminal.items())
    native = {name: terminal.get(('github.com/kjelly/hufu/internal/tools', name)) for name in NATIVE_GREP}
    return {'top_level': top, 'all_case_count': len(nested),
            'all_case_outcome_sha256': hashlib.sha256(json.dumps(nested).encode()).hexdigest(),
            'skipped': [row for row in nested if row[2] == 'skip'],
            'failed_packages': sorted(failed_packages),
            'failed_tests': [row for row in nested if row[2] == 'fail'],
            'native_grep_passed': all(value == 'pass' for value in native.values())}


def inspect_archive(path):
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)):
            raise ValueError('duplicate ZIP members')
        manifests = [name for name in names if name == 'capture.json' or name.endswith('/capture.json')]
        if len(manifests) != 1:
            raise ValueError('expected exactly one capture manifest')
        prefix = manifests[0][:-len('capture.json')]
        manifest = json.loads(archive.read(manifests[0]))
        mode, sample = manifest['mode'], manifest['sample_index']
        if mode not in ('normal', 'race', 'cgo-free') or type(sample) is not int or sample not in (1, 2, 3):
            raise ValueError('invalid mode or planned sample identity')
        if manifest['experiment'] != 'issue26-fixture-remediated-v1' \
                or manifest['samples_required'] != 3 or manifest['timeout_seconds'] != 1800 \
                or manifest['full_suite'] is not True or 'error' in manifest \
                or set(manifest['sources']) != set(SOURCES):
            raise ValueError('incomplete or mismatched experiment manifest')
        rows = {}
        for label, original in SOURCES.items():
            source = manifest['sources'][label]
            if source['original_revision'] != original or source['fixture_fix'] != FIX \
                    or source['measured_revision'] == original or source['exit_code'] != 0 \
                    or source['native_grep']['passed'] is not True:
                raise ValueError('invalid source identity, failed capture or missing native grep coverage')
            base = prefix + label + '/'
            env = json.loads(archive.read(base + 'environment.json'))
            expected_flags = ['-p', '2', '-json', '-timeout', '30m'] + (['-race'] if mode == 'race' else [])
            if env['commit'] != source['measured_revision'] or env['dirty'] is not False \
                    or env['sample_index'] != sample or env['mode'] != mode \
                    or env['repetitions_required'] != 3 or env['flags'] != expected_flags \
                    or env['packages'] != PACKAGES or str(env['gomaxprocs_configured']) != '2' \
                    or env['gomaxprocs_effective'] != 2 or env['go']['GOVERSION'] != 'go1.27.2' \
                    or env['go']['GOTOOLCHAIN'] != 'local' or env['go']['GOFLAGS'] != '-mod=readonly' \
                    or env['go']['CGO_ENABLED'] != ('0' if mode == 'cgo-free' else '1'):
                raise ValueError('source environment does not match experiment')
            observations = {}
            for state in ('cold', 'warm', 'result-seed', 'result'):
                name = f'{state}-{sample}'
                observation = json.loads(archive.read(base + name + '.json'))
                command = ['go', 'test', *expected_flags]
                if state in ('cold', 'warm'):
                    command.append('-count=1')
                command += env['packages']
                if observation['exit_code'] != 0 or observation['name'] != name \
                        or observation['command'] != command:
                    raise ValueError('failed, substituted or differently filtered observation')
                with archive.open(base + name + '.jsonl') as stream:
                    cases = inventory(stream)
                if not cases['top_level'] or cases['failed_packages'] or cases['failed_tests'] \
                        or not cases['native_grep_passed']:
                    raise ValueError('raw outcomes contradict successful observation')
                if base + name + '.stderr' not in names:
                    raise ValueError('missing raw stderr evidence')
                observations[state] = {'observation': observation, 'inventory': cases}
            rows[label] = {'identity': source, 'environment': env, 'observations': observations}
        environments = [row['environment'] for row in rows.values()]
        for key in ('cpu', 'os', 'machine', 'logical_cpus', 'cpu_quota'):
            if environments[0][key] != environments[1][key]:
                raise ValueError('paired sources do not share their declared runner environment')
        return {'archive': path.name, 'sha256': digest_file(path), 'manifest': manifest, 'sources': rows}


def stats(values):
    return {'samples': len(values), 'median': statistics.median(values), 'min': min(values), 'max': max(values)}


def aggregate(captures):
    expected = {(mode, sample) for mode in ('normal', 'race', 'cgo-free') for sample in (1, 2, 3)}
    actual = [(row['manifest']['mode'], row['manifest']['sample_index']) for row in captures]
    if len(actual) != len(set(actual)) or set(actual) != expected:
        raise ValueError('all nine distinct planned samples are required; no substitutes or extra attempts')
    for key in ('controller_revision', 'harness_sha256', 'run_id', 'run_attempt'):
        if len({row['manifest'][key] for row in captures}) != 1:
            raise ValueError('samples belong to different controllers or experiment attempts')
    groups = {}
    for label in SOURCES:
        if len({row['sources'][label]['identity']['measured_revision'] for row in captures}) != 1:
            raise ValueError('source variants changed between samples')
        for mode in ('normal', 'race', 'cgo-free'):
            sources = [row['sources'][label] for row in captures if row['manifest']['mode'] == mode]
            # Same hardware model within a mode; individual paired sources also
            # share their exact runner. Quotas remain reported even when unknown.
            if len({row['environment']['cpu'] for row in sources}) != 1:
                raise ValueError('CPU models differ within a pooled mode')
            for key in ('os', 'machine', 'logical_cpus', 'cpu_quota'):
                if len({json.dumps(row['environment'][key]) for row in sources}) != 1:
                    raise ValueError('runner environment differs within a pooled mode')
            for state in ('cold', 'warm', 'result-seed', 'result'):
                rows = [source['observations'][state] for source in sources]
                inventories = [json.dumps(row['inventory']['top_level']) for row in rows]
                if len(set(inventories)) != 1:
                    raise ValueError('top-level case/outcome set changed between samples')
                groups[f'{label}/{mode}/{state}'] = {
                    key: stats([row['observation'][key] for row in rows])
                    for key in ('wall_seconds', 'cpu_user_seconds', 'cpu_system_seconds')}
    return {'capture_matrix_passed': True, 'acceptance_complete': False,
            'remaining_semantic_gates': ['assertion preservation review', 'CI cost/cache/flake interpretation'],
            'groups': groups, 'captures': captures}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archives', type=Path, nargs='+')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    result = aggregate([inspect_archive(path) for path in args.archives])
    args.output.write_text(json.dumps(result, indent=2, sort_keys=True) + '\n')
    print('Capture matrix validated; semantic acceptance gates still require review.')


if __name__ == '__main__':
    main()
