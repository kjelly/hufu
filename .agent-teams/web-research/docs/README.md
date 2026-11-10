# web-research：搜尋與原生 Chrome CDP

團隊使用 Hufu 內建 `web_search` 發現來源，再由 runtime 原生 CDP 連接
本機 `http://127.0.0.1:9222` 的 Chrome。researcher、countercheck、verifier
各自使用獨立分頁，僅獲授權導覽、快照與人工驗證等待三個 browser 工具。
三個分頁共享研究用 Chrome profile 的 cookies，但不共用目前頁面或工具觀察。
`playwright_<角色>` 名稱是沿用的邏輯識別碼，保留現有引文契約的工具 ID；
實作已不啟動 Playwright MCP。啟動方式見 [setup.md](setup.md)。

## 研究、查核與綜合

預設流程為「兩路獨立研究 → verifier 查核 → synthesizer 綜合」：

| Runtime 階段 | Worker／契約 | 交接與完成條件 |
| --- | --- | --- |
| `prepare` | researcher／`primary-research`、countercheck／`counter-research` | 同批獨立研究，兩份 typed result 都完成才前進 |
| `audit` | verifier／`evidence-audit` | `evidence_from` 引用兩份研究；自行開頁查核，提交 `audit-v1.json` |
| `verify` | synthesizer／`research-synthesis` | `evidence_from` 只引用已完成 verifier；提交 `synthesis-v1.json` |

階段順序、四個必需 worker、typed result 與客觀 verifier 皆由 runtime
強制執行；coordinator 不能在 verifier 完成後直接結束。研究或查核若已完成
但證據有缺口，可交接 `completed_with_gaps`；`partial`、`blocked` 或失敗
不能當作已完成輸入。總結本身未完成也不能通過最終驗收。

synthesizer 只可呼叫 `submit_result`，不再搜尋、瀏覽或重做查核。
它先直接回答原問題，再整合適用條件、重要區分、例外與未決問題，
不能把每項 finding 各縮成一句就當作綜合。每段回答選擇對應的
`finding_indices`；schema 依實際已接受 finding 數量拒絕越界引用。
這只驗證引用存在，文字是否正確理解證據、保留限定語仍是模型的語意責任。

runtime 以 `x-hufu-evidence-inputs` 的 `runtime_owned: true` 綁定完整查核
結果及原 task ID、payload hash；模型不得提交或覆寫 `input_review`。
來源必須是已完成且包含 `kind: evidence_audit` 的查核，不能以原始研究、
未完成 audit 或 coordinator 的摘要代替。模型只提交 `synthesis_complete`、
`answer`、`sections`、`unresolved`。所有來源、引文、診斷、限制及原始主張
處理紀錄都從已接受的 audit 保留在最終附錄，不要求模型重新複製。

最終回覆依序呈現綜合回答、主題說明、尚待釐清、查核限制與詳細查核來源。
只有 synthesizer 宣告 `x-hufu-final-report`，避免兩份最終報告衝突。
總結只能依經 runtime 證據綁定及降級後的判定撰寫；未驗證、相反證據、
發言歸屬與事實成立的差別，不得在總結中被改成確定結論。

此流程變更需要新 session。歷史 `report-v1.json`、`report-v2.json` 保持
原內容以供核對舊結果，但不再由目前 verifier 選用；
`--var report_contract=v2` 不是新流程的切換方式。已保存的報告不會重寫。
恢復舊流程必須使用當時完整的 team 定義與原 frozen policy；保留 schema
本身不足以讓新版 team 相容舊 session。新流程請加 `--new` 或另用新工作區。

## 工具與證據流程

1. `web_search` 搜尋公開來源；搜尋摘要只作為線索。
2. 自己的 `browser_navigate` 接受一個 HTTP(S) `url`。
3. 自己的 `browser_snapshot` 僅接受 `{}`，直接回傳文字；不可指定 filename。
4. source 的 URL 必須逐字等於 snapshot 回傳的 `Page URL`，包括重新導向。
   引文必須是同一次成功 snapshot 的 YAML 區段內連續原文，允許空白差異。
5. runtime 以本 task attempt 的 runner 紀錄覆寫 call ID 與證據狀態。
   verifier 必須重新開啟來源，不能借用其他 worker 的工具紀錄。

