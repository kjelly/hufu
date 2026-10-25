#!/usr/bin/env python3
"""Report observed workflow wall time and runner time across all run attempts."""

import datetime
import json
import os
import sys
import time
from pathlib import Path
import urllib.error
import urllib.request


def timestamp(value):
    return datetime.datetime.fromisoformat(value.replace('Z', '+00:00'))


def summarize(run, jobs, now):
    # Jobs returned for rerun attempts can overlap: count a job ID only once.
    unique = {job['id']: job for job in jobs}
    runner_seconds = 0.0
    running = 0
    for job in unique.values():
        if not job.get('started_at'):
            continue
        if job.get('completed_at'):
            end = timestamp(job['completed_at'])
        elif job['status'] == 'in_progress':
            end = now
            running += 1
        else:
            continue
        runner_seconds += max(0, (end - timestamp(job['started_at'])).total_seconds())
    return {'workflow_wall_seconds_so_far': (now - timestamp(run['created_at'])).total_seconds(),
            'runner_seconds_so_far': runner_seconds, 'job_records': len(unique),
            'in_progress_jobs': running, 'attempts': run['run_attempt']}


def collect(get, run_path):
    run = get(run_path)
    jobs = []
    if type(run['run_attempt']) is not int or run['run_attempt'] < 1:
        raise ValueError('invalid run_attempt')
    for attempt in range(1, run['run_attempt'] + 1):
        page = 1
        while True:
            batch = get(f'{run_path}/attempts/{attempt}/jobs?per_page=100&page={page}')['jobs']
            if not isinstance(batch, list):
                raise ValueError('invalid jobs')
            jobs.extend(batch)
            if len(batch) < 100:
                break
            page += 1
    return run, jobs


def get_json(base, token, path):
    request = urllib.request.Request(base + path, headers={
        'Authorization': 'Bearer ' + token,
        'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28',
    })
    # One retry for transient transport/5xx failures; never wait out rate limits.
    for attempt in range(2):
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if attempt or not 500 <= error.code < 600:
                raise
        except (urllib.error.URLError, TimeoutError):
            if attempt:
                raise
        time.sleep(1)


def main():
    try:
        base = os.environ['GITHUB_API_URL']
        token = os.environ['GH_TOKEN']
        run_path = f"/repos/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
        run, jobs = collect(lambda path: get_json(base, token, path), run_path)
        result = summarize(run, jobs, datetime.datetime.now(datetime.timezone.utc))
    except (OSError, ValueError, KeyError, TypeError, AttributeError, OverflowError) as error:
        # Do not include request URLs, response bodies or credentials in diagnostics.
        if isinstance(error, urllib.error.HTTPError):
            reason = f'HTTP {error.code}'
        elif isinstance(error, (urllib.error.URLError, TimeoutError)):
            reason = 'network error or timeout'
        else:
            reason = f'invalid or missing cost data ({type(error).__name__})'
        message = f'ci_cost=unavailable ({reason})'
        print(message, file=sys.stderr)
        with Path(os.environ['GITHUB_STEP_SUMMARY']).open('a') as stream:
            stream.write(f'### Observed CI cost (all attempts)\n\n- {message}\n')
        return 1
    print(json.dumps(result, indent=2))
    with Path(os.environ['GITHUB_STEP_SUMMARY']).open('a') as stream:
        stream.write('### Observed CI cost (all attempts)\n\n')
        stream.write(f"- Workflow wall time so far: {result['workflow_wall_seconds_so_far']:.3f}s (includes queue/rerun gaps).\n")
        stream.write(f"- Total runner time so far: {result['runner_seconds_so_far']:.3f}s across {result['job_records']} jobs.\n")
        stream.write(f"- Attempts: {result['attempts']}; jobs still running: {result['in_progress_jobs']}.\n")
        stream.write('- These are observations at the aggregate gate; its remaining execution/cleanup is excluded.\n')
        stream.write('- Per-layer command timings, inventories and cache KiB are in the test artifacts; setup-go logs contain cache transfer diagnostics.\n')


if __name__ == '__main__':
    sys.exit(main())
