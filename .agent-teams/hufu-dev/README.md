# Hufu Development Team

hufu-dev 由兩位唯讀 specialist、一位 implementation writer 與一位唯讀 verifier 組成。四個 worker 必須依各自的 result-contract 提交 structured_payload；schema 合法性只保證欄位形狀，不證明命令曾執行。完成仍受 canonical status、獨立 verifier 與 blocking test/vet/lint acceptance 約束。

## Context artifacts：離線 fixture 的範圍

Bundled team.yaml 沒有啟用 context-artifacts。以下是臨時 team copy 可使用的完整測試政策，不是預設配置變更或上線指令：

```yaml
context-artifacts:
  enabled: true
  min-bytes: 51200
  preview-bytes: 8192
  max-artifact-bytes: 1048576
  max-read-bytes: 32768
  max-artifacts-per-attempt: 32
```

大小以完整、遮罩後且包含 exit footer 的 UTF-8 capture bytes 計算。成功發布需要授權的 view、支援的工具及執行路徑、durable journal 與剩餘配額；不會增加任何角色的權限。

| 完整輸出大小 n | 既有行為 |
|---|---|
| n ≤ 8192 | 不 offload；仍可能被既有單行／行數限制截斷 |
| 8192 < n ≤ 51200 | 只有既有輸出會截斷時才 offload |
| 51200 < n ≤ 1048576 | 符合其他條件時 offload，提供 preview 與 opaque reference |
| n > 1048576 或配額用完 | 回退既有有界輸出，沒有完整 artifact reference |

Bash 的既有限制包含 2000 lines、50 KiB 與單行 2000 runes；例如 6000-byte 單行低於 preview 下限，仍會截斷且不保存完整 artifact。不能宣稱啟用後所有命令輸出都可完整取回。

合法 reference 可由 view 按 byte range 讀取，每次最多 32768 bytes，直到完整重建。SHA256 與 byte size 對應完整遮罩內容；不包含遮罩前秘密。Reference 不是一般路徑、task ID 或命令執行證明。

Offload 保留原本的 IsError／exit code。超限、保存失敗或 journal append 失敗時不重跑命令，也不暴露未接受的 reference。無 view、closed tool sequence、asserted result-contains 或不支援的 source 均維持原路徑。缺失、損壞與越權讀取應拒絕，不能改讀別的路徑補造內容。

若必要驗證內容已截斷而不可取得，worker 應回報 unverified／partial／blocked 與 gaps，不能聲稱已讀完整證據。這是 worker 指示；fallback 本身不會自動讓整個 task 失敗，沒有新增 runtime 強制 gate。程序重啟後 attempt-local reference 的可用性沒有本輪恢復保證，未驗證時應標 unavailable。

## 固定測試與限制

只在臨時 fixture 啟用設定；不需要 Ollama、API key、Playwright 或歷史 workspace：

```bash
go test ./internal/team -run '^Test(ContextArtifact|ContextArtifacts|BashOutputOffload|BashSmallLongLineHonorsLegacyFallback)' -count=1
```

[大小邊界回歸](../../internal/team/context_artifact_boundaries_test.go) 覆蓋 preview／min／cap 等於與相鄰值，並使用 production view 重建頭、中、尾及完整 digest。[capture tests](../../internal/team/context_artifact_capture_test.go)、[service tests](../../internal/team/context_artifact_service_test.go) 與 [policy tests](../../internal/team/context_artifacts_policy_test.go) 保留拒絕、配額、發布失敗、來源限制及 snapshot drift 的既有案例。

整合測試 `TestHufuDevWorkflowStructuredHandoffAndArtifacts` 另外以相同 fake-worker 任務切換臨時設定，檢查結構化交接、完整讀回及超限 fallback。此離線覆蓋不代表真實模型品質、成本、延遲或實際 Codex／瀏覽器輸出的相容性已驗證。

回滾 fixture 時恢復舊 team 定義並使用新的臨時 session；既有 session 遇到設定／schema 漂移會拒絕恢復。不要覆寫歷史事件、payload 或 artifact。配置細節見 [agent format](../../docs/reference/agent-format.md)。
