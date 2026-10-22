# Test baseline capture attempt — 2026-10-22

This is historical execution evidence for [issue #26](https://github.com/kjelly/hufu/issues/26),
not an accepted performance baseline or a speedup claim. The maintained capture
procedure and contract inventory are in the
[test baseline reference](../../reference/test-performance-baseline.md).

## Source and environment

- Runtime/test source: `8f65a57cca7470aa773a28f7c70bfaa5c6b4f550`, the issue's
  original reference. Only measurement tooling, documentation and ignore rules
  were added locally; no Go source or assertion was changed for this attempt.
- Go 1.27.2, Linux amd64, kernel 6.18.55, glibc 2.41.
- AMD EPYC 9V45; 5 visible logical CPUs, cgroup CPU quota 400000/100000.
- Configured and effective GOMAXPROCS: 5. CGO_ENABLED=1,
  GOFLAGS=`-mod=readonly`, GOTOOLCHAIN=`local`, GOTMPDIR unset.
- Managed container, not a GitHub Actions runner. Local sockets were allowed
  for the measured invocation. Module cache was populated; the invocation's
  private Go build cache began empty. This is not a cold OS/page-cache run.

## Full-suite attempt

```bash
python3 scripts/test-baseline.py --output work/baseline-normal --cache-state all --profile
# First measured child command:
go test -json -timeout 20m -short -count=1 ./... ./.agent-teams/hufu-dev/diagnostic-pilot/...
```

The first cold-build observation exited **1** after **274.487 seconds**.
Package terminal events: 62 pass, 1 skip, 2 fail. Test terminal events:
14,391 pass, 23 skip, 8 fail; these include overlapping parents/subtests and
must not be interpreted as independent top-level test counts.

Failures on that source revision:

| Package | Test / failed contract |
| --- | --- |
| `internal/workspace/versionstore` | `TestMaterializeRecoversFromPartialWrite`: materialization rejects the existing unmanaged path `a` |
| `internal/team` | `TestProcessSupervisorKillsDescendants`: grandchild remains alive after KillTree |
| `internal/team` | `TestTerminalSession_ProcessGroupKillDescendants`: descendant survives parent termination |
| `internal/team` | `TestTerminalSession_ProcessGroupKillDescendants_NetworkBlock`: descendant survives termination |
| `internal/team` | `TestTerminalSession_ChildTimeout_ProcessGroupKillDescendants_NetworkBlock`: descendant survives ChildTimeout |
| `internal/team` | `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease`: process group remains alive after Close |
| `internal/team` | `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease`: descendant survives finalization |
| `internal/team` | `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap`: descendant survives containment |

The process/PTY observations may depend on the container's process-reaping
behavior; their root cause has not been established. They remain failures,
not skips or evidence of a passing lifecycle contract. An earlier sandboxed
probe also failed because local TCP sockets were forbidden; those times are
not used here.

The harness stopped after this failure. There are **no** three-sample medians,
warm/result-cache groups, race baseline, full CGO-free baseline or team profiles
from this full-suite attempt. It is invalid to infer an acceleration percentage,
flake rate or runner-cost improvement from this observation. No timeout,
assertion or regression case was weakened to obtain a passing result.

## Harness verification on the PR base
The capture tool was also exercised against unchanged `internal/yamlutil`
on `13c126ae`, separately for normal, race and CGO-free (the last without
`-short`). This is a **tool integration check**, not full-suite performance
evidence. Each mode completed three cold, three warm/fresh and three result-cache
observations, plus three untimed-in-summary result seeds (36 commands total).
All commands passed. Result observations reported cache hits; fresh observations
did not. Each mode retained the same observed case-set hash across repetitions.

Wall time in seconds, median (min–max):

| Mode | Cold build | Warm build / fresh tests | Result cache |
| --- | --- | --- | --- |
| normal | 7.439 (7.333–7.952) | 0.185 (0.151–0.225) | 0.083 (0.076–0.092) |
| race | 8.431 (8.334–8.447) | 1.220 (1.215–1.226) | 0.073 (0.072–0.078) |
| cgo-free | 7.776 (7.543–7.802) | 0.156 (0.155–0.165) | 0.078 (0.073–0.107) |

The three Python evidence regression checks, `golangci-lint run` (zero issues),
workflow YAML parsing, `git diff --check`, and `bin/check-docs` also passed on
the newer PR base. These checks do not turn the original full-suite failure
into a passing baseline.

## Follow-up

The tooling PR is based on the newer main revision
`13c126aec5022472fd29ce9c01613f49023c779d`; the historical failure table above
only describes `8f65a57c`, not the newer revision. Collect three successful
observations per configuration on the chosen comparison revisions and retain
raw artifacts before beginning profile-driven optimization. Keep issue #26
open to track that evidence and #27–#32.
