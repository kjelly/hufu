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

每個 worker 的 browser context 有一個目前頁面。對每個來源依序完成
`browser_navigate({"url":"…"})` → 等待導覽結果 → `browser_snapshot({})`
→ 讀取內容，再處理下一個網址。不可先批次導覽多個網址，再嘗試按 URL
取快照；snapshot 沒有 URL 選擇功能。平行研究由不同 worker 的獨立 context
提供。同一來源的導覽與快照也不可放在同一批工具呼叫中。

若模型只看到 `use_dynamic_tool`，先 inspect 自己的兩個 target，確認
`input_schema` 與 `worker_input_schema`，再依各 worker prompt 的範例逐步呼叫。
snapshot 的 `arguments` 必須是 `{}`；`url`、`filename` 等欄位均不接受。
參數被拒絕時，若目前頁面仍是預期來源，只需修正 snapshot 參數；若其間
已導覽其他來源，先重新導覽。快照的 `Page URL` 應與預期來源及實際觀察到的
重新導向一致，不能把別頁引文掛在原本想查的網址下。

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
- 記憶檢索使用語意排序（`semantic-retrieval: {mode: active, fusion: score_normalized}`）：
  研究 goal 多為中文，lexical 檢索讀不到中文。需要本機 Ollama 已有 `embeddinggemma:latest`；
  模型不在或尚未載入時自動退回 lexical。設定會釘在 execution policy snapshot，
  變更後既有 session 要用 `--new` 重新開始。詳見
  [Semantic memory retrieval](../../../docs/reference/semantic-retrieval.md)。
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
`blocked` 或 `partial`。工作已完成但記錄了存取缺口時回報
`completed_with_gaps`，acceptance 以 `required-workers-accept-gaps`
將其視為完成。

## 網站阻擋與證據缺口

- 依實際回傳區分驗證頁、主文缺失、快照截斷和引文不符。
  只有頁面明確識別 Cloudflare 時才作此歸因；403、console errors 或
  `unusable_response` 本身都不足以確認原因。
- 同一不可用頁面最多重試一次，再找公開原始資料或出版者授權轉載。
  替代來源必須重新導覽與取得快照，引用其實際 URL；轉載共享同一來源群組。
  不可用替代報導證明未取得原文的精確措辭或缺少的觀察時點。
- verifier 以自己的回傳判斷存取缺口，不直接沿用研究員的網站阻擋描述。
  若替代來源完整支持同一範圍的主張，以替代來源作證據，受阻網址與缺口留在限制。
  原文仍是成立主張所需的證據時，保留其原網址、空引文與未驗證判定。
- dynamic MCP gateway 的 transcript 保存經遮蔽、受 gateway 容量限制的完整回傳；
  500 字元僅用於狀態與重試預覽。超出 gateway 上限或證據保留預算時仍可能
  無法完成引用綁定，必須如實保留缺口。
- `hufu result final` 只讀取已保存報告，不重新抓取或驗證來源。
  更新後以 `--new` 重新研究，才能產生使用修正流程的新報告。

## 原子主張與提交前檢查

researcher、countercheck、verifier 的 prompt 均包含正反例與
`Before submitting findings` 檢查表。若一句話的不同部分可能得到不同判定，
就拆成獨立 finding；每項仍保留辨識事件必要的主體、時間與範圍。
事件日期、留言數及觀察時間、各方發言、傷勢與因果機制各依其證據判定。
「某人作出指控」與「指控內容成立」分開，不因前者有引文就支持後者。
拆分後超過 finding 上限時，優先回答核心問題，揭露未處理部分，不能再合併。

提交前逐項確認：引文支持完整主張、數字有對應觀察時點、說法有歸屬、
來源獨立性依實際出處、未取得的必要證據有明確缺口。研究員使用
`assessment: unverified`；verifier 使用 `verdict: unverified`，其
`evidence_basis` 可為 `insufficient_evidence`。後者不是 verdict 或 assessment。
這是模型須執行的語意檢查；JSON Schema 驗證不會自行證明主張已原子化。

## snapshot 錯誤追查紀錄（2026-10-08）

追查 run `run-20261008T142356.097246533Z-813ef9a14546`：

- workspace 的 `logs/llm/web-research/verifier/llm.log:11190` 顯示當時
  system prompt 已要求 snapshot 使用 `{}`，因此不能歸因為未載入此規則。
- 同檔第 11564–11567 行，verifier 在一輪回覆中發出四個 navigate；
  第 11656–11659 行又在同一輪回覆中發出四個帶 `url` 的 snapshot。
  這是批次產生的四個錯誤呼叫，並非收到錯誤後連續重試四輪。
- `logs/audit/audit-2026-10-08.jsonl:1752` 起可核對四個不同 call ID。
  每次失敗各有 logical tool 與 gateway 的錯誤紀錄，不能重複計為八次。
  14:27:05–14:27:06 UTC 的拒絕碼為 `dynamic_schema_invalid`，
  訊息為 `arguments.url is not allowed`。
- LLM log 第 11677 行隨後 inspect snapshot，第 11683 行的回傳包含
  空物件 `worker_input_schema`；audit 第 1770–1773 行記錄以 `{}`
  呼叫後成功。參數錯誤回饋有送達，且模型隨後修正。

直接失敗原因是把讀取「目前頁面」誤用成按 URL 抓取的無狀態工具。
gateway 的通用 `arguments` 介面允許任意欄位，各 target 的限制須 inspect；
舊 prompt 雖要求 `{}`，仍缺少逐頁執行、gateway 範例與明確修復步驟。
本次在三個 worker 補上這些規則，並保留既有 schema 拒絕機制。
參數錯誤在 MCP request 前即被拒絕，不是網站阻擋或來源不可用。

可由通用實作核對此路徑：
[gateway 的工具介面、inspect 與呼叫驗證](../../../internal/team/dynamic_tool_gateway.go)、
[參數驗證錯誤](../../../internal/team/dynamic_tool_schema.go)、
[worker policy 的參數驗證](../../../internal/mcp/worker_policy.go)。
此修正未改動 core；提示改進的模型遵循率仍需新執行驗證。
