#!/usr/bin/env python3
"""Report observed workflow wall time and runner time across all run attempts."""

import datetime
import json
import os
from pathlib import Path
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
    for attempt in range(1, run['run_attempt'] + 1):
        page = 1
        while True:
            batch = get(f'{run_path}/attempts/{attempt}/jobs?per_page=100&page={page}')['jobs']
            jobs.extend(batch)
            if len(batch) < 100:
                break
            page += 1
    return run, jobs


def main():
    base = os.environ['GITHUB_API_URL']
    token = os.environ['GH_TOKEN']

    def get(path):
        request = urllib.request.Request(base + path, headers={
            'Authorization': 'Bearer ' + token,
            'Accept': 'application/vnd.github+json',
            'X-GitHub-Api-Version': '2022-11-28',
        })
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    run_path = f"/repos/{os.environ['GITHUB_REPOSITORY']}/actions/runs/{os.environ['GITHUB_RUN_ID']}"
    run, jobs = collect(get, run_path)
    result = summarize(run, jobs, datetime.datetime.now(datetime.timezone.utc))
    print(json.dumps(result, indent=2))
    with Path(os.environ['GITHUB_STEP_SUMMARY']).open('a') as stream:
        stream.write('### Observed CI cost (all attempts)\n\n')
        stream.write(f"- Workflow wall time so far: {result['workflow_wall_seconds_so_far']:.3f}s (includes queue/rerun gaps).\n")
        stream.write(f"- Total runner time so far: {result['runner_seconds_so_far']:.3f}s across {result['job_records']} jobs.\n")
        stream.write(f"- Attempts: {result['attempts']}; jobs still running: {result['in_progress_jobs']}.\n")
        stream.write('- These are observations at the aggregate gate; its remaining execution/cleanup is excluded.\n')
        stream.write('- Per-layer command timings, inventories and cache KiB are in the test artifacts; setup-go logs contain cache transfer diagnostics.\n')


if __name__ == '__main__':
    main()
