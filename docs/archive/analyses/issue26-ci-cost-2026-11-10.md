# Issue 26 correctness CI cost and coverage observations

> Status: historical
> Authority: analysis
> Verified-Commit: `fbf8faaa55d0ea66bdda446a17ebf0a0eedc6b22` (Go tree unchanged across the three completed observations)
> Supersedes: —
> Superseded-By: —

These are observed correctness-CI costs, separate from the historical, paired-pilot and co-located performance experiments. They do not establish a before/after CI speedup.

## Completed correctness runs

| Run / head | Attempts / successful jobs | Total runner seconds | Earliest job start to final completion |
| --- | ---: | ---: | ---: |
| [CI 42735101109](https://github.com/kjelly/hufu/actions/runs/42735101109) / `cd836428` | 1 / 15 | 6046 | 1012 |
| [CI 42743592641](https://github.com/kjelly/hufu/actions/runs/42743592641) / `9758ec40` | 1 / 15 | 6274 | 1278 |
| [CI 42750634547](https://github.com/kjelly/hufu/actions/runs/42750634547) / `fbf8faaa` | 1 / 15 | 5849 | 1485 |

Every completed attempt and job above is included. The span excludes the initial queue before the first job; runner seconds include each job’s setup, tests and cleanup. It is not child CPU time. The Go source, Go tests, module files and correctness workflow are unchanged between these heads; Python tooling counts are 20, 27 and 38 respectively. Builds also carry distinct VCS identities. Hardware/cache/queue variation and that tooling difference prevent a causal CI-improvement claim.

## Timed commands and cache allocation (CI 42735101109)

| Partition | Command wall seconds | User / system seconds | Cache KiB before → after |
| --- | ---: | ---: | ---: |
| integration | 967.02 | 1377.03 / 50.32 | 1915360 → 2480148 |
| short-cli | 237.14 | 265.96 / 17.91 | 1707036 → 2066168 |
| short-improver | 759.50 | 783.73 / 27.67 | 846248 → 974056 |
| short-other | 281.65 | 454.51 / 38.73 | 1028916 → 1295080 |
| short-team-0 | 775.37 | 702.93 / 35.25 | 933904 → 1141396 |
| short-team-1 | 587.04 | 539.44 / 28.68 | 933904 → 1141396 |
| short-team-2 | 475.16 | 439.94 / 20.09 | 933904 → 1141396 |

The seven timed commands report 4782.19 user+system CPU seconds. They exclude other CI gates and setup, so this is neither billed runner CPU nor total utilization. Allocated cache growth totals 1940368 KiB across seven separate runner caches; summing those allocations does not describe one shared cache or a hit ratio. Parent/subtest elapsed and package elapsed are not summed into wall time.

## Actual cache archive transfer

The setup-go logs for these seven jobs report primary-key hits. Their archive sizes and observed restore spans are below. The span runs from the cache-hit log to “Cache restored successfully”; it includes transfer, unpack and action overhead, not pure network time. Archive content can include module and build caches; sizes are not a measurement of Go compilation alone.

| Job | Downloaded archive bytes | Observed restore span, seconds |
| --- | ---: | ---: |
| [job 129562294781](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562294781) | 473257993 | 6.380 |
| [job 129562294746](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562294746) | 521467114 | 13.488 |
| [job 129562294792](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562294792) | 320430758 | 5.339 |
| [job 129562294827](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562294827) | 300442627 | 5.683 |
| [job 129562294927](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562294927) | 320418702 | 4.323 |
| [job 129562295030](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562295030) | 337813514 | 5.056 |
| [job 129562295069](https://github.com/kjelly/hufu/actions/runs/42735101109/job/129562295069) | 320403062 | 4.338 |

Total reported archive payload: 2594233770 bytes across seven runners. Every selected log explicitly says the primary key was hit and the cache was not saved, so the observed post-job cache upload payload for these seven jobs is zero. This does not assert zero network/setup cost for the entire workflow.

The performance workflow disables Actions cache restore/save and removes each intentional private build cache after its observation. Module downloads are outside timed test commands and remain in job cost. There is no performance-build-cache upload/restore operation to compare with the correctness archives. No transfer savings are inferred from disk KiB.

## Coverage and current native grep regression

Independent parsing of all seven raw `tests.jsonl` streams against their complete inventory found:

- Short: 8,303 inventory Tests, 8,276 pass and 27 skip, with zero missing or duplicated inventory identities.
- Integration: 57 inventory Tests, all pass, with zero missing or duplicated identities.
- Short and integration inventory sets are disjoint. Example/fuzz and parent/subtest events remain raw evidence and are not substituted for top-level Test counts.
- Skip is not coverage. Native grep prerequisites are explicitly exercised by the separate performance captures; opt-in live/interactive boundaries remain limitations.

On the current Go tree at `fbf8faaa`, `TMPDIR=<private task directory> GOMAXPROCS=2 mise exec -- go test -p 2 -race -count=1 -timeout 10m ./internal/tools -run '^TestGrep' -json` exited zero: 21 top-level tests, 40 terminal test events including subtests, zero failures/skips. Package elapsed was 1.277 seconds; this is correctness evidence, not a performance sample. Raw JSONL SHA-256: `575ba5b3856d9ea3b6afef645ee92203c1b99f4244a3691197afd52fae371bd5`.

## Raw correctness artifact provenance

All seven ZIP digests were checked against the Actions API before parsing.

| Artifact | ID | SHA-256 |
| --- | --- | --- |
| integration | 13726639088 | `09d3ce7d90b933e305eb2e83a02e282a6ea9bd2f2ab57f3f066ed43eb8f47870` |
| short-cli | 13727120696 | `330f6742d798ed96fa1eacd59153c17021189f446f560541fd26aceef0773dc7` |
| short-improver | 13727696018 | `8f19dceb348316fa54a64750a2ce90b35d0d0c0d3f40a2f63d93727bf928a230` |
| short-other | 13726114793 | `cedc7bcf06046d9db3dc5789c24f97cc632f145b72f4e72193248aeeb1605a0e` |
| short-team-0 | 13727416234 | `43cbf7c93c49f66bc82fe7c7afaf81e3597279eb385033181cbd79277bc27c65` |
| short-team-1 | 13727920433 | `d84819e859cbf317a6a099b9fbd0ec2e4ee22aa2ea1b89ed3748af2959c40b06` |
| short-team-2 | 13726951962 | `6df544f4cafac368d9320ce269170270b59fc72a83e0aadeabfc0eadba6fd88f` |

## Interpretation and current CI limits

Three passing correctness runs are bounded observations, not proof of a zero flake probability. The [completed same-runner acceptance report](issue26-performance-acceptance-2026-11-10.md) records all nine groups, every attempt cost and raw outcome/case consistency. Final published-head CI and review/rules checks remain PR readiness gates; these three selected runs are the completed observations available before report publication. The current three team shards already exist in main; integration is the longest timed command in the first run. These data do not justify another package split or additional sharding. The older October decision remains historical rather than a restriction on subsequent measured CI repairs.
