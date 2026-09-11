# Skill Generation Quality Improvement — Design Specification

**Date**: 2026-06-16
**Author**: hufu development team
**Status**: Draft (pending review)

---

## Executive Summary

The current auto-skill generator produces excessive noise. In one observed run, 97 `draft-*` skill directories were created in a single team — most being low-quality candidates with rule-based names like `draft-bash-bash-bash` or `draft-view-view-view`. The user has no clean way to clean them up; once generated, drafts persist forever in the team skills directory and pollute the LLM's skill context.

This spec introduces three changes to fix this end-to-end:

1. **Better candidate filtering** — fix dead config fields, dedupe overlapping windows, wire up the existing-but-unused semantic merge, sort-then-cap.
2. **Draft isolation** — drafts move to a `drafts/` subdirectory and are excluded from the LLM's default skill pool.
3. **Lifecycle management** — the user gets a multi-select confirm prompt, a `hufu skill promote` command, and a `hufu skill clean` command; usage is persisted to disk so the cleanup command can identify unused drafts.

The expected outcome: from one session's worth of repeating patterns, ≤ 3 high-quality drafts are created (down from 97), and the user has a clear path to either promote them to real skills or delete them.

---

## Current State Analysis

### Generation pipeline (`internal/skill/discovery.go`, `internal/skill/generator.go`)

```
Worker tool calls
   │  RecordToolCall (in-memory, bounded to 1000 calls)
   ▼
[end of session round]
FindCandidates(ctx)
   │  freq >= minFrequency  (BUG: d.minFrequency is dead; const=10 always wins)
   │  drop single-tool repeats
   │  LLM generalization eval (max 5)
   │  qualityThreshold >= 0.7
   │  BUG: "top 5" is wrong — break in map-iteration order
   ▼
displaySkillPreviewAndConfirm → user answers Y/n (binary)
   │
   ▼
GenerateSkill → skills/draft-*/SKILL.md   (in same dir as real skills)
```

### Why 97 drafts accumulated

1. **Windowing explodes candidates.** A 10-call sequence produces 8 distinct overlapping windows (3-10 call windows), each with its own hash. Each is checked independently. A long session easily produces 50+ distinct hashes.
2. **No dedup of overlapping windows.** `view→edit→bash` and `view→edit→bash→grep` are separate candidates.
3. **Dead `minFrequency` field.** `coordinator.go:666` passes `5` but `discovery.go:24` declares `minFrequency = 10` and `FindCandidates:381` uses the const. The constructor argument is silently ignored.
4. **`maxSkillCandidates=5` doesn't work.** Line 436-438 has a `break` after 5 LLM evals, but candidates already collected in `candidates[i]` stay in the slice. The "top 5 by quality" guarantee is broken because map iteration is random.
5. **Drafts and active skills share a directory.** `draft-` is just a name prefix. `DiscoverSkills` returns everything.
6. **No lifecycle.** No way to delete, no way to track usage, no way to promote. The `cmd/hufu/skill.go` CLI has `list` and `review` only.

### Existing in-memory usage tracking

`coordinator.recordSkillUsage` (line 759) is called when `load_skill` succeeds. It updates an in-memory `map[string]*skillUsageState{Name, Count, Agents}`. This is reported at session end. **The data is not persisted** — every new session starts at zero.

### Existing semantic merge (not wired up)

`analyzeSemanticSimilarity` (discovery.go:477) is defined and complete, including `mergeSimilarSequences`, `clusterDescriptions`, `enrichLLMNames`. It is **not called from `FindCandidates`**. The wiring code path is dead.

---

## Design Goals

1. **Cut noise 90%+** — from ~97 candidates per session to ≤ 5 high-quality drafts.
2. **Isolate drafts from active skills** — drafts do not appear in the LLM's default skill context.
3. **Give the user control** — multi-select confirm, `promote` and `clean` subcommands.
4. **No silent deletion** — clean is manual, never auto.
5. **No behavior regressions for existing users** — when the sidecar is unavailable or candidates are filtered out, the system behaves as it does today.
6. **Backward compatible** — old `draft-*/SKILL.md` files migrate automatically to the new layout.

---

## Non-Goals