每個 worker 各有一個專用 Chrome 分頁。對每個來源依序完成
`browser_navigate({"url":"…"})` → 等待導覽結果 → `browser_snapshot({})`
→ 讀取內容，再處理下一個網址。不可先批次導覽多個網址，再嘗試按 URL
取快照；snapshot 沒有 URL 選擇功能。平行研究由不同 worker 的獨立分頁
提供。同一來源的導覽與快照也不可放在同一批工具呼叫中。

若模型只看到 `use_dynamic_tool`，先 inspect 自己實際要使用的 target，確認
`input_schema` 與 `worker_input_schema`，再依各 worker prompt 的範例逐步呼叫。
snapshot 的 `arguments` 必須是 `{}`；`url`、`filename` 等欄位均不接受。
參數被拒絕時，若目前頁面仍是預期來源，只需修正 snapshot 參數；若其間
已導覽其他來源，先重新導覽。快照的 `Page URL` 應與預期來源及實際觀察到的
重新導向一致，不能把別頁引文掛在原本想查的網址下。

三個角色皆設定 `page-dwell: {min-seconds: 300, max-seconds: 600}`。
導覽載入完成即回傳；第一次 `browser_snapshot` 先擷取原文與按鈕，
再隨機停留 5–10 分鐘，結束才回傳等待前擷取的快照與擷取時間。
後續快照讀取當前內容，不重複停留。這段停留不計入 worker／coordinator 的工作 timeout，
亦不計入 `--max-duration` 總時間預算；取消與外部操作期限仍生效。不要再加另一段閱讀等待，亦不可把停留
完成當作頁面已讀取或主張已驗證的證據。調整範圍會改變 frozen policy。

三個角色同時啟用 `page-verification: true`。快照工具由 runtime 呼叫既有
decision runtime 判斷頁面；可沿用 sidecar，或透過 `decision-runtime` 設定
System One。只有明確判定為安全／機器人驗證頁時，工具內才要求人工接手，確認後
重新擷取與判斷，只有正文可讀才回傳成功。即使沒有按鈕、來源只是備援副本，
也會進入此流程。設定的初次快照與停留順序仍保留；若挑戰在停留時自行解除，
runtime 會以新快照確認後繼續。同一未解決網址不反覆要求人工處理。
模型判斷故障、不確定或存取錯誤會回報工具錯誤，不要求人工確認。
直接呼叫 `browser_wait_for_user` 也須通過新快照的驗證頁判定，並共用同一網址的請求次數限制；
不能用人工確認修復分類器故障。使用者取消或無人模式不會被當成讀取成功。

若仍遇到需要人工處理的頁面問題，可呼叫自己的
`playwright_<角色>__browser_wait_for_user({"reason":"請完成公開頁面的人工驗證"})`。
使用 dynamic gateway 時先 inspect 該 target，再以相同 `reason` 欄位呼叫。
Hufu 會切到該角色的分頁並等待 CLI／TUI 回答；等待期間不計入 worker 與
coordinator 的工作 timeout 與 `--max-duration` 總時間預算，重疊等待只扣一次。
取消、Ctrl+C 與外部操作期限仍有效。人工確認後必須再次 `browser_snapshot({})`，確認實際 URL 與原文；
確認本身不是證據，也不能修復 404 或引文不符。無人值守、使用者取消或
驗證後仍無法讀取時，保留缺口並尋找替代來源，不反覆要求相同的人工驗證。

快照的網址由 Chrome frame metadata 綁定，擷取前後的文件身分必須一致。
固定的隔離觀察程式讀取主文與連結，回傳 inline YAML。metadata、搜尋摘要、
人工確認與失敗回傳皆不能通過引文綁定。團隊禁止內建 fetch、download 與
shell 工具。此設定沒有外部 MCP 程序或 Playwright 暫存目錄；Hufu 正常關閉
時只關閉自己建立的分頁，Chrome 與研究用 profile 由操作員管理。

## 通用 runtime 支援

