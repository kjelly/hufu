# 查詢歷史任務結果

> Status: active
> Authority: guide
> Verified-Commit: —（本次工作樹，尚未提交）
> Supersedes: —
> Superseded-By: —

`hufu results` 查詢保存的任務結果，使用者不需要先找 run-id。它從已驗證的 canonical events 讀取任務描述、完成當時的 typed result 和文字結果；`hufu history` 則維持搜尋 prompt 的用途。

## 重看完整最終回覆

```bash
hufu result --team <team>
hufu results --team <team>
hufu result final --team <team>
hufu result --team <team> --all-branches
```

`result` 是 `results` 的別名。不帶子指令時，stdout 直接印出範圍內最近一次 run 保存的完整最終回覆正文，不再把個別 agent 的驗證回條當成最後的結果。預設讀取與 `final` 的 stderr 顯示完整 `Original request`（原始任務）、可見 branches、team、完成時間、invocation、run、result 與來源 event，便於確認讀取的是哪一次執行；`--quiet` 可關閉這些資訊。原始任務不截斷，保留換行；舊紀錄缺少原文時明確標示，不能用報告的「問題」或 agent 描述代替。這個查詢不呼叫模型、不重新執行任務，也不以 summary 或 reason 取代正文；沒有保存的正文時明確報錯。`--agent`／`--status` 須搭配 `list`，分頁須搭配 `runs`。

這是指定 team 的保存回覆正文。CLI 即時 status、execution summary、team／agent 包裝標題和多 team invocation 的完整聚合文件，並非這個歷史記錄的保存契約；不能把較舊的多 run 回覆自行拼湊成原始 CLI 文件。JSON 查詢仍使用 historical_results envelope，在 results[0].output 保存完整正文與來源資訊，不等於 `run --output json` 的整份執行統計文件。

## 瀏覽過去的最終回覆

```bash
hufu results runs --team <team>
hufu results runs --team <team> --all-branches
```

`runs` 顯示最終回覆列表，每筆都有完成時間、team、執行 outcome、最多三行原文預覽和完整閱讀指令。這些預覽不是 LLM 摘要，`completed` 表示 run 完成，不表示報告沒有 findings。storage path、index 和個別 worker 的執行約束不出現在這個列表。

`runs` 與個別任務列表都會顯示 `Original request`（原始提問）及 `Branches (visible in)`（可讀到這筆結果的 branch，包含繼承它的 branches）。`list` 另外以 `Agent task` 標示協調者交給 agent 的子任務，以 `Result (excerpt)` 顯示保存結果的摘要或文字摘錄，方便對照「原始提問 → 子任務 → 結果」。

原始提問來自該任務起始 run、相同 session 與選定 branch lineage 的第一筆 canonical user message，重試或跨 run 恢復不會改用新 run 的提問；run 最終回覆則使用自己的 run。這是保存的提問原文，不是模型產生的摘要，也不保證等於多 team invocation 的完整 CLI prompt。舊紀錄沒有保存提問時會明確標示缺少資料，不以 agent 描述猜測。列表最多預覽 300 字元；`show` 顯示完整提問與子任務描述。JSON 同時提供 `original_prompt` 與 `original_prompt_event_id` 供精確追溯。

要查個別 agent 的結果或重試紀錄，使用 `hufu results list --team <team>`。JSON 預設查詢只回傳最新一筆保存的 run_response；`runs --output json` 回傳分頁列表，`list --output json` 保留 worker 查詢。沒有保存的最終回覆時，runs 列表會說明仍可查個別任務。

## 互動式模糊搜尋與閱讀

```bash
# 搜尋原始提問與保存的最終回覆，選取後直接顯示全文
hufu result pick --team <team> --all-branches

# 預先填入搜尋文字
hufu result pick "逾時" --team <team> --all-branches

# 改查個別 agent 任務結果
hufu result pick --tasks --team <team> --all-branches
hufu result pick --tasks --team <team> --agent verifier --status done
```

這是內建的 Bubble Tea 選擇器，不需要安裝 `fzf`，也不呼叫模型。直接輸入文字即時篩選，支援中文及不連續字元的模糊比對；空白分隔的多個詞必須全部命中。上下鍵或 Ctrl+P／Ctrl+N 移動，Enter 選取並離開選擇器、顯示完整保存結果；Esc 或 Ctrl+C 取消，stdout 不輸出結果。

