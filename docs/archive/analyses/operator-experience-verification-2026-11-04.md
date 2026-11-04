# Operator 修復評估契約局部驗證 — 2026-11-04

> Status: historical
> Authority: reference
> Verified-Commit: `3130bac0557ce31b096cc1b4a3dd9f3cb750a4cd`
> Supersedes: —
> Superseded-By: —

## 範圍與歷史基準

本紀錄支持 [Operator UX §6.2](../../architecture/operator-experience.md#section-6) 的
`assess-result-repair` 文件澄清，僅涵蓋下列程式碼核對及既有測試。
原 2026-09-14 基準 `ba30643754d8d6cd85f91624a3f9bedf9d32457a` 及其 phase gate
結果保留為歷史證據；原 header 的 2026-10-30 日期沒有在該文件中附上新的 revision、
命令與結果，本紀錄不將該日期當成另一次已證明的通過。

本次對照原始碼確認：assessment 必須符合 verified scope、valid/degraded integrity、
consistent projection、verified event chain 與非 wait primary；候選由
`HasCollectionRecovery` 判定為 blocked collection 或 error 狀態的 interrupted collection。
這些是唯讀建議的產生條件，實際修復仍由 runtime 另行檢查。

完整 runtime／CLI 恢復、跨平台、真人 usability 與其餘 phase gate 的驗證範圍不因
本次局部測試而擴大。沒有呼叫真實模型或對現有 team session 執行恢復。

## 執行身分

- 程式碼 revision：`3130bac0557ce31b096cc1b4a3dd9f3cb750a4cd`。
- 開始（UTC）：`2026-11-04T11:20:36.182738+00:00`。
- 結束（UTC）：`2026-11-04T11:20:39.640911+00:00`。
- Go：`go version go1.27.2 linux/amd64`。
- 原生平台：`Linux x86_64`。
- 執行位置：repository root；沿用 mise 固定工具鏈、既有 build/module caches 與 GOTMPDIR。
- 測試前後確認 internal、cmd、go.mod、go.sum、.mise.toml 相對該 revision 沒有修改；
  下列來源檔另外逐一與 Git blob 比對。文件更正與本紀錄是此 revision 之後的文件變更。

## 命令與結果

實際測試由 subprocess argv 執行；以下為保留相同參數及暫存隔離的 shell 寫法：

```bash
task_tmp=$(mktemp -d /tmp/codex-task.XXXXXX)
trap 'rm -rf -- "$task_tmp"' EXIT
export TMPDIR="$task_tmp" GOMAXPROCS=2
mise exec -- go test -p 2 -timeout 5m ./internal/operator ./internal/inspect -run '^(TestSelectActions(OffersOnlyReadOnlyResultRepairAssessment|AssessesEveryCollectionWithoutChangingSafetyPriority|SuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable)|TestRecoveryEligibility(OffersAssessmentForCancelledCollection|RecognizesBlockedCollection))$' -count=1 -v
```

- Exit code：`0`。
- 合併 stdout/stderr 的 SHA-256：`dfbcd2c958192ad29952ca763f0f2302ff1eb1c755baff8127a04e52c95d3ed9`。
- `-count=1` 強制重新執行測試；編譯仍可使用既有快取。

| 既有測試 | 核對行為 |
|---|---|
| `TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment` | 唯讀 argv、blocked／cancelled 候選、完整 target 與 snapshot 契約 |
| `TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority` | 逐任務評估、去重與排序，保留 unknown external effect 的 primary 優先順序 |
| `TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable` | 不支援或不唯一的 binding、不可用 integrity、projection drift 與 runtime wait 的排除案例 |
| `TestRecoveryEligibilityOffersAssessmentForCancelledCollection` | 已封存成功工作的中斷 collection 與失敗／未完成／歷史 receipt 的區別 |
| `TestRecoveryEligibilityRecognizesBlockedCollection` | protocol collection failure、checkpoint 與一般提交／執行失敗的區別 |

## 原始輸出

```text
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/collection_with_denied_replay
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/cancelled_collection
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/ordinary_protocol_failure
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/invalid_evidence
=== RUN   TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/ambiguous_scope
--- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment (0.00s)
    --- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/collection_with_denied_replay (0.00s)
    --- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/cancelled_collection (0.00s)
    --- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/ordinary_protocol_failure (0.00s)
    --- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/invalid_evidence (0.00s)
    --- PASS: TestSelectActionsOffersOnlyReadOnlyResultRepairAssessment/ambiguous_scope (0.00s)
=== RUN   TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority
=== RUN   TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority/unknown_external_effect
=== RUN   TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority/collection_selected
--- PASS: TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority (0.00s)
    --- PASS: TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority/unknown_external_effect (0.00s)
    --- PASS: TestSelectActionsAssessesEveryCollectionWithoutChangingSafetyPriority/collection_selected (0.00s)
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/ambiguous
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unknown_binding
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unsupported_binding
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/invalid_integrity
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unknown_integrity
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/projection_drift
=== RUN   TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/runtime_active
--- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/ambiguous (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unknown_binding (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unsupported_binding (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/invalid_integrity (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/unknown_integrity (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/projection_drift (0.00s)
    --- PASS: TestSelectActionsSuppressesCollectionAssessmentsWhenScopeOrIntegrityIsUnavailable/runtime_active (0.00s)
PASS
ok  	github.com/kjelly/hufu/internal/operator	0.007s
=== RUN   TestRecoveryEligibilityOffersAssessmentForCancelledCollection
=== RUN   TestRecoveryEligibilityOffersAssessmentForCancelledCollection/saved_work
=== RUN   TestRecoveryEligibilityOffersAssessmentForCancelledCollection/failed_work
=== RUN   TestRecoveryEligibilityOffersAssessmentForCancelledCollection/unfinished_work
=== RUN   TestRecoveryEligibilityOffersAssessmentForCancelledCollection/historical_receipt
--- PASS: TestRecoveryEligibilityOffersAssessmentForCancelledCollection (0.00s)
    --- PASS: TestRecoveryEligibilityOffersAssessmentForCancelledCollection/saved_work (0.00s)
    --- PASS: TestRecoveryEligibilityOffersAssessmentForCancelledCollection/failed_work (0.00s)
    --- PASS: TestRecoveryEligibilityOffersAssessmentForCancelledCollection/unfinished_work (0.00s)
    --- PASS: TestRecoveryEligibilityOffersAssessmentForCancelledCollection/historical_receipt (0.00s)
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection/collection
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection/missing_checkpoint
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection/tool_submission
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection/execution_failure
=== RUN   TestRecoveryEligibilityRecognizesBlockedCollection/completed
--- PASS: TestRecoveryEligibilityRecognizesBlockedCollection (0.00s)
    --- PASS: TestRecoveryEligibilityRecognizesBlockedCollection/collection (0.00s)
    --- PASS: TestRecoveryEligibilityRecognizesBlockedCollection/missing_checkpoint (0.00s)
    --- PASS: TestRecoveryEligibilityRecognizesBlockedCollection/tool_submission (0.00s)
    --- PASS: TestRecoveryEligibilityRecognizesBlockedCollection/execution_failure (0.00s)
    --- PASS: TestRecoveryEligibilityRecognizesBlockedCollection/completed (0.00s)
PASS
ok  	github.com/kjelly/hufu/internal/inspect	0.030s
```

## 來源檔身分

| 來源檔 | SHA-256 |
|---|---|
| [internal/operator/actions.go](../../../internal/operator/actions.go) | `f8e20736c8ae8327b15f8426ced31e019231a52e0f3fa2ad8bc279a916b37ebd` |
| [internal/operator/actions_test.go](../../../internal/operator/actions_test.go) | `5445fab8fa0d15ed5971c355cce47d75a1ee84e953fbc7fcd8576695efc7984e` |
| [internal/operator/types.go](../../../internal/operator/types.go) | `fd5c618a9aece41786b6037e07cf24fd53569f1f1e94e3b26346101c312af3f2` |
| [internal/operator/snapshot.go](../../../internal/operator/snapshot.go) | `7b840f3b0e8b19356a22bbc32fe69e3bb5f7e0df4533a31e3e0db650ab8135e8` |
| [internal/inspect/recovery.go](../../../internal/inspect/recovery.go) | `dc8d1d3d9311efba42cce22111998a4d80c75967edb226ae7b799ea07095b9dd` |
| [internal/inspect/recovery_test.go](../../../internal/inspect/recovery_test.go) | `d96b393038f9819677a083e887190e06f8bbdf7d087d81a7cc5120f11f5e0c7b` |
| [go.mod](../../../go.mod) | `9abc675a08afe1625b393fe93c866c0f99bdf034ed0be8bd1bac5cfc8d3b79f2` |
| [go.sum](../../../go.sum) | `bb92505b74744713c6d69aec94b47b53aaad3dc22f2e945c670f798c33a1a317` |
| [.mise.toml](../../../.mise.toml) | `5abb543e6f8ccb6b0efbf0ef46d519c4180c26d6a956f2290ac75088d5ea5391` |
