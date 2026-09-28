# End-to-end performance gate

Use `hufu eval performance-gate` to compare repeated Hufu invocations with a
single-agent baseline while keeping correctness as a blocking condition:

```bash
hufu eval performance-gate baseline.json candidate.json > gate-report.json
```

The command exits non-zero when the candidate regresses. Both inputs are JSON
arrays of end-to-end observations. Durations are exact nanoseconds:

```json
[
  {
    "task_class": "simple-query",
    "duration_ns": 125000000,
    "accepted": true,
    "total_tokens": 640,
    "provider_calls": 1,
    "retries": 0
  }
]
```

Capture each observation around the complete process invocation, not an
internal function. `accepted` must mean that the configured acceptance contract
passed; a plausible answer without acceptance evidence is false. Tokens should
be the provider-reported total, calls should include every model request, and
retries should count repeated attempts after the initial attempt.

Every input must cover these five task classes:

- `simple-query`
- `localized-change`
- `verification`
- `multi-role-workflow`
- `failure-recovery`

Provide at least five observations per class in each file so p95 is not a
single-run anecdote.

The gate calculates accepted-task p50/p95 latency and p95 tokens, provider
calls, and retries per class. By default candidate acceptance may not drop,
tokens/calls/retries may not increase, and p95 latency may be at most 1.25 times
the baseline to tolerate ordinary host variance. Failed samples affect the
acceptance rate but are excluded from latency/resource percentiles, so faster
failures cannot make a candidate look better.
