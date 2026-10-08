# web-research：搜尋與 Playwright MCP

團隊使用 Hufu 內建 `web_search` 發現來源，以 `@playwright/mcp@latest`
開啟公開網頁，再用 `browser_snapshot` 的文字回傳綁定引文。
researcher、countercheck、verifier 各使用獨立 headless Chromium context，
各 worker 僅獲授權存取自己的兩個 browser 工具。

## 工具與證據流程

1. `web_search` 搜尋公開來源；搜尋摘要只作為線索。
2. 自己的 `browser_navigate` 接受一個 HTTP(S) `url`。
3. 自己的 `browser_snapshot` 僅接受 `{}`，直接回傳文字；不可指定 filename。
4. source 的 URL 必須逐字等於 snapshot 回傳的 `Page URL`，包括重新導向。
   引文必須是同一次成功 snapshot 的 YAML 區段內連續原文，允許空白差異。
5. runtime 以本 task attempt 的 runner 紀錄覆寫 call ID 與證據狀態。
   verifier 必須重新開啟來源，不能借用其他 worker 的工具紀錄。

`--snapshot-mode none` 避免導覽自動輸出 snapshot 檔案；明確的
`browser_snapshot` 仍回傳 inline YAML。`--codegen none` 與 `--no-webmcp`
減少非來源內容。metadata、搜尋摘要、snapshot 檔案連結與失敗回傳皆不能
通過引文綁定。團隊禁止內建 fetch、download 與 shell 執行工具。

## 通用 runtime 支援

`toolPolicies` 是團隊維護者對受信任 MCP server 的明確宣告，搭配 worker
literal 工具 grant 才能解除 unbound worker 的外部工具與 `side_effect: none`
限制；參數 schema 在送出 MCP request 前驗證。
此宣告不接受 server 自稱 readonly，也不允許同名自訂 handler 繼承授權。
`--no-net`、team deny、session deny、工作階段與封閉工具順序限制仍生效；
bound workset 仍拒絕不支援 artifact path enforcement 的 MCP 工具。

這是授權與參數驗證機制，並非 MCP server 的作業系統 sandbox。
`filesystem: none` 指不授權模型指定本機讀寫路徑；browser 自身的暫存與
session 狀態由受信任 server 管理。HTTP(S) 格式驗證不會阻擋私人 IP、
網站重新導向或具有副作用的 GET；公開來源限制仍由 team instructions 與
部署環境的網路隔離共同負責。

## 執行環境

- 設定 `OLLAMA_API_KEY`，供內建 `web_search` 使用。
- 安裝 Node.js 18 以上版本及 npm，讓 `npx` 可在 PATH 中找到。
- 首次啟動需要取得 npm 套件與相容 Chromium。
  執行 `npx -y --package=@playwright/mcp@latest playwright-core install chromium`，
  讓 browser 與 MCP 當次使用的 `playwright-core` 版本相符，
  並讓安裝與執行使用同一個 `PLAYWRIGHT_BROWSERS_PATH`（若有設定）。
  `@latest` 可能包含預發行版，單獨安裝 stable Playwright 的 browser 不保證相容。
- `@latest` 依使用者要求保留；更新可能改變工具描述、回傳格式與 browser 需求。
  工具描述或 schema 變更會觸發 frozen catalog 檢查；不符既有文字格式時，
  引文驗證會降級。此檢查不會驗證 npm 套件程式碼本身。

啟動參數參考 [Playwright MCP 官方說明](https://github.com/microsoft/playwright-mcp)。
通用設定參見 [MCP worker policy](../../../docs/reference/mcp-worker-policy.md)。

## 驗證與執行

```bash
hufu team validate --team web-research
hufu list web-research
hufu --agent-team web-research --dry-run '查核一項公開事實並列出來源'
hufu --agent-team web-research --new '查核一項公開事實並列出來源'
```

使用包含新 runtime 支援的 Hufu binary，並以 `--new` 開啟 session。
舊 session 的工具政策、schema 或 server 設定不同，resume 會 fail closed。
缺失的 policy 工具會在 coordinator 初始化時報錯；dry-run 不會檢查實際
browser 啟動、網站可達性或供應商 API。

當 snapshot 呼叫失敗而沒有回傳 URL 時，runtime 無法將它可靠歸屬到某個
來源，該來源會顯示 `not_attempted`；worker 應在 limitations 記錄實際錯誤。
網站拒絕存取、內容缺失或引文不符時，保留來源與精確缺口，不宣稱已驗證。
完整流程仍要求兩份研究結果與 verifier 的完成報告；未完成工作回報
`blocked` 或 `partial`。