預設每筆是一次 run 的最終回覆，畫面顯示原始提問、時間、team、branch 與結果預覽。`--tasks` 改為個別 agent 任務，預設只列 `done`，可搭配 `--agent`／`--status`。搜尋欄位包含原始提問、子任務描述、結果摘要（這三者各最多前 1000 字元）、team、agent、branch 與 result／run／task ID；選取後的全文不受預覽長度限制。

所有候選沿用明確的 team／workspace／branch／run／invocation／時間範圍；沒有 `--all-branches` 時仍只查目前 branch lineage。不使用列表的 20 筆分頁上限；`--limit`／`--cursor` 不適用。範圍超過 10000 筆時會要求先以篩選條件縮小，不會默默漏掉較舊結果。

選擇器使用 terminal stdin 與 stderr，stdout 保留給選中的完整結果，因此可用 `--output json` 或 `--render markdown`，也可將結果導向檔案。stdin／stderr 非終端或設定 `--display-mode plain` 時，改用 `runs`／`list`／`search`。沒有結果時明確提示；選取器不會重新執行任務或重新查詢其他 scope。

## 用自然語言指定 team 查詢

```bash
hufu results query "昨天完成的任務有什麼重要發現？" --team <team>
hufu results query "之前處理 timeout 的任務用了什麼解法？" --team <team>
hufu results query "比較最近兩次檢查的結果" --team <team> --model <provider/model>
hufu results query "上週有哪些結果？" --team <team> --timezone Asia/Taipei
```

`query` 使用 LLM 解析條件、選取相關結果，再根據保存的內容回答。回答中的 `[1]`、`[2]` 連結到 source list，每個來源都有可複製的閱讀指令。模型推論不代表重新驗證過去的執行；缺少修復／驗證證據時，不能只憑較新的成功任務判定舊問題已修好。

模型沿用指定 team 或全域的 sidecar/model 設定，CLI `--model` 優先。模型只接收目前範圍內經 redaction 的描述和結果 excerpts，不具備執行工具。每次查詢最多三次 generation，包含資料讀取的總時間上限為 60 秒。

`--team` 適用於所有 results 子指令。它在結果內容送入模型前限制範圍；模型不能改查另一個 team。未指定 team 時，使用目前 project 已註冊的 active team workspaces。`--workspace <path>` 可改用單一明確 workspace。

## 讀取跨 session 的歷史

```bash
hufu results query "以前處理過哪些 timeout 問題？" --team <team> --all-branches
hufu results list --team <team> --all-branches
```

預設只讀取 active branch 的 lineage。`--new` 建立獨立的 session root，舊結果仍保存在原來的 branch；使用 `--all-branches` 才會包含這些舊 session 與其他 branches，也可用 `--branch <id|name|label>` 指定一條 lineage。branch inheritance 的同一結果會去重；不同 branches 對結果有效性有不同狀態時，顯示 `branch_dependent`，應指定 branch 閱讀。

## 讀取最後的回覆

```bash
hufu results final --team <team>
hufu results final <result-id> --team <team>
```

`final` 不給 ID 時讀取範圍內最近一次 run 保存的最終回覆，不需要 run-id。給個別 agent 的 result-id 時，則讀取那筆結果所屬 run 的最終回覆；尚無保存的回覆時會明確報錯，不會改用另一個 run 的結果。跨 session 查詢仍須加 `--all-branches`。

`show <result-id>` 讀取該 ID 指向的原始記錄。若它是 reviewer 的驗證回條，就只會顯示驗證回條；task-id、run-id 與 `final` 指令會協助找回最終回覆。`latest` 仍指最近完成的個別 agent 任務，最後完成的任務可能只是驗證步驟。

`final` 使用原本保存的 run response，不重新執行 LLM，也不會自行拼湊 worker 結果。多 team invocation 的各 run response 仍是個別回覆，不代表 CLI 聚合輸出的完整副本。run outcome 可能是 partial 或失敗，應一起閱讀。`--agent` 和 `--status` 是 worker filters，不能搭配 `final`；可先找 worker 的 result-id，再交給 `final`。

## 離線列表與結果閱讀

```bash
hufu results list --team <team> --limit 20
hufu results search "timeout" --team <team>
hufu results latest --team <team>
hufu results show <result-id> --team <team>
hufu results invocation <invocation-id> --team <team> --all-branches
```

這些指令不呼叫模型。`search` 支援 Unicode literal matching，包含不以空白分詞的中文；它不會解讀相對日期或自然語言條件。list/search/latest 預設選已完成的 `done` 任務，`--status error`、`--status blocked` 等可選其他保存的 terminal 狀態。

