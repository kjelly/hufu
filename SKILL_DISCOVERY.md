# 自動技能發現系統

## 功能概述

hufu 現在可以**自動偵測重複執行的動作模式**，並將這些模式自動儲存為技能草稿。

## 工作原理

### 三層模式偵測架構

```
┌─────────────────────────────────────────┐
│  Layer 1: Tool Name Sequence (Fast)     │
│  - Sliding window (3-10 tools)          │
│  - Hash-based frequency counting        │
│  - Filter: count >= 5                   │
└─────────────────┬───────────────────────┘
                  │ (candidates)
                  ▼
┌─────────────────────────────────────────┐
│  Layer 2: Parameter Pattern Matching    │
│  - Extract file patterns (*.go, *.ts)   │
│  - Command templates (git *, npm *)     │
│  - Normalize arguments to placeholders  │
└─────────────────┬───────────────────────┘
                  │ (refined candidates)
                  ▼
┌─────────────────────────────────────────┐
│  Layer 3: Semantic Similarity (Sidecar) │
│  - ALWAYS ENABLED when sidecar exists   │
│  - LLM clusters task descriptions       │
│  - Merges similar sequences (≥0.9)      │
│  - 5-second timeout with fallback       │
│  - Cached results for performance       │
└─────────────────────────────────────────┘
```

### 語意相似度分析

**自動啟用**：當 coordinator 初始化時，會自動將 sidecar 實例傳遞給 `SkillPatternDetector`。

**工作流程**：
1. 收集所有候選模式的 task descriptions
2. 調用 sidecar LLM 進行聚類分析
3. 將工具序列相同且語意相似的序列合併（相似度 ≥ 0.9）
4. 生成合併後的技能描述

**範例**：
```
啟用前（僅工具名稱匹配）：
  1. [view → edit → bash] ×5  (Go code editing)
  2. [view → edit → bash] ×3  (TypeScript editing)

啟用後（語意合併）：
  1. [view → edit → bash] ×8  (Code modification workflow)
     - Merged 2 similar patterns (similarity: 0.92)
     - Common intent: "Modify source code files"
```

**性能優化**：
- **批次處理**：一次性分析所有 descriptions
- **結果緩存**：相同的 description 組合使用緩存
- **超時控制**：5 秒超時，失敗時降級到純工具名稱匹配
- **無 sidecar 時優雅降級**：系統正常工作，僅缺少語意分析

## 使用方式

### 1. 即時通知

當 coordinator 完成一輪任務委派後，系統會自動檢查是否有重複的模式：

```
─── SKILL SUGGESTIONS ───
Detected 2 new repeating pattern(s):
  1. [view → edit → bash] ×5 - Use when modifying Go code
  2. [grep → view → edit] ×5 - Use when fixing bugs

Draft skills saved to:
  - workspace/skills/draft-view-edit-bash/SKILL.md
  - workspace/skills/draft-grep-view-edit/SKILL.md

Review and refine with: hufu skill review <skill-name>
```

### 2. 查看技能草稿

```bash
# 列出所有可用的技能草稿
hufu skill list

# 查看特定技能草稿
hufu skill review draft-view-edit-bash
```

### 3. 編輯和完善技能

技能草稿保存在 `workspace/skills/draft-<name>/SKILL.md`，可以直接編輯：

```bash
# 用編輯器打開
vim workspace/skills/draft-view-edit-bash/SKILL.md
```

### 4. 使用技能

一旦技能文件存在，就可以通過 `load_skill` 工具使用：

```markdown
@coordinator Load the skill "draft-view-edit-bash" and use it to modify the codebase.
```

## 配置參數

### 最小重複次數 (minFrequency)

預設：**5 次**

只有當一個模式重複執行至少 5 次時，才會被建議為技能。

### 窗口大小範圍 (windowMin, windowMax)

預設：**3-10 個工具**

系統會分析連續 3 到 10 個工具調用的序列。

### 語意相似度閾值

預設：**0.9**

只有相似度 ≥ 0.9 的序列才會被合併。這是一個嚴格的閾值，確保僅合併非常相似的工作流程。

### Sidecar 超時

預設：**5 秒**

語意分析調用有 5 秒超時限制，超時後自動降級到純工具名稱匹配。

## 技能草稿格式

自動生成的技能草稿包含以下內容：

```markdown
---
name: draft-view-edit-bash
description: Use when modifying Go code (detected from 5 similar executions)
---

# View Edit Bash

## Overview

Auto-generated skill from **5** similar executions.

**First seen:** 2026-05-21 14:30
**Last seen:** 2026-05-21 15:45

## Workflow

This skill automates the following tool sequence:

1. **view** - `*.go`
2. **edit** - `*.go`
3. **bash** - `go build`

## Example Execution

Observed pattern:

```bash
# Step 1: view
view *.go
# Step 2: edit
edit *.go
# Step 3: bash
bash go build
```

## Common Use Cases

This skill was used in the following contexts:

- modifying Go code (×3)
- fixing compilation errors (×2)

## Notes

**This skill was auto-generated.** Please review and refine:

- Verify the tool sequence is correct
- Add error handling if needed
- Improve parameter patterns
- Add edge cases and gotchas

## Implementation Template

```go
// Template for generating similar skills
func ApplyWorkflow(ctx context.Context, params map[string]string) error {
	// Step 1: view
	// Step 2: edit
	// Step 3: bash
	return nil
}
```
```

## 內部實現

### SkillPatternDetector

位於 `internal/skill/discovery.go`：

- `RecordToolCall()` - 記錄每次工具調用
- `FindCandidates()` - 查找重複模式
- `GetSequencesByAgent()` - 按 agent 獲取序列

### AutoSkillGenerator

位於 `internal/skill/generator.go`：

- `GenerateSkill()` - 從模式生成 SKILL.md
- `buildSkillContent()` - 構建技能內容

### Coordinator 整合

- 在 `OnToolCall` 回調中記錄工具調用
- 在 `ExecuteTasks()` 結束時檢查模式
- 自動生成技能草稿到 `workspace/skills/`

## 測試

```bash
# 運行技能發現系統的單元測試
go test ./internal/skill/... -v

# 運行所有測試
go test ./...
```

## 限制和注意事項

1. **語意相似度分析** - 目前尚未啟用 sidecar LLM 分析，僅使用工具名稱和參數模式匹配。

2. **報告整合** - 技能發現報告章節目前返回空數據，因為 `skillDetector` 未導出。主要依賴即時通知。

3. **團隊級別儲存** - 技能草稿儲存在團隊的 `workspace/skills/` 目錄，不是全局共享。

4. **性能影響** - 模式偵測使用滑動窗口算法，對於大量工具調用可能有性能開銷。

## 未來改進

- [ ] 啟用 sidecar LLM 進行語意相似度分析
- [ ] 添加 CLI 命令將草稿轉換為正式技能
- [ ] 支持技能模式的可視化展示
- [ ] 添加技能使用效果追蹤
- [ ] 支持技能的版本控制和迭代
