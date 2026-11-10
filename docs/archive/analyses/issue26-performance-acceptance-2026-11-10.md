# Issue 26 performance acceptance — 2026-11-10

> Status: historical
> Authority: analysis
> Verified-Commit: `fbf8faaa55d0ea66bdda446a17ebf0a0eedc6b22` (measurement controller)
> Supersedes: —
> Superseded-By: —

**The #26 measurement and contract acceptance is complete for the explicitly remediated comparison below.** All nine cohorts passed on their first attempt; every source/mode/cache group has three co-located observations. This completes the evidence requirement, without certifying an overall or causal speedup. Final PR readiness additionally requires successful checks on its published head.

## Scope and source identity

Original endpoints are `8f65a57cca7470aa773a28f7c70bfaa5c6b4f550` and `b3360c9a5804e83934204d8118c5c322e50a94e9`. Each receives only the exact already-merged fixture-drain patch from `300041b84e45e7c53549bd53c753b6b0ac6ad860` (#46). The measured clean variants are `f1faec1b407ba8205b0fdf5bff80d9f254820401` and `c6c0d0b0017b860a80844eb18bf44c80cba7893e`. These are not successful runs of the unchanged original revisions.

Variant trees: before `b21fa215248204c5890e2ec239163e71b33d5199`, after-32 `ab17445e97bc7d37b90f31329bb0fde64ce6a6e3`. Common fixture patch SHA-256: `809adc693dacf1bc6c880ec600dbb2167872b8f776bf0ee54d1f25358c25ff55`. Frozen harness SHA-256: `486fa39f8193da0ac5ce45b4ca5ead129ed1f12433f782e7afa67753422c3093`; all nine cohorts record ripgrep 14.1.0.

The whole revisions differ in production and 29 removed / 183 added top-level identities. [The complete reconciliation](issue26-assertion-reconciliation-2026-11-10.md) distinguishes 15 performance-related removals from 14 other protocol/test changes; stronger or superseded assertions are not falsely labelled token-equivalent. The six focused comparisons retain their original controlled boundaries in the [October report](issue26-performance-acceptance-2026-10-30.md).

## Protocol and evidence boundaries

Primary run: [42750628136](https://github.com/kjelly/hufu/actions/runs/42750628136). The [protocol](../../reference/issue26-performance-acceptance.md) defines nine mode/cache cohorts, three observations of both sources on one runner per cohort, alternating source order. Every sample has a private build cache. Cold is empty-cache fresh execution; warm has compile-only test/CLI seeds followed by full `-count=1`; result replay follows a separately preserved full seed. Runtime-generated helper builds remain measured. Seeds are not counted as measured samples.

Full package universe: `./...` plus `./.agent-teams/hufu-dev/diagnostic-pilot/...`; no `-short`, no scope reduction. Go 1.27.2, GOTOOLCHAIN=local, GOMAXPROCS=2 and measured package parallelism two; all sources/modes share an explicit 30-minute package deadline. The old 20-minute failures remain unmodified. Native grep installation and actual execution of all six required contracts are independently checked.

Within each cohort, all six source/sample environments must have the same CPU, hostname, OS, machine, logical CPUs and recorded quota. A missing quota remains unknown. Different cache cohorts can use different CPUs: do not attribute cross-cache or cross-mode differences solely to cache/CGO. Go source/command hashes, complete raw outcomes and native contract execution are verified, not inferred from workflow prose.

## Observed command wall times

Seconds: median (minimum–maximum), exactly three successful observations per populated cell. All nine groups are complete; seeds are excluded from these measured medians.

| Mode | Cache | CPU | Before remediated | After-32 remediated |
| --- | --- | --- | ---: | ---: |
| normal | cold | Intel(R) Xeon(R) 6973P-C | 437.790 (435.733–439.736) | 416.891 (416.265–417.870) |
| normal | warm | AMD EPYC 9V74 80-Core Processor | 310.812 (310.137–313.661) | 274.710 (271.306–293.650) |
| normal | result | AMD EPYC 9V45 96-Core Processor | 72.974 (72.485–73.585) | 31.964 (30.550–35.460) |
| race | cold | Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz | 1976.179 (1969.796–1977.644) | 2074.642 (2074.178–2078.479) |
| race | warm | AMD EPYC 9V74 80-Core Processor | 1485.760 (1451.734–1488.542) | 1550.117 (1545.526–1551.943) |
| race | result | AMD EPYC 9V74 80-Core Processor | 317.947 (311.888–320.551) | 273.952 (263.482–276.678) |
| cgo-free | cold | AMD EPYC 9V74 80-Core Processor | 417.480 (405.439–441.705) | 396.170 (385.739–432.967) |
| cgo-free | warm | INTEL(R) XEON(R) PLATINUM 8573C | 333.429 (330.867–335.242) | 304.684 (303.046–305.073) |
| cgo-free | result | AMD EPYC 7763 64-Core Processor | 99.625 (99.148–100.913) | 48.388 (47.839–48.808) |

## Observed user/system CPU

Seconds: independent median (minimum–maximum) of native child user and system CPU. These are not total runner utilization or billing.

| Mode | Cache | Before user / system | After-32 user / system |
| --- | --- | ---: | ---: |
| normal | cold | 729.375 (726.131–737.593) / 84.883 (83.495–85.918) | 696.054 (693.675–702.079) / 83.058 (82.528–83.515) |
| normal | warm | 344.751 (343.045–355.533) / 66.442 (66.410–66.933) | 306.763 (305.824–310.595) / 63.555 (62.483–63.892) |
| normal | result | 66.212 (64.502–66.448) / 20.754 (20.518–20.920) | 29.562 (27.957–33.561) / 15.440 (15.185–16.584) |
| race | cold | 3726.243 (3715.683–3732.192) / 180.313 (178.240–180.322) | 3781.392 (3779.605–3783.554) / 180.650 (179.695–182.664) |
| race | warm | 2425.765 (2422.258–2431.114) / 132.589 (131.222–132.669) | 2458.953 (2458.514–2460.427) / 131.594 (130.822–133.871) |
| race | result | 289.135 (286.574–289.856) / 32.173 (31.864–32.519) | 244.761 (243.930–245.061) / 25.894 (25.687–25.974) |
| cgo-free | cold | 655.783 (651.542–664.161) / 103.905 (101.705–104.154) | 624.637 (622.137–625.094) / 100.143 (98.709–100.285) |
| cgo-free | warm | 412.892 (410.773–420.815) / 60.307 (59.823–61.649) | 385.407 (384.504–389.154) / 58.134 (58.008–59.237) |
| cgo-free | result | 97.685 (97.035–98.749) / 29.046 (28.928–29.385) | 48.712 (47.870–48.712) / 22.104 (22.086–22.219) |

## Case/outcome and native execution audit

Every source/sample record is bound to its measured SHA, original parent, exact fixture patch, flags and raw case-name hash/counts. Complete nested case/outcome signatures must match across all three observations and across cache groups. Raw names remain preserved. Normalization is limited to the three previously audited fixture families: two loopback ports, one object hash and one temporary-directory suffix; collisions are rejected. Byte coverage is not proof of semantic equivalence.

| Mode / source | Verified cohorts | Top-level cases | Top-level skips | All terminal identities |
| --- | ---: | ---: | ---: | ---: |
| normal / before | 3 | 7436 | 23 | 14422 |
| normal / after-32 | 3 | 7590 | 23 | 14901 |
| race / before | 3 | 7436 | 23 | 14422 |
| race / after-32 | 3 | 7590 | 23 | 14901 |
| cgo-free / before | 3 | 7436 | 23 | 14422 |
| cgo-free / after-32 | 3 | 7590 | 23 | 14901 |

Case numbers include parent/subtest events as specified; they are not added to package time. Opt-in/live/interactive skips are explicit limitations, not passing contracts. The [current correctness/coverage and cache-cost audit](issue26-ci-cost-2026-11-10.md) additionally records full short/integration membership and current-tree grep race execution.

## Preserved earlier experiments and costs

Original run [41046691306](https://github.com/kjelly/hufu/actions/runs/41046691306) failed with a real fixture race and package deadlines. Its samples, artifacts and failure diagnostics are not overwritten. The [October report](issue26-performance-acceptance-2026-10-30.md) records 35,345 runner seconds for that experiment. The original normal ZIP digest was reverified for the removed-case audit; all three original artifact metadata digests were rechecked.

Paired pilot [42743586308](https://github.com/kjelly/hufu/actions/runs/42743586308) keeps before/after on one runner per sample, but the three CGO-free samples used three CPU models. It is descriptive paired evidence, not three same-environment repetitions. All nine pilot jobs and 72 commands completed successfully without retries; every source/mode has stable normalized outcomes across its 12 raw commands. Its complete cost is included below.

Primary and pilot terminal costs and raw artifact provenance are recorded below. No failed observation is replaced by a retry or omitted from cost.

## Readiness gate

The numerical matrix, removed-case review and CI/cache/flake interpretation are complete. The automated aggregate deliberately retains `acceptance_complete: false`: it validates the matrix and leaves semantic judgment to this report. The manual conclusion is bounded by the stated workload differences and skipped opt-in/platform contracts. PR #53 must still pass final published-head CI and review/rules checks before ready; merge remains a separate action.

The race/cold and race/warm cohorts are slower at the after-32 endpoint (2074.642 versus 1976.179 seconds cold; 1550.117 versus 1485.760 seconds warm median), while several normal/CGO-free observations are faster. Production and case inventories differ, so neither direction is attributed solely to the six refactors. No speedup threshold was specified by #26 and no universal acceleration claim is made.

Retain the existing three team shards and do not add another package split or shard from these data. In the timed correctness observation, integration is longer than every individual short shard; extra team shards alone would not remove that bottleneck and can duplicate compilation, fixture setup and cache transfer. Revisit only with current critical-path/profile evidence.

## Package and slow-test observations

Seconds: median (minimum–maximum) of exactly three observations in the named cohort. The three largest package medians and three largest passing top-level Test medians are listed per source. Test medians are calculated from complete raw JSONL streams, including tests outside each sample’s compact slowest-test list. Parent/subtest and package elapsed overlap; none are summed into command wall time. Result replay can replay historical package/test elapsed, so those values describe cached outcomes and must not be interpreted as live execution cost.

| Mode / cache / source | Three largest package elapsed medians (ranges) | Three largest top-level Test elapsed medians (ranges) |
| --- | --- | --- |
| cgo-free / cold / before | `internal/team` 161.219 (151.679–174.725); `cmd/hufu` 95.303 (93.770–104.157); `internal/improver` 94.880 (82.445–97.744) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 28.530 (27.900–29.520); `TestRunAllEvalSuites` (internal/evalharness) 18.160 (13.270–20.080); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 15.890 (12.620–20.790) |
| cgo-free / cold / after-32 | `internal/team` 168.640 (158.662–197.843); `internal/improver` 85.755 (83.176–89.511); `internal/tools` 46.793 (46.721–46.863) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 28.470 (26.990–29.150); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.030–15.030); `TestRunAllEvalSuites` (internal/evalharness) 14.920 (14.500–15.250) |
| cgo-free / result / before | `cmd/hufu` 95.246 (94.722–96.458); `internal/team` 0.146 (0.138–0.178); `internal/tools` 0.016 (0.016–0.017) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 37.290 (36.950–37.400); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 17.480 (17.160–17.710); `TestRefinementResumeImportsCompletedProposalBeforeBinding` (internal/improver) 16.790 (16.630–17.780) |
| cgo-free / result / after-32 | `cmd/hufu` 43.948 (43.489–44.300); `internal/team` 0.159 (0.137–0.182); `internal/tools` 0.022 (0.017–0.022) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 34.590 (34.350–35.180); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 19.150 (18.230–19.510); `TestRunAllEvalSuites` (internal/evalharness) 18.390 (18.260–18.520) |
| cgo-free / warm / before | `internal/team` 189.088 (185.115–195.389); `cmd/hufu` 105.543 (103.836–111.503); `internal/improver` 100.439 (100.321–101.662) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 36.620 (36.250–37.100); `TestRunAllEvalSuites` (internal/evalharness) 17.520 (17.030–21.100); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 17.250 (16.950–17.640) |
| cgo-free / warm / after-32 | `internal/team` 200.571 (199.873–201.554); `internal/improver` 101.474 (100.405–103.747); `cmd/hufu` 48.714 (48.459–49.295) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 33.570 (33.430–33.580); `TestRunAllEvalSuites` (internal/evalharness) 19.120 (18.670–19.380); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 17.330 (17.050–17.360) |
| normal / cold / before | `internal/team` 162.286 (162.049–163.755); `cmd/hufu` 104.003 (103.493–106.776); `internal/improver` 84.790 (84.626–85.399) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 30.540 (30.170–30.660); `TestRunAllEvalSuites` (internal/evalharness) 15.050 (15.000–15.210); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.020–15.030) |
| normal / cold / after-32 | `internal/team` 172.783 (172.540–173.162); `internal/improver` 88.184 (87.227–89.748); `cmd/hufu` 48.275 (47.817–49.243) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 29.120 (29.110–29.230); `TestRunAllEvalSuites` (internal/evalharness) 16.640 (16.380–16.680); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.020–15.030) |
| normal / result / before | `cmd/hufu` 69.362 (69.307–70.228); `internal/team` 0.091 (0.090–0.093); `internal/tools` 0.011 (0.011–0.011) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 21.620 (21.540–21.680); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.020–15.030); `TestSSH_ConnectionReuse` (internal/tools) 14.030 (14.020–14.060) |
| normal / result / after-32 | `cmd/hufu` 28.604 (27.378–32.099); `internal/team` 0.090 (0.089–0.091); `internal/tools` 0.010 (0.010–0.011) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 20.380 (20.050–20.970); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.020–15.030); `TestSSH_ConnectionReuse` (internal/tools) 14.020 (14.020–14.030) |
| normal / warm / before | `internal/team` 172.791 (170.407–178.666); `cmd/hufu` 99.977 (97.509–102.652); `internal/improver` 86.417 (85.743–90.692) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 30.790 (30.030–33.360); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.020 (15.020–15.030); `TestRunAllEvalSuites` (internal/evalharness) 14.230 (13.390–14.570) |
| normal / warm / after-32 | `internal/team` 180.676 (174.861–193.977); `internal/improver` 84.080 (82.772–98.286); `internal/tools` 46.911 (46.900–47.300) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 27.150 (25.980–27.930); `TestRunAllEvalSuites` (internal/evalharness) 16.730 (15.420–17.160); `TestStdioProcessCancellationDeadlineAndDrain` (internal/mcpserver) 15.030 (15.020–15.030) |
| race / cold / before | `internal/team` 1106.027 (1105.792–1109.720); `internal/improver` 975.334 (974.444–976.453); `cmd/hufu` 475.751 (471.558–477.562) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 355.840 (355.350–359.240); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 162.060 (161.700–162.090); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 161.860 (161.630–162.180) |
| race / cold / after-32 | `internal/team` 1209.097 (1208.984–1210.286); `internal/improver` 971.643 (971.079–975.064); `cmd/hufu` 418.350 (416.178–418.705) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 353.890 (353.820–356.330); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 161.130 (160.920–161.890); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 160.820 (160.650–161.520) |
| race / result / before | `cmd/hufu` 313.196 (307.162–315.796); `internal/team` 0.102 (0.100–0.113); `internal/tools` 0.012 (0.012–0.012) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 309.310 (307.900–311.870); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 136.550 (134.830–137.880); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 135.610 (135.480–143.590) |
| race / result / after-32 | `cmd/hufu` 269.214 (258.743–271.855); `internal/team` 0.121 (0.111–0.124); `internal/tools` 0.013 (0.013–0.018) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 300.580 (299.320–303.680); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 138.480 (137.340–145.080); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 135.990 (132.810–136.700) |
| race / warm / before | `internal/team` 941.624 (922.863–961.154); `internal/improver` 833.281 (818.167–837.629); `cmd/hufu` 314.113 (313.457–322.394) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 304.980 (298.480–310.850); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 139.390 (138.860–139.460); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 136.970 (136.330–137.770) |
| race / warm / after-32 | `internal/team` 1017.393 (1012.501–1017.877); `internal/improver` 814.739 (814.302–816.157); `cmd/hufu` 267.428 (264.835–271.164) | `TestReviewBundleSyntheticLessonAndInvalidEvidence` (internal/improver) 296.020 (295.130–298.670); `TestRefineCLIUsesFrozenOfflineExample` (cmd/hufu) 139.360 (139.260–139.870); `TestRefinementCompositionRegressionAndIndependentViews` (internal/improver) 135.490 (135.410–137.330) |

