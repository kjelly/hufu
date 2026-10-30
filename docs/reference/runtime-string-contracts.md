# Runtime 字串判斷與契約遷移

> Status: active
> Authority: reference
> Verified-Commit: `2100b1b227f27ecc11790b49b8fe8915920e4f9a`
> Supersedes: —
> Superseded-By: —

Runtime 的控制訊號應來自原生執行資料、型別化錯誤與已驗證的結果契約。
自然語言輸出不應因為特定措辭而被接受或拒絕。

## 本次行為變更

- 工具權限拒絕會記錄 `tool_permission_denied` disposition；guard 拒絕沿用
  `guard_denied`。失敗任務從 attempt receipt 傳遞這項資訊，並在 failure detail
  保存 `permission_blocked`。一般 OS/provider 錯誤提到 permission denied 不代表
  Hufu 已取得人工拒絕。沒有新標記的歷史紀錄仍使用舊判讀方式。
- `source` 必須是完整的 failure header 欄位；`error=` 後面的文字不是 metadata。
- 最終回答只檢查是否為空，不再用「我會」或「Let me」等字首猜測完成狀態。
  原有 `submit_result`、result contract 與 verification 要求仍然適用。
  明確啟用唯讀 free-text handoff 的團隊仍由該設定承擔自由文字接受策略。
- `command_exit` 保留實際退出碼。輸出 `failed -1` 不再覆寫退出碼。
  舊 runner 若以 stdout 表達失敗，應改回傳非零退出碼，或輸出 JSON 檔並以
  `json_assert` 驗證欄位。普通文字的 weak-verifier warning 仍只是警告。
- Coordinator 以 `role: coordinator` 或 `role: orchestrator` 指定；名稱不再提供
  角色推斷。缺少明確角色時沿用內建 coordinator fallback。
- Capability 必須精確匹配宣告 ID；`security` 不會匹配 `security-review`。
  自然語言需求應在派工前解析成經 schema 驗證的 ID。
- Decision replay 的不存在錯誤與未關閉 terminal session 使用 sentinel identity，
  支援錯誤包裝，不再依人類可讀訊息辨識。

## 相容性與診斷規則

通用 failure classifier 的文字 fallback 已限制在未帶分類的持久化紀錄相容解析；
即時執行的未知錯誤歸為 execution failure。路徑與 read-only shell 的拒絕透過
原生 disposition 傳遞，重試流程不再比對拒絕訊息。既有 failure detail 的權限
判讀仍保留相容路徑；新 attempt detail 記錄明確的權限／安全拒絕標記。

壓縮已完整保留結構化 error output；普通文字中的英文診斷關鍵字仍作補充保留規則。
完整工具證據與 artifact 參照仍由既有儲存機制負責。這些補充規則不是任務成敗依據。

## 本次驗證（2026-10-30）

`mise run vet`、`mise run lint`（0 issues）及相關分類、拒絕、重試、修復與
evalharness 回歸測試均通過。最終執行
`mise exec -- go test -p 2 -timeout 30m ./...`，只有以下四個測試失敗；
四者也已在未修改 HEAD 的獨立 archive snapshot 重現：

- `TestHufuCodeReviewRuntimeOwnsAll51GapIdentities`：缺少 snapshot coverage 的來源 occurrence。
- `TestHufuCodeReviewEmptyCritiqueOwnsSource`：同樣缺少 snapshot coverage 的來源 occurrence。
- `TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope`：靜態 workflow edge 的同批契約不完整。
- `TestHufuCodeReviewSynthesisReceivesCompactPrepareStatusWithoutCoordinatorConstraints`：缺少 `scope_context` runtime output。

因此完整測試並非全綠；上述既有團隊契約 fixture 問題未納入本次修改。
