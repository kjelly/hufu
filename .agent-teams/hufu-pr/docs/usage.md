# GitHub pull request tasks

`hufu-pr` takes a GitHub.com PR URL or a repository-qualified number and one
read-only task. It needs a configured model and `gh` on PATH with access to
that repository. The target can be outside the project where Hufu is running.

```bash
hufu run --team hufu-pr --new "Summarize kjelly/hufu#51"
hufu run --team hufu-pr --new "Why did CI fail on kjelly/hufu#51?"
hufu run --team hufu-pr --new "Review https://github.com/kjelly/hufu/pull/51"
hufu run --team hufu-pr --new "Assess whether kjelly/hufu#51 is ready to merge"
```

Codex can also provide a structured target. The `action` values are
`summarize`, `triage_ci`, `review`, and `assess_readiness`.

```bash
hufu run --team hufu-pr --new --input 'pr.target={"action":"triage_ci","repository":"kjelly/hufu","number":51}' "Check CI failures for kjelly/hufu#51"
```

| Action | Evidence |
| --- | --- |
| Summarize | PR title, author description, branch names and GitHub change totals |
| Diagnose CI | PR check statuses, failed steps and excerpts from up to three failing or cancelled GitHub Actions jobs |
| Review | PR metadata, checks, the complete changed-file list, and complete patches that fit the evidence budget |
| Assess readiness | Draft and merge state, GitHub review decision, checks, and up to 100 review threads |

Review takes up to 256 changed files and 128 KiB of evidence. It reserves
space for the file list before adding patches and identifies paths whose
patches are unavailable or exceed the remaining budget. The other actions
use metadata or logs and do not download patches. CI excerpts include
selected diagnostics and a bounded log tail. The availability of job logs
depends on GitHub access and retention.

The trusted GitHub adapter masks recognizable credentials before publishing
evidence. If masking would invalidate a patch's line ranges, the report lists
the patch as unavailable. The reviewer reads only the assigned artifacts using
`view_artifact`, with local filesystem tools and worker tool network access
disabled. The collector validates a JSON report against the team contract.

Each report identifies the GitHub base and head SHAs. Multi-request actions
verify those SHAs again before publishing evidence. Merge readiness describes
the observed GitHub signals at collection time. The `passed` count includes
GitHub's successful, skipped, and neutral checks. Cancelled checks count as
failed and qualify for CI log collection. The team reports its findings in
Hufu, and GitHub operations are read-only.
GitHub's `HAS_HOOKS` state remains `undetermined` when its other signals pass.
The read-only assessment cannot establish the outcome of a pre-receive hook.