## Cohort environment and raw artifact provenance

Every source/sample within a row has the same recorded environment. CPU quota is recorded as unknown when the runner exposes no readable quota; four logical CPUs and GOMAXPROCS=2 do not prove an unlimited quota. The raw metadata retains the complete toolchain, OS, runner identity, flags and measured SHA.

| Mode / cache | OS / machine | Hostname / logical CPUs / quota | Artifact ID | ZIP SHA-256 |
| --- | --- | --- | ---: | --- |
| cgo-free / cold | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13735555985 | `fb12c7cd945d6a95335873ac9867a8d216754b9c429bfb3f11324140c84a7d44` |
| cgo-free / result | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13735981593 | `f0d971cefd0ae1284957d847491409afb511a4275c313ef7309598d4db3d1364` |
| cgo-free / warm | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13734684918 | `86f4acbef86e2198325c9b43580d79b929042900f8c8dfa8da9ab6cfc2731b07` |
| normal / cold | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13735765511 | `450d193f3a11f0d1364977ab6f332c18778865d3a8a0f42a9291ce5ee657a228` |
| normal / result | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13735236255 | `ad1010a26cc400a7234f290250987e1902cfe2713ce13aa6fcecafd6c440bc7f` |
| normal / warm | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13735207845 | `150951fc99feb537464d8f2f901e066fbb96fce77809acdff99367228fcfb089` |
| race / cold | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13741648893 | `2fec1e7b5866d19081c9902d0e8127d13f57b3157ececede75e53d3b7659aa09` |
| race / result | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13741669459 | `fc4ff212c52c64efbd53939e222d05cd7daa4c68aa7dfd9bf73da1c14c0a87da` |
| race / warm | Linux-7.0.0-1017-azure-x86_64-with-glibc2.39 / x86_64 | runnervm7aw5e / 4 / unknown | 13741481352 | `1d51ba1dbd071952ed20761c8028d06a7373cc24d9210c3d700aee9570370249` |

