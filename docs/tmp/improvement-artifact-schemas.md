# Agent Team Improvement Artifact Schemas（Phase 1）

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

## Hypothesis（人工建立，Phase 2 前不自動套用）

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

## Experiment Report（Phase 2 reserved）

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
decision: pending # adopt | retry | reject | rollback
rollback_revision: sha256-definition-hash
```

此 schema 預先固定 Phase 2 需要的審計欄位，但 Phase 1 不會建立 candidate、執行 benchmark 或修改正式 team。
