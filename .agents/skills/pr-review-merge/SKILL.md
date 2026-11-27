---
name: pr-review-merge
description: 獨立審查 kjelly/hufu 的單一 open Ready PR，對核實的 head、base、Hufu CI 與分支規則做合併判定。通過才合併，證實需修復時轉 Draft，待證據或人工審查時等待。一般唯讀 review 請用 code-reviewer。
---

# Hufu Ready PR → Merge

先讀[共用流程](../_shared/hufu-pr-workflow.md)。使用者要求執行本流程時，操作範圍為指定 PR 的審查結論留言、合併或轉 Draft。唯讀或禁止合併的要求優先。審查使用隔離快照；需要修改 PR 分支時交由 `pr-ready`。

## 審查與整合

先查[審查注意事項與歷史來源](references/review-cautions.md)，按本 PR 變更選用。

1. 核實 PR 已 open 且 Ready，記錄 head／live base、相依 PR、CI 與 mergeability。Draft、closed 或 merged 回報 `SKIPPED`。確認 live base 與 head 的 ancestry。若 head 落後，檢查 merge-base 後雙方改動是否有檔案重疊、GitHub 測試合併是否乾淨，並核對是否必須 up-to-date。若可直接合併，審查須涵蓋與現行 base 行為的交互。
2. 閱讀完整 diff、需求、既有 reviews／threads、反證與目前 CI，安排獨立 reviewer。自身也核對重大 findings、覆蓋率與 Hufu 專案規範。依共用流程查歷史回歸、逐 commit 敏感資料、目前版本驗證與整合證據。測試成功與審查通過分開記錄。必要資訊不足時記錄缺口。
3. 核對所有必要人工核准、CODEOWNERS、changes requested、review threads、active rules、mergeability 和 applicable CI。若 GitHub 版本改變，重跑相關審查及驗證。現行 protections 與 rulesets 由 GitHub 判定，不能因本機判定通過而略過。

## 判定與執行

| 結果 | 條件與操作 |
| --- | --- |
| `MERGE` | 完整獨立審查、相關驗證、head／base、CI、人工核准和 branch rules 全數通過。無需 branch repair 或已確認安全整合。執行核對後的合併。 |
| `DRAFT-BLOCKED` | 證實有需修改分支的阻礙，例如行為缺陷、差異造成 CI 失敗、敏感資料、衝突或待整合的重疊改動。確認阻礙仍存在後，留言記錄 SHA、證據、修復和重驗步驟，再轉 Draft。 |
| `WAITING` | CI pending、必要核准或 re-review 待回覆、缺少獨立審查、缺少 CI 或合併證據、未知規則或權限、外部失敗、純相依等待或已排隊。維持現行狀態，說明可繼續的條件。有界追蹤暫態問題。 |
| `SKIPPED` | 已是 Draft、closed 或 merged，回報實際狀態。 |

- 轉 Draft 前重讀 head／live base 與 PR 狀態，核對留言屬於該版本；留言後、轉狀態前再核對。可用 `gh pr ready <number> --repo kjelly/hufu --undo`。重讀確認 open、draft、head 及 base；拒絕或狀態不符時如實回報。敏感資料 finding 僅寫位置與類型。
- 合併前再次重讀 head／live base、checks、reviews／threads、rules、mergeability、draft 與 queue／auto-merge 狀態。普通分支用支援 expected head SHA 的 connector，或 `gh pr merge <number> --repo kjelly/hufu --match-head-commit <reviewed-head-SHA>` 搭配允許的方式。遵循使用者及 repo 規範，無偏好時採允許的 squash；不可使用 `--admin`、`--auto` 或 `--delete-branch`。遇到拒絕或逾時先讀回，不能省略 SHA guard 重試。
- 若 rules 要求 GitHub merge queue，僅使用能明確針對已審查 HEAD 入列且不會隱式啟用 auto-merge 的操作。證實成功才回報 `QUEUED` 並追蹤，工具能力不足則回報 `WAITING`。只有遠端證實已 merged 且可讀取 merge commit 時回報 `MERGED`。
- 完成後停在指定 PR。回報審查及驗證的 SHA、findings、CI 與實際遠端狀態，帶上後續必要條件。