## Result reuse and seed costs

Counts list cached packages observed in each measured repetition. A result-replay command is not assumed to cache every package; packages that rebuild or reexecute remain in measured wall/CPU. Seed wall and CPU values sum only the serial seed commands for one repetition, then report the median and range over three repetitions. They remain outside measured-command time and inside all-attempt runner cost.

| Mode / cache / source | Cached package counts (samples 1, 2, 3) | Seed wall seconds | Seed user + system CPU seconds |
| --- | --- | ---: | ---: |
| cgo-free / cold / before | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| cgo-free / cold / after-32 | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| cgo-free / result / before | 63, 63, 63 | 505.407 (500.179–505.983) | 1015.150 (1004.957–1023.106) |
| cgo-free / result / after-32 | 65, 65, 65 | 479.022 (477.403–485.658) | 969.432 (968.797–986.234) |
| cgo-free / warm / before | 0, 0, 0 | 168.645 (166.627–169.116) | 470.763 (468.770–471.690) |
| cgo-free / warm / after-32 | 0, 0, 0 | 173.640 (171.370–173.882) | 482.965 (481.221–483.603) |
| normal / cold / before | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| normal / cold / after-32 | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| normal / result / before | 63, 63, 63 | 351.701 (346.675–352.359) | 610.590 (608.638–619.774) |
| normal / result / after-32 | 65, 65, 65 | 336.326 (330.258–348.020) | 603.241 (594.336–642.174) |
| normal / warm / before | 0, 0, 0 | 152.743 (151.138–157.495) | 411.965 (409.395–425.472) |
| normal / warm / after-32 | 0, 0, 0 | 153.215 (151.061–156.781) | 414.978 (407.851–421.842) |
| race / cold / before | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| race / cold / after-32 | 0, 0, 0 | 0.000 (0.000–0.000) | 0.000 (0.000–0.000) |
| race / result / before | 63, 63, 63 | 1726.988 (1693.308–1748.873) | 3207.103 (3204.280–3224.628) |
| race / result / after-32 | 65, 65, 65 | 1818.956 (1817.085–1848.927) | 3256.490 (3245.286–3278.887) |
| race / warm / before | 0, 0, 0 | 300.959 (300.732–301.166) | 634.401 (632.836–635.672) |
| race / warm / after-32 | 0, 0, 0 | 306.076 (304.859–306.494) | 643.642 (642.113–644.489) |

