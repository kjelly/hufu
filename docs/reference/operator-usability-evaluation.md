# Operator usability evaluation

> Status: draft
> Authority: reference
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

This is the Phase 7 human-evaluation record required by
`docs/architecture/operator-experience.md` section 18. It is intentionally
incomplete: no real participant session has been performed or supplied.
Automated tests and model judgments are not substitutes for this report.

## Current result

| Field | Value |
|---|---|
| Evaluation date | pending |
| Participant count | **0** |
| New Hufu users | 0 of recommended 3 |
| Existing Hufu users | 0 of recommended 3 |
| Baseline observations | pending |
| Candidate observations | pending |
| Safety incidents | not measured |
| Decision | **not passed; keep operator-experience surfaces preview** |

Because the denominator is zero, success rate, median duration, median action
count, and improvement percentages are unknown—not zero.

## Session protocol

1. Recruit at least three engineers new to Hufu and three with an existing
   Hufu workflow. Obtain explicit consent for screen/voice recording.
2. Use the same Journeys A–F corpus for baseline and candidate. Counterbalance
   order across participants and record the presented order.
3. During a measured task, do not explain subsystems or suggest the next
   command. Participants may use normal help and UI.
4. Record only cohort, scenario/order, elapsed time, action count, answer
   correctness, error category, incomplete reason, and interface mode. Do not
   retain prompts, memory, tool arguments, secrets, complete paths, or document
   contents.
5. Score State, What, and Next against the facilitator key after the task.
   Preserve failed and abandoned scenarios in the final report.

Create separate worksheets before each condition:

```sh
scripts/operator-usability-measure.sh baseline /tmp/hufu-baseline.md
scripts/operator-usability-measure.sh candidate /tmp/hufu-candidate.md
```

Do not commit raw recordings or identifying participant data. Commit only the
redacted aggregate and anonymous per-scenario rows needed to reproduce the
decision.

## Acceptance report

| UX metric | Required result | Observed result | State |
|---|---|---|---|
| Three-question recognition | >=90% correct within 10 seconds | not measured | pending |
| First verified run | provisioned median <=10 minutes and no baseline regression | not measured | pending |
| Failure next action | median necessary actions >=30% lower than baseline; zero safety errors | not measured | pending |
| Workspace selection | zero wrong-scope writes and participant identifies exact path | not measured | pending |
| Promotion comprehension | every participant distinguishes proposed/approved/applied | not measured | pending |
| E-paper comfort/control | keyboard-complete, no input loss, persistent flashing, or color-only signal | not measured | pending |
| Power-user efficiency | baseline scripts do not gain mandatory interaction | deterministic non-TTY tests pass; human replay pending | pending |

## Per-participant results

No rows yet. The final report must merge the two generated worksheets and add
an anonymous cohort (`new` or `existing`), environment-provisioned duration,
install/model-loading duration, interface mode, error category, and incomplete
reason for every scenario.

## Promotion rule

The report may be changed from draft to active only after the raw worksheets
have been checked, the aggregates above are reproducible, every safety metric
is zero, and unmet non-safety targets are either fixed and retested or left
explicitly preview. A failed gate must not be addressed by weakening
verification, authorization, or confirmation.