`latest` 依 canonical terminal event 時間選最近的一筆，`show` 顯示保存的 typed result 與完整文字交付。artifact／transcript 欄位是原來保存的 references，這些指令不載入或執行其內容，也不代表外部檔案仍然存在。

`show` 使用獨立的 CLI formatter，保留 details 的縮排、findings 的 severity 與 decisions 的 reason；不使用給模型看的 `FormatForContext()`。一般文字閱讀只預覽不超過 2 KiB 的 structured payload，較大者顯示大小與完整閱讀方式：

```bash
hufu results show <result-id> --team <team> --payload
hufu results show <result-id> --team <team> --output json
```

`--payload` 只印出完整、縮排後的 payload JSON value，包含超過 16 KiB 的內容，沒有文字標題或 stderr 來源提示。沒有 payload 時明確報錯；不能與 `--output json` 或 `--render markdown` 同時使用。`--output json` 則輸出整份 historical_results envelope，包含 typed result、原始 payload hash 與來源資訊。格式化的空白不代表重新計算或更新 canonical hash；查詢仍保留 team／branch／時間範圍及 redaction，並不開啟 artifact 路徑。

## 終端 Markdown 閱讀

```bash
hufu result --team <team> --render markdown
hufu results final <result-id> --team <team> --render markdown --theme light
hufu results show <result-id> --team <team> --render markdown
```

預設 `--render plain` 保留正文。Markdown 排版使用 Glamour，依終端寬度與 Hufu 的 light／dark／auto theme 渲染標題、清單及程式碼。只有 stdout 是可用色彩的 TTY 時才渲染；管線、`NO_COLOR`、`--no-color`、`--theme mono`、`--display-mode plain` 及 epaper 保留無 ANSI 的原文。JSON 輸出須省略 `--render markdown`。渲染上限為 1 MiB，超過時請改用 plain 閱讀全文。

新的 hufu-code-review 報告先列審查結論、原始 findings、Critic 判定與後續處理，再列完整證據、原始缺口及跨工作集解答。測試通過不代表沒有 findings；`unverified` 不會被升格為已確認缺陷。這項編排只影響之後產出的報告，已保存的歷史正文保持原樣。

主報告以原始問題名稱與位置作為標題，並列出原始影響／說明、可讀的複核判定、理由、待補證據與建議。task 編號、完整 payload SHA-256、artifact references 及「原始審查 → 獨立複核」對應保留在稽核附錄，讓閱讀問題時不用先查 task 做了什麼。

`invocation` 顯示範圍內所有關聯的任務與保存的各 run response。一個 invocation 可以包含多個 run；這些 response 不一定等於原始 CLI 最終聚合輸出的完整副本。

## 結果識別碼與即時通知

result-id 是 Hufu 由 canonical terminal event 自動產生的 `res-...` 定位碼，不是使用者要自行建立的 task-id 或 run-id。執行途中用 task-id 顯示進度；每筆結果 durable 後即時顯示 result-id 和 team-qualified 閱讀指令，不必等整個 invocation 結束。

terminal status 與 TUI task detail 都顯示這個引用；TUI logs 可用既有選取／複製操作複製它。JSONL transport 的 `task_result_ready` event 在 `result` 欄位提供 schema_version、result_id、run_id、task_id、attempt、branch_id、source_event_id 和可用的 invocation_id。quiet／JSON final-result stdout 維持原有格式。

一般自然語言查詢不需要 result-id。它主要用於精確閱讀與引用，`show` 接受完整 ID 或唯一的短前綴（至少 12 字元），歧義時必須改用完整 ID。

## 自動化與錯誤處理

```bash
hufu results list --team <team> --output json --limit 10
hufu results query "昨天完成了什麼？" --team <team> --output json
hufu results query "昨天的結果" --team <team> --preview
```

JSON envelope 的 schema_version 是 1，包含 operation、coverage、matched_count、results，以及 query 使用的 plan、sources、answer blocks 和 usage。preview 不呼叫模型，不解析自然語言，也不宣稱 query 已命中；只讀取明確的 scope。

`runs` 與其他離線列表超過 limit 時提供 next_cursor，以 `--cursor <value>` 續查；scope/filter 或 event chain head 改變時 cursor 失效，應重新開始查詢。預設完整回覆與 final 不支援 cursor。`--since`、`--until` 接受 RFC3339，區間是包含 since、不包含 until 的 terminal-event 時間範圍。

