# Runtime 任務完成能力：已完成修正與驗證

> Status: completed
> Authority: historical
> Verified-Commit: —（隨本修正提交；基底 `032e10b354b1bb7d9bb8249c6ee37f9a632b7f31`）
> Supersedes: —
> Superseded-By: —

2026-11-12 依既有 Hufu 設定，以合成資料進行 runtime 實測，完成以下四項通用缺陷修正。使用 Go 1.27.2、Ollama 與 `deepseek-v4.1-flash:cloud`；runtime control decisions 設為 `active`。本紀錄只收錄完成的修正與已執行驗證，實測結論限於下述工作負載。

## 已交付修正

### 首次結果收集的診斷

觸發條件是第一次正常結果收集、尚無遭拒回應。`collectionPrompt` 原本無條件將空 diagnostic 送入 `schemaRepairDiagnostic`，產生「runtime 未保存驗證訊息」的虛構格式錯誤。

現在首次收集省略 `format_diagnostic`；已有 diagnostic、response 或 repair issue 時，仍提供精確修復訊息或既有 fallback。不改結果、語意判斷、receipt 或 acceptance。

回歸：`internal/team/result_collection_response_test.go` 的 `TestResultCollectionRepairsResponseEnvelopeWithOriginalArguments`，encoded_claims／array／null 三例在修正前均因首次請求帶有虛構診斷而失敗；修正後通過，並同時檢查後續 response-envelope 修復回饋。

### 明確授權的 runtime 記憶寫入

明確 opt-in 的 `memory_save` 先遭中央 artifact policy 當作外部工具拒絕；補上具體型別 capability 後，實測再確認第二層錯誤：`InferSideEffectClass` 將記憶 mutation 工具推導為 `none`，使 mutation gate 拒絕執行。

現在 `memorySaveLTMWrapper`、`stmWriteTool`、`ltmUpdateTool` 具有 package-private unbound capability，並保守推導為 `workspace_write`。這些 handler 只操作 runtime-scoped canonical state，`file_paths` 是 metadata，不作為讀取來源。既有 opt-in grants、private identity、candidate confirmation、明示 `none` 與 bound-workset 拒絕仍維持。

回歸：`TestLegacyMemoryMutationArtifactPolicy` 的三種 handler 在修正前 unbound 均遭拒，修正後 unbound 可用而 bound 仍拒絕；`TestInferSideEffectClass` 的三個工具由錯誤的 `none` 改為 `workspace_write`。測試分別位於 `internal/team/worker_memory_query_test.go` 與 `internal/team/recovery_classification_test.go`。

### Unicode lexical 查詢

既存 unicode61 索引包含「青鷺」，但關閉 embedding 的精確名稱查詢回傳空結果；同資料查詢 `QL-7391` 或 `UTC` 可命中。原因是 `ftsQuery` 只保留 ASCII，刪除了中文、阿拉伯文、西里爾字，並破壞重音字。

現在保留 Unicode 字母、數字與 combining marks，維持 token quoting、去重及既有 operator 處理。此修正保留完整 token，不新增斷詞或自然語言意圖解析，也不改 scope、allow-set、lifecycle 或來源授權。

回歸：`internal/context/lexical_query_test.go` 的 `TestSearchLexicalPreservesUnicodeTerms` 使用真實 SQLite，涵蓋中文、組合／分解重音、阿拉伯文、西里爾字、operator、lexical／supplemental 搜尋及跨 team 隔離。Unicode 案例在修正前失敗，修正後通過。

### Selection preflight 與正式政策隔離

全新 default workspace、不加 `--new`，在 worker 開始前即因 max-concurrent、model routes 等 policy fingerprint 漂移而失敗。原因是 selection 輔助 coordinator 先將自己的 policy snapshot 寫入正式 workspace 的 `runtime.sqlite`。

現在每次 selection 在選定 workspace root 下使用獨立私有 `.selection-*` control directory，保留輔助審計，與正式 team policy 分離。不同 selection model 或並行 selection 不共用該 control directory；dry-run 不建立 selection invocation 或目標 workspace。正式規範同步於 [execution runtime](../../architecture/execution-runtime.md)。

回歸：`cmd/hufu/autoteam_test.go` 的 `TestSelectionPreflightDoesNotFreezeTargetWorkspace`，run／dry-run 在修正前分別因污染正式 store、建立 selection invocation 而失敗；修正後通過。另驗證 audit store 在關閉後保留，後續改用不同輔助模型不改變正式 frozen policy。

## 已執行的實機驗證

診斷使用公開 status、inspect、context、results 介面；既有 receipt、事件、policy 與被拒絕的 run 保留原樣。

| 工作負載 | Run／結果 |
| --- | --- |
| 合成 artifact 交付 | `run-20261112T094005.770340344Z-c3e822642f04`：completed、acceptance passed、1/1 done、0 unresolved；獨立檔案檢查為42。專案檔案被誤列為 workspace artifact 的負向 probe 仍正確遭 source_invalid 拒絕。 |
| 私人持久記憶寫入 | `run-20261112T095528.885518650Z-c9a44e7ee071`：completed、acceptance passed、2/2 done；公開 metadata 確認兩筆 private persistent 記憶皆為 confirmed。 |
| 新 session 記憶召回 | `run-20261112T095915.712996432Z-049ff7d6f779`：未在 prompt 提供答案，仍正確回答 QL-7391 與星期三02:40 UTC；completed、acceptance passed。 |
| 同索引 Unicode 查詢 | semantic-mode off、不重建索引，合法 worker 查「青鷺」由0變為命中2筆目標 private IDs；outsider 命中這些 IDs 為0。 |
| 有界結果階段修復 | `run-20261112T100952.556720139Z-e0eb8a85126c` 的 worker 單次計算23+38；proxy 對前兩次 collector 注入非法 envelope，原 run blocked。新程序 result-only repair `run-20261112T101114.292312439Z-e6eb70a2a6d1` 交付61、acceptance passed。原 math 1次、resume 0次，55個原公開 event refs 的 hash 全數保留；collector 共3次，proxy 事後關閉。 |
| 新 default workspace 啟動 | `run-20261112T112923.637403143Z-d5c0d6cef30a`：無 `--new`，1/1 done、0 unresolved、無 snapshot drift；default team 沒有 acceptance，整體如實為 unverified／acceptance_not_configured。另一次完整 CLI dry-run exit0，目標目錄不存在。 |

