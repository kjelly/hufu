---
name: issue-to-pr
description: 將 kjelly/hufu 的單一 GitHub Issue 從根因分析、實作與獨立審查做到 Draft PR，通過 Hufu 驗證及該版本的 CI 後轉 Ready。用於指定 Issue 的修復或實作。已有 Draft PR 時用 pr-ready。
---

# Hufu Issue → PR

先讀共用流程 [`.agents/skills/hufu-pr-workflow/SKILL.md`](../hufu-pr-workflow/SKILL.md)。請求執行本流程時，操作範圍為指定 Issue 的實作、commit、正常 push、建立及更新一個 PR，以及符合條件時轉 Ready。使用者的唯讀、禁止 push 或保留 Draft 等限制優先。

## 選定 Issue 與候選分支

1. 核實 Issue 開啟、需求及討論、關聯 PR、相依 Issue 和 base。查 GitHub 的關聯、PR 搜尋結果及候選分支，排除正在處理的同一範圍。已有 PR 時回報連結，既有 Draft 的修復交由 `pr-ready`。相依項先查證，只處理此 Issue。尚未合併的前置變更若阻擋修復，回報其連結與影響。
2. 以 live 目標分支（通常為 `main`）的 SHA 建隔離 worktree。新分支用 `issue/<number>`，使重跑能找到同一個 head。若同名遠端分支已存在，先查明歸屬與是否有未完成工作，再決定續跑或等待。
3. 驗證 Issue 的觸發條件、根因假設、支援範圍與可觀察的驗收條件。查受影響呼叫端及 Git 歷史，決定是否需要回歸測試與外部驗收。無法重現或範圍有歧義時保留證據，避免把診斷結果當作已完成修復。

## 實作與交付

1. 單一 writer 在隔離分支實作，涵蓋根因及相應回歸。依共用流程驗證候選 commit，安排獨立 reviewer 審查完整 diff 與驗收證據。修正證實的阻擋項後，針對新 commit 重驗並更新審查。
2. push 前重新查 Issue、現有 PR、遠端分支與 base tip。範圍、base 或 head 移動時重新驗證，再做正常 push。建立 PR 前再查該 head/base 是否已有 PR，避免重跑或併行操作重複建立。命令逾時後先查遠端實際狀態。
3. 明確建立 **Draft PR**（例如 `gh pr create --repo kjelly/hufu --draft`，明確給定 base／head）。PR 說明包含 Issue 連結（完整交付時用 `Closes #N`）、根因或需求、改動、驗證和限制，以及適用的 fix evidence。讀回 PR 確認編號、draft、head 與 base。
4. 讀取專案的 `.agents/skills/pr-ready/SKILL.md`，依其驗證及 Ready gate 完成同一個 Draft。CI 失敗時查看原始 job 診斷並修正範圍內的問題。若必要驗證、獨立審查或權限尚缺，保留 Draft 並回報阻礙。轉 Ready 後讀回狀態及 SHA。這個流程在同一個 PR 停止，合併由 `pr-review-merge` 處理。