## Complete experiment cost and flake observations

Every job in every attempt of these selected experiments is included, including the original failed race job. Spans exclude the initial queue; creation-to-last spans include that gap. These are measurement costs, not production PR-CI acceleration or billed CPU.

| Experiment | Attempts / jobs | Runner seconds | First job → final completion | Creation → final completion |
| --- | ---: | ---: | ---: | ---: |
| [Original 41046691306](https://github.com/kjelly/hufu/actions/runs/41046691306), failed | 1 / 3 | 35345 | 16149 | 16151 |
| [Paired pilot 42743586308](https://github.com/kjelly/hufu/actions/runs/42743586308) | 1 / 9 | 51129 | 12551 | 12556 |
| [Co-located primary 42750628136](https://github.com/kjelly/hufu/actions/runs/42750628136) | 1 / 9 | 52476 | 12468 | 12472 |

| Experiment command evidence | Commands / nonzero exits | Child user / system CPU seconds, sum |
| --- | ---: | ---: |
| Pilot (full/cache commands, including full seeds) | 72 / 0 | 82964.021 / 6648.394 |
| Primary (measured commands and all compile/full seeds) | 108 / 0 | 86779.924 / 7638.292 |

These child CPU sums exclude toolchain/module setup, metadata probes, archive compression and other runner processes; job-duration costs include those operations. All original failures remain original failures. The new zero observed failures across 180 commands and the three initially passing correctness CIs are finite observations, not proof of a zero flake probability. Full result seeds, compile seeds and cached replays are separate evidence classes, not 180 independent fresh executions. Performance captures disable Actions cache transfer; the separate correctness audit records actual restore bytes and durations instead of inferring them from cache disk growth.

## Preserved pilot artifact inventory

These ZIPs are descriptive evidence and are not pooled into same-environment medians. All published digests were verified. Source variants and six native grep contracts were checked against raw outcomes.

| Mode / sample | CPU | Artifact ID | SHA-256 |
| --- | --- | ---: | --- |
| cgo-free / 1 | AMD EPYC 7763 64-Core Processor | 13732795329 | `85db4b7fc9087057cd81ed029270fda22d5de4a46af7e933461ae05cfa1e8e0e` |
| cgo-free / 2 | AMD EPYC 9V74 80-Core Processor | 13731996240 | `68acbc338f5aba50295925106182878ea3abe1d5fdafcec1724292a4f9bc3942` |
| cgo-free / 3 | AMD EPYC 9V45 96-Core Processor | 13731079557 | `8e95035c453a2e2d7c7404d0c94e0de403e3eb2f4dfda5839aab702e1a00cac4` |
| normal / 1 | Intel(R) Xeon(R) Platinum 8370C CPU @ 2.80GHz | 13732331349 | `47d90b1f9b20b9858fc026b700720490a200a48613eddb90666f01d72ae34d08` |
| normal / 2 | AMD EPYC 7763 64-Core Processor | 13732755946 | `dc899d97135fd6a3e7cacfcf23abd8ac13a56e709b577b301018f3bf56d81d4c` |
| normal / 3 | AMD EPYC 7763 64-Core Processor | 13732162313 | `1353efe2549de465b25b64f4ae77d1b109a647262ada2de99ea3f1156bf488ca` |
| race / 1 | AMD EPYC 7763 64-Core Processor | 13739218754 | `904f4720b7eb97a296e78eb2b3572a4dcd4e2a18dac582ae80bd1262c3f6b46d` |
| race / 2 | INTEL(R) XEON(R) PLATINUM 8573C | 13739742104 | `820ba6557196e3c4322935c0326a14fa2fc3a02b3ab925b005a76786d7a988a1` |
| race / 3 | AMD EPYC 7763 64-Core Processor | 13739089282 | `a38a40033b2e9acbe401b27565ac0447d6ca68b3c44a2fd3b9ec31a1c06dcb4c` |

## Controller and publication checks

The measurement-controller/auditor phase passed 38 Python tooling tests, including reproducible fixture variants, failed-command preservation, native execution, source/patch substitution rejection, raw count/hash binding and complete cohort admission. `mise run lint` passed with zero issues; Go sources/settings were unchanged during that phase. The later runtime repair is identified separately below. `bin/check-docs` validates 102 active / 199 total files, 679 relative links and 46 Go anchors; `git diff --check` passes. The final published-head CI is a subsequent readiness gate recorded on PR #53, rather than another performance sample.

## Publication CI exposed a concurrent-snapshot defect

The fourth correctness CI, [42773558940](https://github.com/kjelly/hufu/actions/runs/42773558940) at `d22ae604`, failed the historical-result CLI extra-model path and its aggregate; the other 13 jobs passed. Its 6740 runner seconds and raw failed integration evidence are preserved in the [CI audit](issue26-ci-cost-2026-11-10.md). This later failure is outside the frozen nine-cohort measurement and is not overwritten by a rerun.

The source snapshot copier verifies database/WAL identity, metadata and contents, but concurrent local workers can keep changing that WAL across all three attempts. The current PR therefore adds [Go function: pauseRuntimeSnapshotWriter](../../../internal/runtimestore/snapshot.go) to borrow the already-owned single connection during copy/verification, settling earlier writes and queuing later local writes. The reference prevents the last writer close from checkpointing mid-copy; connection release precedes reference release. No SQL or maintenance operation is executed by that lease. External changes still undergo every original identity/digest check, and attempts/timeouts are unchanged. Waiting consumes the caller/capture deadline and cancels without leaking the shared connection reference.

The new cancellation/serialization regression fails on pre-fix source with the new test, then passes in the fixed runtimestore race suite. All 20 snapshots during local writes must now succeed; the source-immutability, WAL, external mutation/replacement and cleanup checks remain. The historical-result CLI race regression passes all four variants and follow-ups. Local `mise run test`, `mise run vet`, `mise run lint` (0 issues) and `mise run test-contracts` all passed for this repair. The updated documentation check validates 102 active / 199 total files, 681 relative links and 47 Go anchors. A successful subsequent current-head CI remains required before ready. These are correctness checks of the new Go tree; the historical `f1faec1b`/`c6c0d0b0` performance data remain unchanged and make no performance claim for this additional fix.

## Reproduction and review gates

Dispatch the registered **Issue 26 full-suite evidence** workflow from frozen controller `fbf8faaa55d0ea66bdda446a17ebf0a0eedc6b22` (or a branch pointing to it). It runs `mise exec -- python3 scripts/issue26_acceptance.py --mode <mode> --cache-state <state> --output <new disk-backed directory>` for all nine mode/cache pairs. The controller intentionally admits hosted Actions captures only; do not bypass it to allocate private cold caches on local tmpfs. Original parents and the exact #46 patch regenerate the stated variant SHAs.

Download each raw cohort ZIP from run 42750628136 and compare its SHA-256 with the table and Actions API. At audit revision `d5c73d7e`, run `PYTHONDONTWRITEBYTECODE=1 python3 scripts/issue26_group_report.py <all-nine-cohort-zips> --output <audit.json>`; the parser checks immutable source/patch, full commands, seeds, raw case hashes/counts, native execution and co-located environments. The task audit additionally binds the published ZIP size/digest, frozen controller/harness hash and run/attempt to the API. Raw artifacts expire after 30 days; their IDs/digests and a separate complete new-experiment evidence bundle preserve provenance. The original capture remains in its earlier archive; only its normal ZIP was newly downloaded for this reconciliation.

The [assertion reconciliation](issue26-assertion-reconciliation-2026-11-10.md) accounts for every removed identity; the [six focused reports](issue26-performance-acceptance-2026-10-30.md#existing-focused-comparisons-not-new-full-suite-samples) retain their controlled scopes, including #32’s lack of a median speedup. The [CI audit](issue26-ci-cost-2026-11-10.md) reports complete short/integration union membership, actual native grep race coverage, four selected complete pre-fix CI costs, including the failed publication run and observed cache transfers. These gates, rather than the existence of a workflow or a successful parser alone, support the manual acceptance.