另外完成以下有界評估：

- 語意檢索：既定兩筆私人記憶、30組中英文查詢，off 命中0/30、active命中30/30，60個命令均exit0；其他worker暴露目標private IDs為0。這是兩筆相近記錄上的召回證據。
- 固定資料 Context A/B：79個檔案的初始hash一致，legacy／active各一次新session均答對、completed、acceptance passed、fallback0。這是單次成對觀察。
- 歷史查詢：30題合成正向改寫均operation succeeded，預期值與引用ID符合返回資料，回答逐題閱讀，額外數字與hash對回canonical typed result。結論限定於此正向題集。

上述為 bounded probes：受控格式故障證明該結果修復路徑保存工作；小樣本檢索與A/B結果只支持其列出的題集與次數。

## 最終來源驗證

四項修正完成後核對全部變更來源 hash，依序執行以下命令，均exit0；過程未與live team並行執行：

| 命令 | 結果 |
| --- | --- |
| `mise run test-contracts` | 通過；team 85.651s、CLI 36.957s，inspect／operator／auditverify／reviewgate／reviewprep／web-research通過。 |
| `mise run test` | 全部通過；team 711.953s、CLI 129.303s、context 22.203s。 |
| `mise run vet` | 通過，無診斷。 |
| `mise run lint` | 通過，0 issues。 |
| `git diff --check` | 通過。 |

保留原 GOCACHE、GOTMPDIR、GOMODCACHE。依新增回歸字串與checkout compile-unit路徑辨識，排除現用exports及open／mapped／hard-linked項目後，清理6個過期team test archives，共902,627,520 bytes；未清除共享依賴。最終沒有遺留驗證或probe程序，full-test工作目錄已釋放。

## Structured fix evidence

```hufu-fix-evidence
{
  "kind": "bugfix",
  "acceptance": "首次collector省略虛構格式診斷；已授權的unbound runtime記憶mutation可執行且bound仍拒絕；Unicode完整token可命中既存索引並保持scope隔離；selection不污染正式policy，fresh default可啟動且dry-run無workspace副作用。",
  "cause_status": "confirmed",
  "cause": "空診斷無條件fallback；記憶wrapper缺具體capability與side-effect分類；FTS query刪除非ASCII字元；selection輔助coordinator共用正式store。",
  "cause_evidence": [
    "docs/archive/implementation-plans/runtime-task-completion-hardening.md#已交付修正"
  ],
  "regressions": [
    {"path": "internal/team/result_collection_response_test.go", "scenario": "首次collector及後續response-envelope修復", "before": "encoded_claims、array、null三例因首次虛構診斷失敗", "after": "focused、contracts及full通過"},
    {"path": "internal/team/worker_memory_query_test.go", "scenario": "三個具體mutation wrapper的unbound與bound政策", "before": "三種unbound wrapper遭拒", "after": "unbound允許、bound拒絕，focused及full通過"},
    {"path": "internal/team/recovery_classification_test.go", "scenario": "三種記憶mutation的side-effect推導", "before": "推導none", "after": "推導workspace_write，focused及full通過"},
    {"path": "internal/context/lexical_query_test.go", "scenario": "Unicode lexical與supplemental命中及scope隔離", "before": "中文、重音、阿拉伯文、西里爾字案例失敗", "after": "context及full通過"},
    {"path": "cmd/hufu/autoteam_test.go", "scenario": "selection與正式policy隔離、dry-run無目錄", "before": "run污染正式store、dry-run建立invocation", "after": "focused及full通過"}
  ],
  "checks": [
    {"command": "mise run test-contracts", "result": "passed", "evidence": "docs/archive/implementation-plans/runtime-task-completion-hardening.md#最終來源驗證"},
    {"command": "mise run test", "result": "passed", "evidence": "docs/archive/implementation-plans/runtime-task-completion-hardening.md#最終來源驗證"},
    {"command": "mise run vet", "result": "passed", "evidence": "docs/archive/implementation-plans/runtime-task-completion-hardening.md#最終來源驗證"},
    {"command": "mise run lint", "result": "passed", "evidence": "docs/archive/implementation-plans/runtime-task-completion-hardening.md#最終來源驗證"}
  ],
  "workflow": {
    "level": "bounded_probe",
    "claim": "source_fix",
    "scope": "基底032e10b354b1bb7d9bb8249c6ee37f9a632b7f31加本修正，Go1.27.2，Ollama/cloud模型與合成artifact、private記憶、正向歷史題集、受控collector格式故障及fresh default啟動。",
    "evidence": ["docs/archive/implementation-plans/runtime-task-completion-hardening.md#已執行的實機驗證"],
    "limitations": "結論限於列出的有界工作負載：受控格式故障修復、小型正向題集及單次成對A/B。default team無acceptance，保持unverified。"
  }
}
```