- Not changing the rule-based or LLM-based name generation algorithms.
- Not changing the `ToolSequence` windowing algorithm (3–10 calls).
- Not touching the in-memory `skillUsage` tracking that the report / display already use (we add *persistence* on top, not a replacement).
- No new dependencies.
- No auto-cleanup on session start.
- No auto-promotion of high-quality drafts.

---

## Proposed Design

### 1. Filter & dedup candidates

**Bug fixes in `internal/skill/discovery.go`:**

- Remove the package-level `minFrequency` constant. The field `d.minFrequency` set by the constructor is the single source of truth. The const loses its purpose. (Alternative: keep the const as the *default* if `NewSkillPatternDetector(0, ...)` is called. The implementation will keep the const for this purpose.)
- Replace the `break` at line 436-438 with proper sort-then-take-top-N. After all candidates have been scored, sort by `QualityScore` desc, then truncate to `maxSkillCandidates`.

**New filter steps added to `FindCandidates`** (in order, after the existing frequency filter):

1. **Prefix-dedup.** Sort candidate tool sequences by length descending. For each candidate, check whether any already-kept candidate is a strict prefix of it. If yes, skip. (e.g. `view→edit` is dropped if `view→edit→bash` is present.)
2. **Semantic merge.** Wire up the existing `analyzeSemanticSimilarity` (line 477). This collapses candidates whose task descriptions cluster together.
3. **Sort + cap.** Sort by `QualityScore` desc, then take top `maxSkillCandidates` (default 5).
4. **Session cap.** New field `maxDraftsPerSession` (default 3) on the coordinator, applied as the final cap.

**Files:**
- `internal/skill/discovery.go` — bug fixes, new dedup step, wire semantic merge, new constant and field
- `internal/skill/discovery_test.go` — tests for the new dedup and sort behavior

### 2. Drafts in a separate directory

**Generator change** (`internal/skill/generator.go`):

