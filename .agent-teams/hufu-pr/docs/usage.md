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
| Review | PR metadata, checks, and the full set of available PR patch excerpts |
| Assess readiness | Draft and merge state, GitHub review decision, checks, and up to 100 review threads |

Review takes up to 256 changed files and 128 KiB of evidence. The other
actions use metadata or logs and do not download patches. Incomplete or
unavailable observed evidence is listed in the report. CI excerpts include
selected diagnostics and a bounded log tail. The availability of job logs
depends on GitHub access and retention.

Each report identifies the GitHub base and head SHAs. Multi-request actions
verify those SHAs again before publishing evidence. Merge readiness describes
the observed GitHub signals at collection time. The `passed` count includes
GitHub's successful, skipped, and neutral checks. Cancelled checks count as
failed and qualify for CI log collection. The team reports its findings in
Hufu, and GitHub operations are read-only.
