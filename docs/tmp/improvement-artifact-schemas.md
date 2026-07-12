# Agent Team Improvement Artifact Schemas（Phase 1–2）

本文件定義 Phase 1 的改善產物。所有欄位都必須維持 metadata-only：不得包含 task prompt、agent output、tool arguments、tool results、secrets 或可識別使用者資料。

## Improvement Report

由 `hufu improve --format json [--runs N]` 產生。這是 Phase 1 唯一由程式直接產生的 artifact。

```json
{
  "team": "dev-team",
  "workspace": "/path/to/workspace",
  "generated_at": "RFC3339 timestamp",
  "source": "hufu improve",
  "run_ids": ["run-a", "run-b"],
  "team_revisions": ["sha256 definition hash"],
  "metrics": {
    "run_count": 2,
    "total_tasks": 12,
    "done": 10,
    "error": 1,
    "planned": 1,
    "total_attempts": 15,
    "retried_tasks": 3,
    "total_tokens": 42000,
    "tool_calls": 32,
    "tool_errors": 2
  },
  "trend": [],
  "groups": {
    "by_agent": [],
    "by_task_type": [],
    "by_model": [],
    "by_skill": []
  },
  "findings": []
}
```

`trend` 的每個項目包含單一 `run_id`、時間窗、當時的 `team_revision` 與該 run 的 metrics。`groups` 的項目包含 `key`、tasks、done、error、planned、attempts、retried tasks 與 total tokens。skill 分組可重疊：一個 task 關聯兩個 skills 時，會出現在兩組中。

## Finding

finding 由 deterministic rule 產生，供人工審核與後續 hypothesis 使用。

```json
{
  "layer": "agent | team",
  "target": "developer",
  "severity": "critical | warning | suggestion",
  "category": "prompt | tools | guard",
  "metric": "retry_rate",
  "value": "3/12 (25%)",
  "suggestion": "具體、可執行的改善方向",
  "source_rule": "retry_rate",
  "evidence": "metadata-only 摘要",
  "confidence": "high | medium | low",
  "run_ids": ["run-a", "run-b"],
  "team_revisions": ["sha256 definition hash"]
}
```

`run_ids` 與 `team_revisions` 是必要的追溯鏈；不可以用原始 output 取代 evidence。

## Hypothesis（人工建立，不自動套用）

```yaml
id: H-023
status: proposed # proposed | approved_for_experiment | rejected | adopted
finding_ids: [F-001]
team: dev-team
baseline_revision: sha256-definition-hash
symptom: "重構任務重試率偏高"
evidence:
  run_ids: [run-a, run-b]
  metric: retried_tasks
  observed: "6/10 (60%)"
change:
  files:
    - path: developer.md
      intent: "補上 API 相容性與測試交付條件"
success_criteria:
  - "acceptance / verification 通過率不得下降"
  - "重試率低於 30%"
risks:
  - "平均執行時間增加"
review:
  owner: "human reviewer"
  approved_at: null
```

Hypothesis 只提出可測試變更；它不是對正式 team 的寫入授權。

## Phase 2：Benchmark Fixture

Benchmark fixture 是刻意編寫且可納入 Git 的測試輸入；prompt 只儲存在 fixture，不會進入 execution telemetry、improvement report 或 experiment report。

```yaml
version: 1
name: refactor-smoke
team: dev-team
category: refactor
description: "Representative compatibility and safety checks"
cases:
  - id: api-compat
    type: happy # happy | failure | edge | safety
    prompt: "Refactor the API without breaking callers."
```

fixture revision 是 canonical JSON 的 SHA-256。建立後不可覆寫；修改 fixture 需以新的名稱或 Git revision 建立新的實驗輸入。

## Phase 2：Team Snapshot

baseline 與 candidate 都是複製到 `workspace/improvement/` 的 immutable team definition。candidate 必須：

- 指向 baseline ID；
- 使用相同 team name；
- 與 baseline 有不同 content revision；
- 附帶非空的 review patch。

```json
{
  "version": 1,
  "id": "candidate-023",
  "kind": "candidate",
  "team": "dev-team",
  "definition_revision": "telemetry-compatible definition hash",
  "content_revision": "full snapshot content hash",
  "patch_revision": "review patch hash",
  "baseline_id": "baseline-017",
  "created_at": "RFC3339 timestamp"
}
```

## Experiment Report

