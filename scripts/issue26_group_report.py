"""Validate three co-located observations in every mode/cache cohort."""

import argparse
import json
from pathlib import Path
import zipfile

from issue26_acceptance import FIX, PATCH_SHA256, SOURCES, VARIANTS
from issue26_report import PACKAGES, digest_file, inventory, stats


def inspect_group(path):
    with zipfile.ZipFile(path) as archive:
        names = archive.namelist()
        if len(names) != len(set(names)):
            raise ValueError('duplicate ZIP members')
        manifests = [name for name in names if name == 'capture.json' or name.endswith('/capture.json')]
        if len(manifests) != 1:
            raise ValueError('expected one group manifest')
        prefix = manifests[0][:-len('capture.json')]
        manifest = json.loads(archive.read(manifests[0]))
        mode, state = manifest['mode'], manifest['cache_state']
        if mode not in ('normal', 'race', 'cgo-free') or state not in ('cold', 'warm', 'result') \
                or manifest['experiment'] != 'issue26-co-located-v2' or manifest['samples_required'] != 3 \
                or manifest['timeout_seconds'] != 1800 or manifest['full_suite'] is not True \
                or 'error' in manifest or set(manifest['sources']) != set(SOURCES):
            raise ValueError('incomplete or mismatched co-located group')
        if manifest['source_order'] != {str(i): ['before', 'after-32'] if i % 2 else ['after-32', 'before']
                                        for i in (1, 2, 3)}:
            raise ValueError('source order differs from the planned counterbalance')
        flags = ['-p', '2', '-json', '-timeout', '30m'] + (['-race'] if mode == 'race' else [])
        protocol = {'cold': 'full_cold_warm', 'warm': 'compile_only_warm',
                    'result': 'direct_full_result_seed'}[state]
        rows, environments = {}, []
        for label, original in SOURCES.items():
            source = manifest['sources'][label]
            if source['original_revision'] != original or source['fixture_fix'] != FIX \
                    or source['measured_revision'] != VARIANTS[label] or source['patch_sha256'] != PATCH_SHA256 \
                    or set(source['samples']) != {'1', '2', '3'}:
                raise ValueError('source identity or sample inventory does not match')
            samples = []
            for sample in (1, 2, 3):
                status = source['samples'][str(sample)]
                if status['exit_code'] != 0 or status['native_grep']['passed'] is not True:
                    raise ValueError('failed sample or missing native grep execution')
                base = prefix + label + f'/sample-{sample}/'
                env = json.loads(archive.read(base + 'environment.json'))
                if env['commit'] != VARIANTS[label] or env['dirty'] is not False \
                        or env['sample_index'] != sample or env['mode'] != mode \
                        or env['repetitions_required'] != 3 or env['seed_protocol'] != protocol \
                        or env['flags'] != flags or env['packages'] != PACKAGES \
                        or str(env['gomaxprocs_configured']) != '2' or env['gomaxprocs_effective'] != 2 \
                        or env['go']['GOVERSION'] != 'go1.27.2' or env['go']['GOTOOLCHAIN'] != 'local' \
                        or env['go']['GOFLAGS'] != '-mod=readonly' \
                        or env['go']['CGO_ENABLED'] != ('0' if mode == 'cgo-free' else '1'):
                    raise ValueError('environment or flags differ from the group protocol')
                environments.append(env)
                commands = []
                if state == 'warm':
                    commands += [(f'build-seed-tests-{sample}', ['go', 'test', *flags, '-count=1', '-run', '^$', *PACKAGES], False),
                                 (f'build-seed-cli-{sample}', ['go', 'build', '-p', '2', '-o', '/dev/null', './cmd/hufu'], False)]
                elif state == 'result':
                    commands.append((f'result-seed-{sample}', ['go', 'test', *flags, *PACKAGES], True))
                target = ['go', 'test', *flags] + (['-count=1'] if state != 'result' else []) + PACKAGES
                commands.append((f'{state}-{sample}', target, True))
                evidence = {}
                for name, command, full in commands:
                    observation = json.loads(archive.read(base + name + '.json'))
                    if observation['exit_code'] != 0 or observation['name'] != name or observation['command'] != command:
                        raise ValueError('failed or substituted group command')
                    if base + name + '.stderr' not in names or base + name + '.jsonl' not in names:
                        raise ValueError('raw evidence is incomplete')
                    if full:
                        with archive.open(base + name + '.jsonl') as stream:
                            cases = inventory(stream)
                        if not cases['top_level'] or cases['failed_packages'] or cases['failed_tests'] \
                                or not cases['native_grep_passed']:
                            raise ValueError('raw outcomes contradict successful sample')
                    else:
                        cases = None
                    evidence[name] = {'observation': observation, 'inventory': cases}
                samples.append({'sample': sample, 'environment': env, 'evidence': evidence,
                                'measured': evidence[f'{state}-{sample}']})
            if len({row['measured']['inventory']['normalized_case_outcome_sha256'] for row in samples}) != 1:
                raise ValueError('full case/outcome inventory differs between co-located samples')
            rows[label] = {'identity': source, 'samples': samples,
                           'statistics': {key: stats([row['measured']['observation'][key] for row in samples])
                                          for key in ('wall_seconds', 'cpu_user_seconds', 'cpu_system_seconds')}}
        for key in ('cpu', 'os', 'machine', 'hostname', 'logical_cpus', 'cpu_quota'):
            if len({json.dumps(env[key]) for env in environments}) != 1:
                raise ValueError('samples are not co-located in the same declared runner environment')
        return {'archive': path.name, 'sha256': digest_file(path), 'manifest': manifest, 'sources': rows}


def aggregate(groups):
    expected = {(mode, state) for mode in ('normal', 'race', 'cgo-free') for state in ('cold', 'warm', 'result')}
    actual = [(row['manifest']['mode'], row['manifest']['cache_state']) for row in groups]
    if len(actual) != len(set(actual)) or set(actual) != expected:
        raise ValueError('all nine distinct mode/cache groups are required')
    for key in ('controller_revision', 'harness_sha256', 'run_id', 'run_attempt'):
        if len({row['manifest'][key] for row in groups}) != 1:
            raise ValueError('groups come from different experiments')
    for label in SOURCES:
        for mode in ('normal', 'race', 'cgo-free'):
            signatures = {sample['measured']['inventory']['normalized_case_outcome_sha256']
                          for group in groups if group['manifest']['mode'] == mode
                          for sample in group['sources'][label]['samples']}
            if len(signatures) != 1:
                raise ValueError('case/outcome coverage changed between cache groups')
    return {'capture_matrix_passed': True, 'acceptance_complete': False,
            'remaining_semantic_gates': ['assertion preservation review', 'CI/cache/flake interpretation'],
            'groups': groups}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('archives', type=Path, nargs='+')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    args.output.write_text(json.dumps(aggregate([inspect_group(path) for path in args.archives]),
                                     indent=2, sort_keys=True) + '\n')
    print('All co-located groups validated; semantic acceptance requires review.')


if __name__ == '__main__':
    main()