`toolPolicies` 是團隊維護者對受信任 MCP server 的明確宣告，搭配 worker
literal 工具 grant 才能授權 unbound worker 的 MCP 工具；參數 schema 在送出
MCP request 前驗證。唯讀快照使用 `sideEffect: none`；原生點擊另需
`browser_interaction` task 與同類工具 policy，不會藉由唯讀例外授權點擊。
此宣告不接受 server 自稱 readonly，也不允許同名自訂 handler 繼承授權。
`--no-net`、team deny、session deny、工作階段與封閉工具順序限制仍生效；
bound workset 仍拒絕不支援 artifact path enforcement 的 MCP 工具。

這是授權與參數驗證機制，並非 MCP server 的作業系統 sandbox。
`filesystem: none` 指不授權模型指定本機讀寫路徑；browser 自身的暫存與
session 狀態由受信任 server 管理。HTTP(S) 格式驗證不會阻擋私人 IP、
網站重新導向或具有副作用的 GET；公開來源限制仍由 team instructions 與
部署環境的網路隔離共同負責。

## 執行環境

- 三個 worker 設定 `reasoning-effort: none`，並保留 65,536 token 的單輪
  輸出預算。長證據的結果修復曾用滿 24,576 token，未產生 `submit_result`；
  即使 reasoning 計數為零，部分模型仍把思考文字放進一般文字欄位，因此
  不能只靠 thinking 控制保障提交空間。此預算不放寬引文、schema、證據綁定
  或驗收。Ollama OpenAI 相容端點以 `none` 請求關閉 thinking，實際模型行為
  仍需驗證（[官方說明](https://docs.ollama.com/api/openai-compatibility)）。
- 設定 `OLLAMA_API_KEY`，供內建 `web_search` 使用。
- 記憶檢索使用語意排序（`semantic-retrieval: {mode: active, fusion: score_normalized}`）：
  研究 goal 多為中文，lexical 檢索讀不到中文。需要本機 Ollama 已有 `embeddinggemma:latest`；
  模型不在或尚未載入時自動退回 lexical。設定會釘在 execution policy snapshot，
  變更後既有 session 要用 `--new` 重新開始。詳見
  [Semantic memory retrieval](../../../docs/reference/semantic-retrieval.md)。
- 使用支援原生 CDP 的 Hufu，並依 [setup.md](setup.md) 啟動有畫面的 Chrome。
  Chrome 與 Hufu 在同一台 Ubuntu，除錯網址固定為 `http://127.0.0.1:9222`。
  執行環境不需要 Node.js、npm 或 Playwright；人工驗證需要互動式 CLI／TUI。
- 三個角色目前共享研究用 profile 的 cookies；若需要 cookie 隔離，應使用
  不同 Chrome profile／port，並同步修改對應宣告的 `url`。

原生連線設定見 [Chrome CDP](../../../docs/reference/chrome-cdp.md)。
通用設定參見 [MCP worker policy](../../../docs/reference/mcp-worker-policy.md)。

## 驗證與執行

團隊預設啟用 `request-acceptance: true`，依每次使用者任務產生並驗收動態要求，
不必再加 `--request-acceptance`。動態要求會補充既有團隊驗收；明確要求取得原文時，
替代摘要不能取代該要求。單次停用可用 `--request-acceptance=false`。
此設定屬於 execution policy snapshot；既有 workspace 首次套用時需加 `--new`。
詳見 [Request acceptance](../../../docs/reference/request-acceptance.md)。

```bash
hufu team validate --team web-research
hufu list web-research
hufu --agent-team web-research --dry-run '查核一項公開事實並列出來源'
hufu run --team web-research --workspace /path/to/a/new-workspace '查核一項公開事實並列出來源'
```

此次 Chrome CDP 的 server 設定與新增工具授權會改變 frozen policy。
請使用新的 workspace；既有 Playwright 任務若仍需恢復，必須使用原本的
team 定義，不能直接以 CDP 設定繞過 drift 檢查。歷史結果與收據不會重寫。
以下恢復方式只適用於政策相容的 session。使用包含新 runtime 支援的 Hufu binary，並以
`resume --team web-research --branch <來源分支> --model <原 worker model>`
接續；先透過 `session list`、`inspect task --branch` 核對來源。已完成的
researcher、countercheck 結果保留原 task ID、run ID 與 payload hash，後續
透過 `evidence_from` 引用，只派發尚未完成的工作。

若 verifier 已有瀏覽證據但提交停在 blocked protocol，使用同一 resume
命令加上 `--repair-result <原 verifier task ID>`，明確授權有限的結果提交
修復。runtime 只提供 `submit_result`，承接原工具收據與觀察，不重新瀏覽；
舊修復歷史與原分支成果仍保留。模型耗盡輸出卻未提交時，應檢查修復診斷。
此命令不能以更弱的契約取代缺失證據。

工具政策、schema 或 server 設定不同，resume 會 fail closed，並列出可供
核對的分支；不會自動改用 main 或啟動全新研究。只有刻意換用不同契約時，
才另行建立新工作流程。
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
- 遇到驗證頁先使用自己的人工等待工具，確認後重新快照；不要自動化驗證。
  仍不可用的頁面最多重試一次，再找公開原始資料或出版者授權轉載。
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

## 部分支持、發言歸屬與單一判定來源

- 複合主張只驗證一部分時，拆成逐項 finding，不得保留原句再標
  `supported（僅某人部分）`。不同人物的發言、所援引文件與數值各自驗證；
  未處理部分明列缺口。`mixed` 用於同一命題的已查證證據有分歧，不能代替缺證。
- 甲引用某數字，不能代證乙也說過，或證明乙援引的丙確曾答詢。
  每個歸屬環節都要有對應證據；只能驗證甲時，縮小結論到甲的發言，
  乙、丙及數值本身的真實性仍依各自證據判定。
- 最終判定只由經 runtime 引文綁定後的 `findings[].verdict` 呈現。
  `claim_assessments` 保留每個原始輸入的處理情形，不是第二套事實判定。
  `addressed` 表示已涵蓋，也可能對應未驗證的結論。模板將原始主張明確標為
  研究輸入，只呈現其處理狀態，不呈現模型自由撰寫的 `reason`，避免正文遭
  runtime 降級後，末尾仍殘留舊判定。理由仍留在結構化結果供追查。
  summary 也只記錄流程，不複寫判定。

team 內的模板回歸測試：

```bash
GOMAXPROCS=2 mise exec -- go test -p 2 ./.agent-teams/web-research/tests
```

它驗證最終判定改變時不會呈現舊的處理理由，並保留原始輸入與來源缺口。
原子性及人物歸屬仍由模型比對語意；schema 與模板不能自行證明內容正確。
更新不會改寫已保存的歷史報告；以 `--new` 重新研究才能套用新契約。


## 提交契約與既有 session

`audit-v1.json` 延續舊 report v2 的外層提交欄位限制、runtime-owned 研究
輸入、引文綁定與原始主張涵蓋檢查，但把最終呈現交給後續綜合契約。
另外要求兩份研究輸入分別具有 `primary_search` 與 `countercheck` lens。

候選保存與局部修復仍使用通用 runtime 能力。修復承接原收據與已接受
來源，不重跑研究或瀏覽；修復時依當前工具 schema 修改被拒絕欄位。
新增綜合階段不會修復歷史失敗任務，也不會改寫已保存的證據。

## 彈窗處理

快照的 Buttons 區段列出按鈕名稱、disabled 狀態與該次快照專用 ref。
研究員可用 `playwright_<角色>__browser_click({"ref":"快照中的識別碼"})`
點擊明確的關閉／拒絕非必要 Cookie 控制。runtime 檢查文件與按鈕是否仍有效，
拒絕失效、停用、被遮蔽、連結或表單控制；不接受 selector、座標或 JavaScript。
點擊後自動重新擷取，研究員須確認實際 URL 與原文，再呼叫 `browser_snapshot({})`
取得可綁定引文的證據。Buttons 與 click 回傳不直接作為引文來源。
無法確定按鈕用途或三次嘗試後仍被遮蔽時，改用人工等待。

三個瀏覽角色／task contracts 使用 `side_effect: browser_interaction` 與
`recovery: manual`，專案檔案仍唯讀，但不把點擊當成可自動重播的觀察。
不得自動登入、送出表單、購買、授予權限或處理 CAPTCHA。
變更會影響 frozen policy；既有執行的歷史資料與 checkpoint 不可改寫。
