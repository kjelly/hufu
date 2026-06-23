---
name: git-commit-message
description: Generate a conventional commit message from a staged diff. Use when the user asks to write a commit, format a message, or describe staged changes.
---

# Git Commit Message Skill

When invoked with a staged git diff, produce a conventional commit message.

## Format

```
<type>(<scope>): <description>

<body>

<footer>
```

Where:
- `type` is one of: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert
- `scope` is the module or area affected (e.g. `cli`, `tui`, `team`, `mcp`, `tools`)
- `description` is a one-line summary, present tense, imperative mood, under 72 chars
- `body` explains *why* the change was made (not *what* — the diff shows that)
- `footer` references issues (`Closes #123`, `Refs #456`) or notes breaking changes

## Steps

1. Run `git diff --staged` to read the changes.
2. Identify the primary type from the change:
   - New functionality → `feat`
   - Bug fix → `fix`
   - Tests only → `test`
   - Documentation only → `docs`
   - Refactor without behavior change → `refactor`
3. Identify the scope from the changed files (e.g. files under `cmd/hufu/` → `cli`).
4. Write the description as if completing "This change will ___".
5. If the change is non-trivial, write a 2-4 line body explaining motivation.
6. If the change is breaking, append `BREAKING CHANGE: <description>` to the footer.

## Anti-patterns to avoid

- Past tense ("added", "fixed") — use imperative ("add", "fix").
- Trailing period in the description line.
- Vague descriptions like "update code" or "fix bug".
- Mixing multiple unrelated changes in one commit — suggest splitting.