| Exit code | 意義 |
| --- | --- |
| 0 | 查詢完成，包含零命中與 preview |
| 1 | canonical integrity／讀取失敗 |
| 2 | 查詢或 scope 無效、歧義、需要 clarification、超出讀取預算 |
| 3 | LLM unavailable、timeout、或 model response 契約失敗 |

LLM 失敗時仍可使用 list/search/show；JSON 的 operation 明確標示 failed。候選或 excerpts 受上限截斷時回傳 truncated，回答不能代表全部歷史結果。

query 沒有 literal 命中時，會在同一個已授權的 team、branch、時間、status 範圍內，以近期結果作有界候選交給 LLM 判斷，並標示 recency_fallback。這不會擴大讀取範圍，也不表示那些候選已符合問題；只有模型選取的 sources 用於回答。matched_count 是候選集合的 filter 計數，lexical_match_count 是 literal 命中數，兩者都不能當成自然語言問題中「全部符合的任務數」。

## 保存與目前界線

歷史來源獨立於 task-journal cache 和 tasks Markdown，`--new`、`--no-journal` 或 journal compaction 不會刪除已保存的 canonical 結果。撤回或重試的舊結果仍保留，validity 顯示 invalidated／superseded。這是完成當時的記錄，不能冒充目前有效的交付。

第一版從事件重建，index_state 顯示 `event_replay`；沒有自動索引維護、embedding 呼叫或 workspace 寫入。單次 snapshot 的上限為一百萬 events 與 256 MiB event payload，超限直接失敗，不回傳貌似完整的部分結果。只有 journal／Markdown 而無 canonical anchor 的內容不納入；已被刪除且無 durable copy 的內容無法恢復。workspace 被明確 purge 後，相關歷史資料也隨之消失。

設計與後續性能／語意品質工作見 [改善計畫](../implementation-plans/historical-task-results-llm-query.md)。

## 歷史執行報告與 Markdown 匯出

新執行的 `run_finished` 與報告快照會在同一筆 SQLite transaction 保存到
`runtime.sqlite`。不需要加 `--report`，也不依賴 task journal。
每份快照綁定來源 event ID／hash、team、session、invocation、run 與來源 branch；
切換 branch、開始新任務或覆寫 `report.md` 不會改寫它。

```bash
# 搜尋原始任務，選取後閱讀該 run 的執行報告
hufu result pick --team web-research --all-branches --view report

# 閱讀最近一個 run 的報告，或用 worker／run response 的 result-id 定位
hufu result report --team web-research
hufu result report <result-id> --team web-research --all-branches

# 匯出同一份保存資料，或以 JSON 同時取得 report 與 report_markdown
hufu result report <result-id> --team web-research --export report.md
hufu result report <result-id> --team web-research --output json
```

`--branch`、`--all-branches`、`--run` 與時間範圍和其他 result 指令相同。
繼承自父 branch 的報告按 event lineage 可見；選取 worker 結果時，讀取的是該結果
完成所在 run 的報告。`pick --view report` 不能與 `--tasks` 同用。
預設 `result`、`result final` 和 `pick` 繼續顯示最終回覆。

執行時的 `--report` 與 team 的 `auto-report: true` 現在只控制是否將當次 team run
的保存報告匯出為 workspace 的 `report.md`；每個 team 使用自己的回覆。
歷史保存不再由這個 flag 決定。dry-run 或尚未確認持久完成的 run 不會匯出舊報告。
匯出為原子寫入，檔案權限為 `0600`；
`result report --export` 不能覆寫 runtime／context SQLite 檔案。

報告包含當次原始請求、最終回覆、觸及的任務契約／結果、findings、驗證紀錄、
run outcome／metrics，以及事件中保存的成本、模型與 context routing 資料。
任務重試／恢復會保留原契約；其他 run 未觸及的任務不會混入。
不讀取可變 STM、任務 Markdown 或即時 workspace 診斷，不重新查驗外部 artifact；
缺少的觀測資料不宣稱為已量測。這是執行當時的報告，不代表目前 artifact 仍存在。
Markdown 由版本化結構快照渲染；未保存 Markdown 的舊版排版副本。

本功能啟用前的 run 沒有報告快照時，會明確回報 unavailable；不會以最新 checkpoint
或舊 `report.md` 補成歷史報告，也不會在查詢時寫入／遷移 DB。
每份快照上限 8 MiB；超限或無法投影時記錄 unavailable，保留原本 run outcome。
真正的 SQLite 寫入失敗會使整筆 transaction 回滾，沿用 terminal persistence 的恢復流程。
