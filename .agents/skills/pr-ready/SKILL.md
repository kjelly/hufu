---
name: pr-ready
description: 修正 kjelly/hufu 的單一既有 Draft PR，處理已證實的 review、回歸及 CI 問題。獨立審查並驗證同一版本後轉 Ready for Review。用於修到可以送審或取消 draft，合併交由 pr-review-merge。
---

# Hufu Draft PR → Ready

先讀共用流程 [`.agents/skills/hufu-pr-workflow/SKILL.md`](../hufu-pr-workflow/SKILL.md)。請求執行本流程時，操作範圍為指定 Draft 的修正、commit、正常 push、更新 PR 說明及轉 Ready。使用者的唯讀、禁止 push 或保留 Draft 等限制優先。

## 修復與重驗

先查[歷史回歸注意事項](references/merge-cautions.md)，按本 PR 變更選用。

1. 核實 PR 仍是 open Draft 且 head 可寫。已 Ready、closed 或 merged 則回報現況。讀完整 diff、Issue、reviews、所有未解決 threads 和 CI。對每個 review 意見記錄證實／反證／缺少證據、觸發、根因、修正及回歸。不同意時保留具體反證供審查者確認。
2. 從核實的 PR head SHA 建隔離 worktree。若有 conflict、PR 與 live base 修改同一檔需驗證整合，或分支規則要求更新，正常合併 live base，重新核對整體 diff 與回歸，再驗證合併結果。fork 同樣只寫回核實的 head。修正根因，不跳過測試或放寬驗收。
3. 將修正凍結成候選 commit，針對完整 diff 執行 Hufu 驗證與獨立審查，記錄候選 head 與 live base。正常 push 後讀回 SHA，更新 PR 說明中的行為、證據、已驗證的相關歷史回歸與限制。只有具修復證據且獲授權時才處理相應 review thread。未解除的 changes requested 交由原 reviewer 重審。
4. 等待該版本 CI。讀取失敗 job 診斷、修正、重新驗證及審查受影響的結果。同一失敗反覆出現時回頭查根因與 fixture／環境差異。缺少新證據時停止重跑，回報阻礙。

## Ready gate

- 仍是同一個 open Draft。獨立 review 與必要本機驗證對應當前 head／live base，完整涵蓋驗收，沒有已確認的阻擋缺陷或未解決實質 thread。單純等待 Ready 後才適用的人工核准可進入 review，已要求的實質修正需先處理。若規則另有明確的 Ready 前置條件則依規則。
- 當前候選的 `ci-success`、適用的必要 jobs 與 branches 規則要求的 checks 結果通過，無未處理 conflict。CI 缺失、未知、失敗或尚未結束時保留 Draft。若必要 CI 只能在 Ready 後執行，回報具體依賴。
- 操作前重新讀取 head、live base、Draft 狀態、reviews／threads、checks 與 rules。符合條件才用對應 connector 或 `gh pr ready <number> --repo kjelly/hufu`。讀回證實同一個 open PR、`isDraft=false`、head/base 一致。若轉 Ready 觸發新 checks，等結果並在回報說明。發現新的確認阻擋項且獲授權時，依同樣的遠端核對方式轉回 Draft 再處理。
- 回報 `READY`、`DRAFT-BLOCKED` 或實際遠端狀態、候選 SHA、checks 與阻礙。此步驟結束於 Ready，人工核准與合併由下一階段判定。
