---
name: hufu-review-recent-commits
description: Run the hufu-code-review agent team on the most recent Git commits of this repository, choosing the largest commit count that stays within the team's limits, or lowering a requested count that would exceed them. Use when asked to review the latest or recent N commits with hufu-code-review (for example 「用 hufu-code-review review 最近幾個 commit」).
---

# Hufu Review Recent Commits

Run this command from the repository root, with `N` replaced by a commit count
that the team can review without hitting a limit:

```bash
mise exec -- go run -p 2 ./cmd/hufu --team hufu-code-review --model ollama/minimax-m3.1-flash:cloud --auto-approve --report --new "review 最近N個的 git commit，告訴我這些修改有無潛在問題"
```

Never choose `N` by hand. The limits depend on how much the commits change,
not on how many there are, so only the picker script can tell which counts fit.

## 1. Pick the commit count

```bash
python3 .agents/skills/hufu-review-recent-commits/scripts/pick_commit_count.py
```

- If the user named a count, pass `--count N`. The script keeps it when it
  fits and otherwise lowers it to the largest count that does.
- Without `--count`, the script looks for the largest count that fits, up to
  the team's maximum of 100.
- Pass `--model` only when the user asks for a different model.

The script needs Git, PyYAML, and mise with the repository's pinned Go toolchain.
It runs Go through `mise exec` from the repository root, preserves configured
caches and `GOTMPDIR`, and limits builds to two parallel packages. It builds
the team's workset producer and runs it for
a few candidate counts, usually in 1–3 minutes. Progress goes to stderr; stdout
is a JSON summary:

| Field | Meaning |
| --- | --- |
| `count` | The chosen commit count. The producer accepted it and it passed every check below. |
| `head` | The commit the count was measured against. |
| `selected` | The size of the chosen scope: changed paths, diff lines, items per route, reviews, and the worst-case evidence sources. |
| `probes` | Every count that was tried, with the reason each rejected count failed. |
| `notes` | A lowered request or a longer test step; relay these to the user. |
| `command` | The command above with `N` already filled in. |

If the script exits with an error, report the error and stop. Do not guess a
count or fall back to a default.

## 2. Run the review

Tell the user the chosen count, any `notes`, and that the run takes a while:
about 25 minutes for 3 commits, and roughly 1.5–2.5 hours near the limit.
Then run `command` exactly as printed, from the repository root.

- Do not commit, check out, or rebase between picking and running. The count
  was measured at `head`; if HEAD has moved, run the picker again.
- If your shell tool cannot wait that long, start the run detached and keep
  its output inside the task's private `/tmp/codex-task.XXXXXX` directory.
  Pass that directory as `TMPDIR`, record the process ID, and keep logs bounded.
  Follow progress with the log and
  `mise exec -- go run -p 2 ./cmd/hufu status --team hufu-code-review --json`.
  To investigate a stall or failure, use `$hufu-diagnose-invocation`.
  Wait for the process and all consumers to exit before removing its temporary
  outputs; preserve the requested report.

## 3. Report the result

When the run ends, give the user the run's outcome and the findings from the
`--report` output at the end of the log. A completed run with findings is a
successful review. If the run ended blocked or failed, say so and include the
failing task and its reason. Do not present a partial report as a complete
review.

## What the picker checks

The script reads every limit from `.agent-teams/hufu-code-review/team.yaml`
and the team's result schemas, so it follows changes to the team. A count
passes only when all of these hold:

1. The producer accepts the scope. It rejects scopes over its totals (2 MiB of
   diff, 30,000 diff lines, 256 changed paths, or 64 review items) and items
   it cannot split. This rejection costs nothing, because the run stops
   before any model call.
2. No route has more items than the schemas' `workset_key` enum allows
   (`unit-0000` to `unit-0031`). The producer does not check this; the run
   would fail partway, after spending time and tokens, when a review cannot
   name its item.
3. All reviews, including no-op items, plus one possible critic per real
   review fit within the synthesizer's `max-evidence-sources` (64).
4. The count does not exceed the `review.scope` maximum (100) or the
   first-parent history length.

The search assumes a larger count never makes the scope smaller, which holds
unless a later commit reverts a large earlier one. The chosen count is always
one the producer actually accepted, so a rare reversal can only make the
choice smaller than necessary, never unsafe.

If the range changes `go.mod`, `go.sum`, `go.work`, or `go.work.sum`, the
targeted-test step runs `go test ./...` instead of the changed packages. That
takes longer, and the verifier allows 1200 seconds. The script adds a note
when this applies.