- `GenerateSkill(candidate)` writes to `<baseDir>/drafts/<skillName>/SKILL.md` instead of `<baseDir>/<skillName>/SKILL.md`. The new `drafts/` subdir is created automatically.
- The generated frontmatter gains two new fields: `created_at` and `last_modified`, both set to the current time. This is the data source for `clean --older-than`.
- `save_skill` (coordinator's `saveAndReloadSkill`) does **not** change — it still writes to `<baseDir>/<slug>/SKILL.md` (active skills).

**Loader change** (`internal/skill/skill.go`):

- `SkillDef` gets a new field `Draft bool`.
- `DiscoverSkills(dirs)` gets a new parameter `includeDrafts bool` (default `false`):
  - For each `dir` in `dirs`, scan `dir/*/SKILL.md` → set `Draft = false`.
  - For each `dir` in `dirs`, scan `dir/drafts/*/SKILL.md` → set `Draft = true`. Only included when `includeDrafts=true`.
- Callers that want drafts (e.g. `cmd/hufu/skill list`) pass `includeDrafts=true`. Callers that load skills for the LLM pass `false`.

**CLI change** (`cmd/hufu/skill.go`):

- `hufu skill list` shows all skills, marking drafts with a `[draft]` tag. New `--drafts-only` flag filters.
- `hufu skill review <name>` works on drafts (existing behavior preserved).

**Migration** (`cmd/hufu/main.go`):

- On `runTeam` startup, scan for any legacy `skills/draft-*/SKILL.md`. If found, move them to `skills/drafts/draft-*/SKILL.md` (preserving the `draft-` prefix). Print one log line. Idempotent.

**Files:**
- `internal/skill/generator.go` — write to `drafts/` subdir, add frontmatter fields
- `internal/skill/skill.go` — `DiscoverSkills` with `includeDrafts`, new `Draft` field, parse `created_at`
- `internal/skill/skill_test.go` — new tests for draft discovery and frontmatter
- `internal/team/parse.go:647`, `internal/team/coordinator.go:860` — pass `includeDrafts=false`
- `cmd/hufu/main.go` — one-time migration
- `cmd/hufu/skill.go` — `[draft]` tag, `--drafts-only` flag

### 3. Per-candidate confirm + lifecycle CLI

**Multi-select confirm** (`internal/team/coordinator.go`):

- `displaySkillPreviewAndConfirm` is replaced with a new function that:
  - Renders the candidate list (same as today).
  - Sends a single prompt: `Keep which? (e.g. "1,3" / "a" for all / "n" for none; default: n): `
  - Parses the user's response:
    - `n`, empty → return empty list
    - `a` → return all candidates
    - `1,3` → return those indices (1-based, validated against len)
    - Anything else → warn and re-prompt
  - For TUI mode, uses the existing `ask_user` infrastructure with `allow_any=true` and a short hint about the syntax.

**Usage persistence** (`internal/skill/lifecycle.go` — new file):

- `UsageStats` struct: `{Name, UsedCount, FirstUsed, LastUsed}`.
- `LoadUsageStats(workspaceDir string) (map[string]UsageStats, error)` — reads `<workspace>/.skill-usage.json`.
- `SaveUsageStats(workspaceDir string, stats map[string]UsageStats) error` — atomic write (write to tmp, rename).
- `RecordUsage(workspaceDir, skillName, agentName string) error` — load → update → save.
- `PromoteDraft(skillsDir, draftName string) (string, error)` — moves `<skillsDir>/drafts/<name>/` to `<skillsDir>/<newName>/`, stripping the `draft-` prefix.
- `CleanDrafts(skillsDir string, opts CleanOpts) (CleanResult, error)`:
  - `CleanOpts{OlderThan time.Duration, UnusedOnly bool, DryRun bool}`
  - `CleanResult{Deleted []string, Kept []string}`
  - Iterates `skillsDir/drafts/*/SKILL.md`, filters by `created_at` and `UsedCount`, deletes matching dirs.

**Coordinator integration** (`internal/team/coordinator.go`):

- `recordSkillUsage` (line 759) calls `lifecycle.RecordUsage` to persist. Batched: only write to disk every 10 records or at session end, to avoid thrash.
- New `HotReloadSkills()` method (or similar) so `cmd/hufu skill promote` can refresh the in-memory `c.skills` after promotion.

**CLI** (`cmd/hufu/skill.go`):

- `hufu skill promote <draft-name>` — calls `lifecycle.PromoteDraft`. Optionally accepts `--no-reload`. Prints the new path.
- `hufu skill clean` — flags:
  - `--older-than 30d` (default: no age filter)
  - `--unused` (default: false; if true, only deletes drafts with `UsedCount == 0`)
  - `--dry-run` (default: true; `--apply` to actually delete)
  - `--yes` (skip the final confirm)
  - Print plan first, then either confirm or execute.

**`save_skill` tool** (`internal/team/skill_tool.go`):

- Add optional parameter `as_draft bool` (default `false`). When `true`, writes to `skills/drafts/<slug>/SKILL.md` instead of `skills/<slug>/`. This gives the coordinator (or a human via the agent) a way to save a manual skill as a draft first.

**Files:**
- `internal/team/coordinator.go` — multi-select confirm, persist usage, hot-reload
- `internal/skill/lifecycle.go` — new file
- `internal/skill/lifecycle_test.go` — new tests
- `internal/team/skill_tool.go` — `as_draft` param
- `internal/team/skill_tool_test.go` — tests for the new param
- `cmd/hufu/skill.go` — `promote` and `clean` subcommands, `--drafts-only` flag
- `cmd/hufu/skill_test.go` (new) — CLI tests

---

## Data flow

### Skill generation (new)

```
Worker tool calls
   │  RecordToolCall (in-memory, bounded to 1000 calls)
   ▼
[end of session round]
FindCandidates(ctx)
   │  freq >= d.minFrequency          (field, not const)
   │  drop single-tool repeats
   │  prefix-dedup                    (NEW: drop shorter prefix duplicates)
   │  analyzeSemanticSimilarity       (NEW: was defined but never called)
   │  LLM generalization eval
   │  qualityThreshold filter
   │  sort by QualityScore desc
   │  take top maxSkillCandidates
   │  cap at maxDraftsPerSession      (NEW)
   ▼
displaySkillPreviewMultiSelect → "1,3" / "a" / "n"
   │
   ▼
GenerateSkill(cand) → <baseDir>/drafts/<name>/SKILL.md   (CHANGED)
   │  frontmatter: +created_at, +last_modified
   ▼
display message: "drafts saved to skills/drafts/, run hufu skill list / promote / clean"
```

### Skill loading (new)

```
DiscoverSkills(dirs, includeDrafts=false)
   │
   ▼
for each dir in dirs:
   scan dir/*/SKILL.md          → SkillDef.Draft=false
   if includeDrafts:
     scan dir/drafts/*/SKILL.md → SkillDef.Draft=true
   │
   ▼
return []*SkillDef  (drafts excluded by default)
```

### Usage tracking (new)

```
load_skill tool called
   │
   ▼
recordSkillUsage(name, agent)
   │  - in-memory map update
   │  - mark dirty
   ▼
[every 10 records or session end]
lifecycle.RecordUsage(workspaceDir, ...)
   │
   ▼
write <workspace>/.skill-usage.json (atomic)
```

### Cleanup (new)

```
$ hufu skill clean --older-than 30d --unused --dry-run
   │
   ▼
lifecycle.CleanDrafts(skillsDir, CleanOpts{DryRun: true})
   │  - list skillsDir/drafts/*/SKILL.md
   │  - parse frontmatter.created_at
   │  - filter: older than X AND used_count == 0
   │  - print plan
   ▼
[confirm: y/N]
   │
   ▼
lifecycle.CleanDrafts(skillsDir, CleanOpts{DryRun: false})
   │  - os.RemoveAll each matched dir
   ▼
print "deleted N drafts"
```

---

## Frontmatter schema change

Generator now emits an additional frontmatter field:

```yaml
---
name: draft-view-edit-bash
description: Use when ...
created_at: 2026-06-15T10:23:00Z
last_modified: 2026-06-15T10:23:00Z
---
```

This is backward-compatible (existing parsers ignore unknown fields). The `skillFrontmatter` struct in `skill.go:77` gets new fields `CreatedAt time.Time` and `LastModified time.Time`, parsed by `parseSkillYAML` falling back to file mtime if missing.

For the migration of old drafts: when a draft without `created_at` is encountered, the loader treats it as having `created_at = file mtime`. The migration command (`cmd/hufu/main.go`) also touches the file mtime so old drafts naturally show a real age.

---

## Bugs found during investigation (will be fixed in 1.1)

| # | Location | Bug | Fix |
|---|---|---|---|
| 1 | `discovery.go:381` | `d.minFrequency` field is set by constructor but never read; the const `minFrequency=10` always wins. The `5` passed by `coordinator.go:666` is dead. | Use `d.minFrequency` as the source of truth. The const becomes the default for the constructor (`NewSkillPatternDetector(0, 3, 10)` → uses 10). |
| 2 | `discovery.go:436-438` | `break` after 5 LLM evaluations depends on map iteration order — "top 5 by quality" is not actually top 5. | Sort first, then take top-N. |
| 3 | `discovery.go:477` | `analyzeSemanticSimilarity` is defined but never called from `FindCandidates`. | Wire it up between the prefix-dedup step and the LLM eval. |

---

## Risk & migration

### Migration of existing drafts

All current drafts live in `skills/draft-*/SKILL.md`. They need to move to `skills/drafts/draft-*/SKILL.md`. The implementation includes a one-time migration in `runTeam` startup:

1. For each team directory, check if `skills/draft-*/SKILL.md` exists.
2. If yes, create `skills/drafts/` and move each `skills/draft-*/` → `skills/drafts/draft-*/` (preserving the prefix).
3. Print one log line: `migrated N draft skills to skills/drafts/`.
4. Idempotent: if `skills/drafts/draft-*/` already exists with the same name, skip (refuse to overwrite, log a warning).

### Backwards compatibility

- `save_skill` tool: stays writing to `skills/<slug>/` (active path). The new `as_draft=true` param is a new feature, not a behavior change.
- `DiscoverSkills`: the new `includeDrafts` parameter is mandatory at call sites; existing callers that didn't need drafts are updated to pass `false`. No new drafts leak into the LLM context.
- `cmd/hufu skill review` and `cmd/hufu skill list`: continue to work; list now shows drafts with a tag.

### TUI compatibility

The new multi-select prompt must work in TUI mode. The existing `ask_user` infrastructure (which already supports `multiple_choice` and free-text fallback) is reused. The candidate list is rendered as the question text; the user types `1,3` / `a` / `n` in the free-text field.

---

## Testing

### Unit tests (added in 1.1, 1.2, 1.3)

| File | Tests |
|---|---|
| `internal/skill/discovery_dedup_test.go` (new) | prefix-dedup; semantic merge wired; sort-then-take-top-N; `maxDraftsPerSession` cap |
| `internal/skill/discovery_test.go` | extend `TestCalculateQualityScore` to cover the new field-as-source-of-truth |
| `internal/skill/skill_test.go` | `DiscoverSkills` with `includeDrafts=true/false`; draft subdir scanning; parse `created_at` |
| `internal/skill/lifecycle_test.go` (new) | `RecordUsage`, `PromoteDraft`, `CleanDrafts` (all branches: older-than, unused, dry-run, apply) |
| `internal/team/skill_tool_test.go` | `as_draft=true` writes to `drafts/`; `as_draft=false` (default) keeps old behavior |
| `cmd/hufu/skill_test.go` (new) | `hufu skill list` (with/without drafts), `promote`, `clean` (all flags) |

### Integration / regression

- `go test ./...` after each sub-PR
- Manual: start a `reviewer` session, do 10+ tool calls of a repeating pattern, confirm:
  - ≤ 3 drafts are created
  - Drafts land in `skills/drafts/` subdir
  - Multi-select prompt appears with the right syntax
  - After session, `<workspace>/.skill-usage.json` is written
  - `hufu skill clean --dry-run` lists unused drafts
  - `hufu skill promote <name>` moves a draft out and reloads
  - Existing non-draft skills are not affected

---

## File summary

| File | Sub-PR | Change |
|---|---|---|
| `internal/skill/discovery.go` | 1.1 | Bug fixes (dead field, ordering) + prefix-dedup + wire semantic merge + new `maxDraftsPerSession` field |
| `internal/skill/discovery_test.go` | 1.1 | Extend tests for new behavior |
| `internal/skill/discovery_dedup_test.go` | 1.1 | New: dedup, sort, cap |
| `internal/skill/generator.go` | 1.2 | Add `created_at`/`last_modified` to frontmatter; write to `drafts/` subdir |
| `internal/skill/skill.go` | 1.2 | `DiscoverSkills` with `includeDrafts` flag; new `Draft` field on `SkillDef`; parse `created_at` |
| `internal/skill/skill_test.go` | 1.2 | Tests for draft discovery |
| `internal/skill/lifecycle.go` | 1.3 | New file — usage stats, promote, clean |
| `internal/skill/lifecycle_test.go` | 1.3 | New tests |
| `internal/team/coordinator.go` | 1.1, 1.2, 1.3 | Filter wiring + multi-select confirm + persist usage + hot-reload |
| `internal/team/skill_tool.go` | 1.3 | `as_draft` parameter on `save_skill` |
| `internal/team/skill_tool_test.go` | 1.3 | Tests for the new param |
| `internal/team/parse.go` | 1.2 | Pass `includeDrafts=false` to `DiscoverSkills` |
| `cmd/hufu/skill.go` | 1.2, 1.3 | New `promote` and `clean` subcommands; `--drafts-only` flag on `list` |
| `cmd/hufu/skill_test.go` | 1.3 | New CLI tests |
| `cmd/hufu/main.go` | 1.2 | One-time migration: move `skills/draft-*/` → `skills/drafts/draft-*/` |

---

## Open questions / decisions deferred

- **Auto-cleanup on session start** — manually rejected in this spec; could be a follow-up if desired.
- **Aggressive semantic merge** — using existing 0.9 threshold; tunable later.
- **`minFrequency` default** — kept as the default-value init for the constructor; field is the source of truth.

---

## Rollout

1. Land 1.1 in one PR (filtering). Lower blast radius.
2. Land 1.2 in one PR (drafts/ subdir + migration). Includes the migration code which is backward-compatible.
3. Land 1.3 in one PR (lifecycle CLI + multi-select). Largest surface area; consider a `--enable-skill-cleanup` flag if you want to keep it dark until tested.
4. No deprecation. No behavior change for users who don't use the auto-skill feature.