```yaml
id: E-014
hypothesis_id: H-023
status: planned # planned | running | passed | failed | inconclusive
baseline:
  team_revision: sha256-definition-hash
candidate:
  team_revision: sha256-definition-hash
  patch_ref: git-or-candidate-reference
benchmark_revision: benchmark-set-revision
gates:
  safety_violations: 0
  acceptance_non_regression: true
results:
  baseline: {}
  candidate: {}
decision: eligible_for_review # eligible_for_review | retry | reject
gates:
  - baseline_control
  - candidate_safety
  - candidate_acceptance
  - completion_non_regression
  - error_non_regression
```

`hufu improve experiment compare` 會以 benchmark、snapshot、兩份 Phase 1 JSON report 與明確記錄的 acceptance/safety 結果執行 deterministic A/B gate。結果會寫入 JSON 與 Markdown，但 `eligible_for_review` **不是採納授權**：不會改動正式 team、建立 PR 或合併變更。

## Phase 2 CLI Workflow

```bash
# 1. 建立並提交 benchmark fixture
hufu improve benchmark create refactor-smoke --workspace workspace --team dev-team \
  --category refactor --case api-compat::happy::"Refactor the API safely"

# 2. 擷取正式 team 的 immutable baseline
hufu improve experiment snapshot baseline-017 --workspace workspace --team dev-team

# 3. 以獨立 candidate directory 與 review patch 建立候選快照
hufu improve experiment candidate candidate-023 --workspace workspace \
  --baseline baseline-017 --from .agent-teams/dev-team-candidate --patch candidate.patch

# 4. 在相同 benchmark / model / budget 下執行兩組 team，分別保存 hufu improve JSON report
# 5. 比較並產生 review-only experiment report
hufu improve experiment compare E-014 --workspace workspace \
  --baseline baseline-017 --candidate candidate-023 --benchmark refactor-smoke \
  --baseline-report baseline.json --candidate-report candidate.json \
  --baseline-accepted=true --candidate-accepted=true
```

## Phase 3：Promotion、Adoption 與 Production Monitoring

### Pull Request Promotion

只有 `status: passed` 且 `decision: eligible_for_review` 的 experiment 可以建立 PR。

```bash
# 先檢閱會執行的 repository / branch / patch；不會呼叫 Git 或 GitHub
hufu improve experiment pr E-014 --workspace workspace --repo . --base main --dry-run

# 建立 hufu/improve/E-014 branch、套用 patch、commit、push，最後 gh pr create
hufu improve experiment pr E-014 --workspace workspace --repo . --base main
```

PR promotion 的前提是乾淨 worktree。它只會執行 `git switch -c`、`git apply`、`git add`、`git commit`、非 force 的 `git push --set-upstream` 與 `gh pr create`。任何失敗都會保留已建立 branch/commit 供人類檢查；它**永不執行 merge、reset、force push 或自動合併**。

```json
{
  "version": 1,
  "experiment_id": "E-014",
  "pull_request_url": "https://github.com/org/repo/pull/42",
  "branch": "hufu/improve/E-014",
  "base_branch": "main",
  "candidate_snapshot_id": "candidate-023",
  "candidate_revision": "candidate definition hash",
  "baseline_revision": "baseline definition hash"
}
```

### Adoption 與 Knowledge Entry

PR 已建立或合併不等於 adoption。人類確認 rollout 後才建立 adoption，並自動追加一條 metadata-only knowledge entry：

```bash
hufu improve experiment adopt adopt-014 E-014 --workspace workspace \
  --pr-url https://github.com/org/repo/pull/42 \
  --change-summary "Require focused verification before reporting completion" \
  --condition task-type:refactor
```

每個 entry 包含 `issue_type`、`effective_change`、`conditions`、experiment/adoption IDs、baseline/candidate revisions 與 `outcome: adopted`。可用下列指令查詢：

```bash
hufu improve knowledge list --workspace workspace --issue-type refactor
```

### Production Monitoring 與 Rollback Suggestion

```bash
hufu improve monitor adopt-014 --workspace workspace --runs 10 \
  --acceptance-passed=true --safety-violations=0
```

monitor 會將 production telemetry 和 adoption baseline 比較：candidate revision、acceptance、安全違規、完成率及 error 數。`--acceptance-passed` 必須明確提供；安全違規數可由執行環境的安全監控提供。當 safety/acceptance 失敗、完成率下降或 error 增加時，會寫出監測報告與 rollback suggestion，指向 immutable baseline snapshot/revision。**它不會執行 rollback 指令或更動 repository。**
