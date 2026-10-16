# Hufu 歷史任務結果與 LLM 查詢改善計畫

> Status: active
> Authority: reference
> Verified-Commit: `494171c806104b39ef9306069b66eb452a56f768`
> Supersedes: —
> Superseded-By: —
> Proposed-Date: 2026-10-14
> Execution-Status: 核心 CLI、team 篩選、LLM 三階段查詢與即時結果身份已於工作樹實作；完整驗證與實際模型品質評估見目前交付狀態
> Scope: team-neutral 歷史結果讀取、LLM 查詢、結果保存與 CLI
> Reviewed-Baseline: 上述 HEAD 的目前 working tree；包含其他進行中的 runtime 變更，開工時重新確認實際介面

使用者應能直接問「昨天完成的 code review 有哪些發現」或「之前處理 timeout 的任務結果是什麼」，由 Hufu 搜尋已保存的歷史任務並回答，無須先取得 run-id。回答必須連回實際任務及保存的結果；模型無法取得資料時，應清楚指出缺少什麼。

本計畫新增 `hufu results`，以 LLM 自然語言查詢為主要入口，同時提供離線列表、精確篩選、結果閱讀與 invocation 查詢。核心指令與 schema 已於本次工作樹實作，操作以 [使用指南](../guides/historical-results.md) 為準；工作包中的性能索引與語意品質門檻保留為後續交付條件。歷史 execution facts 沿用既有 canonical event chain，搜尋索引是可重建的 projection，LLM 回答是推論。

## 目前交付狀態

- 已實作 team-scoped list/search/latest/show/final/invocation/query、Unicode literal retrieval、typed LLM resolver/selection/answer、來源引用、分頁 snapshot、終端/TUI/JSONL result identity。`final` 讀取保存的 run response；`show` 區分個別 agent 結果與 run response，避免把最後完成的驗證回條當成最終交付。
- 無子指令的 `result`／`results --team <team>` 直接印出最新保存的完整最終回覆正文，沒有 history metadata 或預覽。`runs` 顯示有界預覽與分頁的回覆列表，`list` 保留個別 agent 和重試記錄。JSON 使用既有歷史 envelope 與完整 run_response，不宣稱能重建未保存的 CLI 包裝標題、多 team 聚合或執行統計。
- canonical task snapshots 已保存 typed result 與文字結果，讀取重用既有 event chain 與 reducer，沒有新增第二份結果 truth store。`--new` 建立的舊 root 透過 `--all-branches` 明確讀取。
- query 沒有 literal 命中時，以同一授權 scope、日期與 status 的近期候選作明確的 recency fallback，再由 LLM 判斷相關性；這不適用於 resolver 失敗，不能猜測未解析的意圖。回傳 lexical_match_count、recency_fallback 和截斷資訊。
- 目前使用 event_replay，沒有實作落盤索引、embedding 或 index build 指令；索引維護段落仍為條件式設計。artifact/transcript 只回傳保存的 references，不宣稱檔案現況已驗證。
- preview 只顯示明確 scope 與呼叫者設定的 model；不解析問題。clarification/unsupported 回傳可讀診斷與 exit 2，由使用者帶補充問題重新查詢。
- 契約測試與真實 binary 搭配本地 fixture provider 的三次生成流程已通過；本機未提供 Ollama service，尚未實測實際模型的自然語言 recall/support gate，不能宣稱達到後述 90%／95% 指標。實際檢查與界線記錄於 [驗證記錄](../reference/historical-results-validation.md)。

## 1 目前問題與可重用能力

| 現況 | 來源與限制 | 改善方向 |
| --- | --- | --- |
| `inspect task` 需要 task-id 與 run-id | [inspect CLI](../../cmd/hufu/inspectcmd.go)、[InspectQuery](../../internal/inspect/types.go)；查詢結果以 metadata 為主 | 新入口自動探索歷史結果，再綁定精確身份；既有 inspect 契約維持相容 |
| invocation 與 run 有保存的關係 | [RunEvent](../../internal/team/event_store.go)、[CLI identity](../../cmd/hufu/run_identity.go)；一個 invocation 可以包含多個 run | 依 invocation 找到範圍內所有 run 與任務，不只選最近的一個 |
| 任務 Markdown 有描述與 Result | [workspace](../../internal/team/workspace.go)；[new session](../../cmd/hufu/team_loader.go) 會清理 tasks 目錄 | 用作人類可讀 projection，不作歷史結果唯一來源 |
| task journal 保存 typed result 與 output | [task journal](../../internal/team/task_journal.go)；大於 5 MiB 時會壓縮成可重用的 cache entries，不能承諾永久保存所有 result records | 歷史查詢與 cache retention 分離，canonical result 不受 journal 壓縮影響 |
| task events 已能 replay typed result、receipt 與 verification | [event reducers](../../internal/team/event_reducers.go)、[TaskResult](../../internal/team/task_result.go) | 重用 replay 與 accepted attempt 選擇，避免重寫成功判定 |
| `hufu history` 搜尋 prompt | [prompt history](../../cmd/hufu/history.go)；沒有任務結果查詢 | 保留原有語意，新增 results 指令 |
| `hufu explain --ai` 可分析一個 scope 的進度 | [explain CLI](../../cmd/hufu/explaincmd.go)、[AI analysis](../../cmd/hufu/explaincmd_ai.go)；不是跨歷史任務檢索 | 重用 provider 設定、模型選擇、bounded evidence 與取消機制；擴充共用服務，不複製整套分析執行器 |

本案遵循 [execution runtime](../architecture/execution-runtime.md)、[inspector](../architecture/unified-observability-inspector.md)、[operator experience](../architecture/operator-experience.md) 與 [文件權威順序](../README.md#authority-order)。本案需要明確擴充歷史結果與內容查詢契約；不能直接放寬現有 metadata-only inspect 的輸出。

## 2 使用流程與 CLI 提案

```bash
# 主要入口：自然語言搜尋並回答，不要求任何 run-id
hufu results query "昨天完成的 code review 有哪些重要發現？" --team <team>
hufu results query "之前處理 timeout 的任務，最後採用什麼解法？"
hufu results query "比較最近兩次部署檢查的結果" --team ops

# 可設定查詢模型；沿用 Hufu 已設定的 provider
hufu results query "上週有哪些任務仍然 blocked？" --model <provider/model>

# 離線操作；results 無子指令時等同 list
hufu results
hufu results list --limit 20
hufu results search "timeout" --agent helper
hufu results latest --agent helper
hufu results show <result-id>

# 所有查詢子指令共用 team 篩選，不必先查 run-id
hufu results list --team <team>
hufu results search "timeout" --team <team>
hufu results latest --team <team>
hufu results final --team <team>
hufu results final <result-id> --team <team>
hufu results show <result-id> --team <team>

# 精確查 invocation，列出其所有 run、任務與保存的 run response
hufu results invocation <invocation-id>
hufu results invocation <invocation-id> --team <team>

# 自然語言也可以接受 invocation ID，resolver 只產生精確 selector
hufu results query "整理 invocation <invocation-id> 的任務結果"

# 腳本與除錯
hufu results query "上週完成了什麼？" --output json
hufu results query "昨天的結果" --preview
```

`query` 本身即表示使用 LLM，不需要再加 `--ai`；`list`、`search`、`latest`、`show`、`final`、`invocation` 與 `--preview` 不會呼叫模型。`--preview` 只顯示呼叫者指定的範圍、模型與預算，不能聲稱已解析自然語言或預先顯示查詢命中。

`search` 是離線的 literal／lexical 搜尋，不替使用者解讀「昨天」「最近幾次」等自然語言。`latest` 明確取完成時間最新的一筆已完成任務；不是模型對「最符合問題」的判斷。

### 預設範圍與選擇

- `--team <team>` 是 `hufu results` 的共用 persistent flag，適用於 query、list、search、latest、show、invocation 與索引維護；可放在 results 後或子指令後。指定時先鎖定 team scope，再檢索／讀取內容／呼叫 LLM，不能只在最終顯示時才過濾。
- 未指定 workspace 時，使用 registry 定位目前 project，查該 project 已註冊且可讀的 active team workspaces；`--team` 可縮小範圍。project 無法唯一解析時，回傳選擇提示，不能任選 workspace。
- `--workspace` 是 exact override，只查指定 workspace；不自動搜尋其他 project、trash 或任意磁碟路徑。多 workspace 結果顯示所屬 team 與 scope。
- `--team` 以 registry 與保存的 canonical team identity 解析，不靠模型猜名稱或比對描述。未知 team、與 exact workspace 不相容的 team、或 show 的 result-id 不屬於所選 team 時，回報明確 scope error，不能自動改查別的 team。歷史結果的 team/agent 歸屬取自保存身份，不要求當年的 agent 定義仍然存在。
- 明確的 CLI team scope 高於自然語言 selector；模型若要求其他 team，回傳 scope conflict 或 clarification。JSON 回傳 effective team scope，文字顯示所屬 team；引用命令也保留 team selector。
- 每個 workspace 預設只查 active branch 的 lineage；`--branch` 或明確的 `--all-branches` 才擴大 branch 範圍。自然語言不能覆蓋 CLI 的授權範圍。
- 明確的 `list` 預設顯示已完成的個別 agent 任務；其他 terminal 狀態透過 `--status` 或 query 的已驗證 filters 指定。`runs` 列表顯示保存的最終 run response，保留其原始 outcome，不把完成與審查無 findings 畫上等號。無子指令時只讀最近一筆完整回覆正文。接受的 task status 必須引用 runtime 既有 enum，不能另造成功狀態。尚無 terminal event 的進行中任務由現有 status/explain 查詢，不創造尚未存在的結果。
- `--since`、`--until` 接受明確 timestamp，預設篩選 terminal event 的時間；相對日期由 LLM 解析，reference time 與 timezone 一起提供。JSON 回傳實際的半開區間 `[since, until)`、time basis 與 timezone。
- `--invocation`、`--run`、`--agent` 是可選的縮小條件；run-id 保留為進階診斷 selector，不是一般使用前置條件。
- 列表使用完成時間降冪，時間相同再以 workspace identity、event ordinal 與 result-id 排序；分頁 cursor 綁定 scopes、filters 與各 event chain head。

### 結果閱讀與引用

列表顯示完成時間、team、agent、狀態、任務描述及短摘要；摘要屬於結果內容，必須通過內容讀取權限與 redaction。`show` 顯示保存的 typed result，包括 summary、details、findings、decisions、structured payload 與 artifact references；缺少欄位就標示 unavailable，不補造內容。

`result-id` 是本計畫新增的單筆歷史結果定位碼，格式為 `res-...`，由 Hufu 自動產生並顯示。它識別一次有 canonical anchor 的保存結果；task-id 識別任務，run-id 識別一次執行，invocation-id 識別 CLI 呼叫。使用者不用自行組合這些身份，也不用在自然語言 query 中提供 result-id。

每一筆結果提供可複製的 `result-id`。`show` 可以接受唯一短前綴，但遇到碰撞或歧義必須列出候選；畫面上的列表序號不是可跨查詢保存的 ID。模型回答用 `[1]`、`[2]` 引用這次查詢的 source map，並附上 `hufu results show <result-id> --team <team>`。一般使用者可直接用 `query`、`latest` 或 `list` 找結果，result-id 主要供精確閱讀、引用與自動化使用。

`invocation` 顯示它在目前授權範圍內的所有 run 與 task，保留 team、branch、attempt 等關係。同時可顯示 `run_finished` 保存的各 run response。只有各 run response 時，不把重新聚合的文字冒充原始 CLI 最終輸出的完整副本。

### 執行期間顯示 result-id

result-id 依 terminal/result event anchor 產生，尚在執行的 attempt 不預先宣稱已有可讀結果。執行進度持續顯示 task-id、attempt、agent 與 team。每筆 terminal result 保存成功後，即時發布 result identity，不必等所有任務或整個 invocation 完成；失敗／blocked 記錄僅顯示實際保存的診斷內容，不表示已成功交付。

```text
Task 7 [team=<team>, agent=<agent>, attempt=1] done
Result ID: res-...
View: hufu results show res-... --team <team>
```

- 一般 terminal status 在該任務的完成通知附上 result-id 與閱讀指令，寫到既有 status transport，stdout 保持原 final-result 格式。
- TUI 的任務 detail 及結果區顯示完整 result-id、team 與閱讀指令；與現有欄位、overlay 優先順序和純 Update 契約一起更新。提供複製結果引用的操作。
- JSONL 增加版本化的 `task_result_ready` status event，包含 result_id、team、invocation_id、origin_run_id、task_id、attempt、branch_id 與 source_event_id，不包含完整結果內容。
- `--quiet`／`--no-summary` 等輸出抑制規則維持既有語意；不為 result-id 把額外狀態文字插入 JSON stdout。需要即時機器引用時明確使用 JSONL event transport，未顯示 identity 也可稍後以 results query/list 查回。
- 保存失敗不發布 result-ready event。resume、同一 canonical commit 的通知重送與 branch inheritance 必須維持同一 result-id；新的 attempt 結果則有新的 identity。

本項擴充 terminal/TUI/JSONL presentation，不讓顯示層產生或變更 runtime 完成狀態。JSONL consumer 以 event type 辨識 additive identity event；所有既有 event payload 與原 JSON final-result 契約保持相容。

## 3 歷史結果來源與保存契約

### Canonical source 與身份

以已驗證的 event chain、task transition payload、accepted typed result、execution receipt 及既有 artifact store 為 authority。重用 [LoadLineage](../../internal/inspect/source.go) 與既有 reducers，在已選定 lineage 內建立歷史結果 projection；session checkpoint、task journal、Markdown 與模型回答都不能自行建立完成事實。

result identity 必須由 runtime identity 決定，至少綁定 project/workspace identity、team、origin run/task occurrence、attempt 與 terminal/result event ID。不能只用可重複的 todo-id，也不能把 resume 的新 run-id 當成另一份已完成的同一結果。

對身份版本採固定序列化並雜湊成 opaque `res-...`。同一筆結果在 branch inheritance 中維持相同 result-id，另列出可見 branch；fork 後新 attempt／新 terminal result 具有新身份。短前綴僅供定位，完整 ID 與 event anchor 用於 JSON 和引用。

歷史投影保留完成當時的結果與後續 lifecycle：若任務之後因 on_failure、reset、remove 或其他 canonical transition 被撤回，仍可作歷史記錄顯示，但標示 `invalidated` 或 `superseded`，不能再次顯示為目前可接受的結果。有效性是獨立的 result lifecycle 欄位，不能覆寫 runtime task status。顯示任務成功也不代表整個 run acceptance 通過。

投影以每一個 canonical terminal occurrence 的狀態與結果為快照，再連結後續 lifecycle；不能只 replay 最新 TodoList 後按 todo-id 建表，否則被重置、移除或再次完成的舊結果會被覆蓋。

### 保存與索引

1. 先驗證所有執行路徑的 canonical terminal transition 是否完整保存 accepted typed result，特別是 details、structured payload、receipt 引用與 artifact hash。足夠時，直接 replay，不新增第二份結果 truth store。
2. 如果某類完整內容只存在可清理的檔案，補充不可變的 result artifact 與 canonical event reference。使用既有 artifact 寫入、hash、atomic commit 和授權機制；不得把 raw transcript 或完整資料任意塞進 event payload。
3. task completion 必須在 canonical result／reference 已 durable 後才可見。index 更新失敗不能抹掉成功的 canonical commit，也不能要求重跑 worker；read path 可以從 event chain 重建。
4. `--new`、cache expiry、task-journal compaction、session checkout 與結果索引重建不得刪除 canonical historical results。`--no-journal` 也不關閉 canonical event/result 保存。整個 workspace 被明確刪除或 purge 時，歷史結果隨其 lifecycle 處理。
5. 訊息只保存 reference 時，artifact retention 必須 pin 住歷史結果需要的內容。raw transcript 或外部檔案已不存在，就顯示 unavailable；本案不承諾保留任務讀取過的所有原始專案檔案。
6. 首版使用 snapshot-bounded、in-memory event projection 交付正確性。第二階段可在同一 `runtime.sqlite` 新增版本化 result lookup／全文索引，包含 source event ID/hash、chain watermark 與 projector version；索引完全可由 canonical sources 重建。
7. 查詢命令不 migrate、repair、重建或寫回 workspace。缺少／過期索引時使用有界 replay fallback 並回報狀態；資料規模超出 fallback 預算就明確要求縮小範圍或執行獨立維護指令，不把部分搜尋冒充完整搜尋。
8. 若提供 `hufu results index build`，它是明確的 projection 維護操作，與 query 分離；dry-run、atomic replacement、resume/failure recovery 及 SQLite schema migration 都納入驗收。

### 舊資料與缺失

舊 event 有 terminal typed result 時直接讀取，不需要先重新執行。缺少 invocation_id 的舊資料不能由 timestamp 猜測父 invocation。只有 journal／Markdown 而無可驗證的 canonical terminal anchor 時，可在明確的 legacy view 顯示來源與缺失，但不能混入 canonical 成功結果或作已驗證的回答依據。

已遭清理而且沒有任何 durable canonical result 的內容無法恢復。索引重建不是資料救援。JSON 與文字輸出必須區分 `missing_content`、`legacy_unverified`、`index_unavailable`、`partial_coverage` 與真正的零命中。

## 4 LLM 查詢流程

```text
使用者問題 + CLI scope + reference time/timezone
    ↓
LLM structured query resolver
    ↓ schema 與 scope 驗證
deterministic authorized retrieval
    ↓ bounded candidates + content references
LLM relevance selection
    ↓ 驗證被選 ID 都在候選集合
authorized result excerpts + source map
    ↓
LLM grounded answer
    ↓ 驗證引用與輸出契約
回答 + 任務來源 + 搜尋範圍/涵蓋程度
```

### 4.1 問題轉成 typed query

建立版本化 `HistoricalResultQuery` schema。模型只決定 intent、時間、status、agent/team 的縮小 selectors、有限 search terms、排序與結果數；scope 的上限由 CLI 和 registry 先決定。runtime 確認 selector 確實存在於允許集合後才查詢。

建議欄位如下，具體 schema 隨第一工作包定版：

| 欄位 | 契約 |
| --- | --- |
| `schema_version` | 固定版本，拒絕不支援的版本 |
| `resolution` | enum `resolved`、`needs_clarification`、`unsupported` |
| `intent` | enum `find`、`summarize`、`compare` |
| `selectors` | 可選 invocation/run/agent/team；只可縮小 caller scope |
| `time_range` | 可選 RFC3339 since/until 與固定輸入 timezone；驗證順序、範圍與 reference time |
| `statuses` | 歷史 terminal projection 支援的 runtime task status enum；不靠 prose 猜成功 |
| `status_basis` | enum `at_terminal`、`current`；區分「當時 blocked」與「目前仍 blocked」，後者須由截至 snapshot 的後續 transitions 證明 |
| `search_terms` | 最多 3 個、有長度上限的檢索文字；可由模型提供跨語言同義表達 |
| `order` | enum `relevance`、`newest`、`oldest` |
| `limit` | 有上下限的 integer；不能超過 caller query budget |
| `clarification` | 必要時的短問題與歧義原因，不包含模型生成的執行指令 |

禁止模型輸出 SQL、shell command、filesystem path、任意 tool name 或額外授權。嚴格拒絕 unknown fields、trailing JSON、過長字串、無效時間或 cross-field conflict。自然語言條件不可用 regexp、關鍵字表或中英文固定句型補救；模型不可用或解析失敗時回傳可理解的錯誤，提供離線指令，不執行猜測出的 query。

問題不明確時，TTY 可在一次查詢流程中呈現 schema-valid clarification；non-TTY 回傳 `needs_clarification` 及精確的重試方式。不能用「最近的任務」偷偷取代缺失的 selector。clarification 後重新解析是另一個明確 query attempt，各 attempt 的模型呼叫與預算都要呈現，不能以此迴避單次查詢上限。

### 4.2 候選檢索與 LLM 選取

先依已授權的 project/team/branch/time/status/selectors 篩選，再搜尋任務描述、summary、details 及可授權的文字結果欄位。候選檢索有界；中文、英文與 identifier 都必須有品質 fixture。第一版使用 Unicode literal/lexical matching 與模型生成的有限 search terms，不能只以空白切詞或 ASCII FTS 宣稱支援中文。

新增 semantic embeddings 並非 LLM 查詢的前置條件。首版仍然包含 LLM intent resolution、候選相關性選取與結果回答；vector retrieval 待 lexical + LLM 基線通過後另評估，不能直接借用 context memory index 混合不同 authority 與 retention 的資料。

候選上限預設 50，供模型選取的 excerpts 預設最多 20 筆；最後回答預設最多引用 10 筆。LLM selection 回傳候選 ID 與 relevance reason，不生成可執行 selector。不存在、超出範圍或重複的 ID 都拒絕，不能反過來查詢模型臆造的任務。

retrieval 回傳 matched count、scanned scope、index state、truncation、source chain heads 與 coverage。查詢涉及「全部」「有多少」時，count 由完整 filter 集合的 deterministic aggregation 產生；若內容 predicate 只由模型檢查部分 candidates，就只能回報 sample findings，不能宣稱全域數量。

### 4.3 依來源回答

回答器只能使用這次授權並檢索到的結果 excerpts，回傳 typed answer blocks：claim text、source IDs/field references，以及 facts／inference／unavailable 的分類。CLI 產生 citation map，不接受模型自行編造 URL 或任務路徑。沒有命中時不呼叫 answer LLM。

例如「採用什麼解法」可引用任務 decisions/details；「是否已修復」需要結果或後續驗證明確記錄。只有較新的成功任務，不能依時間順序推論已修掉某個舊 finding。比較問題保留不同結果及矛盾，不把多個任務寫成同一個 execution。

runtime 可驗證引用確實存在、field/excerpt 屬於該結果與該 snapshot，以及 ID 未超出 scope；這不能完全證明自然語言 claim 的語意正確。以限定來源、引用、對抗 fixture 與評估揭露模型推論的限制，不把 citation validation 宣稱為完整事實驗證。

### 4.4 模型邊界與預算

- 沿用已設定 provider 與 model metadata/admission；優先 CLI `--model`，單一 team 時沿用該 team 的 sidecar/model precedence，多 team 時使用明確設定的共用/global analysis model，無可唯一解析的模型就回報設定方式。
- 查詢模型沒有 bash、MCP、agent delegation、write、resume、retry 或 finish 等工具；不載入可執行 team coordinator。歷史內容與使用者 query 都以資料邊界輸入，不能改寫權限與系統指令。
- 明確的 `query` 授權把範圍內的 redacted 任務描述與結果 excerpts 傳給解析後的 provider；preview 顯示 provider/model 與內容種類。不自動切換到另一個遠端 provider，不傳整個 workspace 或無關 transcript。
- 先對保存的 canonical bytes 驗證既有 hash，再建立獨立的 redacted 顯示／模型 projection；不把新的 redaction 結果寫回已 sealed 的 receipt、runtime outputs 或 artifacts，也不據此改寫 canonical hash。
- 預設最多 3 次 LLM generation：resolver、relevance selection、answer 各一次；無無限 agent loop 或隱藏修復重試。總 wall time 建議 60 秒，最終 evidence input 建議 8K tokens、answer 最多 2K tokens，仍受實際模型正 context capacity 與 admission 上限限制。
- 同一查詢使用獨立 query identity 與歷史 snapshot anchors，不能把分析 token/cost 寫入舊 run 的帳，不能覆蓋原 invocation_id 或 session checkpoint。
- timeout、Ctrl+C、schema error、provider unavailable、token budget 超限都終止有關查詢呼叫並回報原因；離線列表與閱讀仍可用。

## 5 輸出與相容性

JSON 使用版本化 envelope，包含 `schema_version`、`kind`、resolved scope、validated query、operation、integrity、coverage、results、answer/source map、model/usage 及 diagnostics。partial coverage、內容缺失及 LLM failure 是可供腳本判讀的不同欄位，不只塞進 prose。

成功取得投影時回傳 0，包含明確的零命中；canonical integrity failure 回傳 1；無效/歧義/未授權 scope 回傳 2；LLM 不可用、timeout 或回答契約失敗使用獨立 analysis failure code 3。分析失敗時可以同時回傳已找到的 deterministic candidates，但不可標示為成功回答。

現有 `hufu history`、`inspect`、`explain` 與 JSON schema/exit codes 不變。本案若新增 inspect projection kind，必須另外版本化與回歸驗證；不能讓既有 `inspect task` 默認輸出完整內容。

所有查詢保持 workspace read-only：不改 branch、session、task status、journal、projection、migration 或 historical events。允許 query 按已配置 provider 呼叫 LLM；任何索引維護／schema upgrade 都是另一個明確操作。

## 6 分階段交付

| 工作包 | 交付內容 | 完成條件 |
| --- | --- | --- |
| HRQ 00 來源與契約 | inventory normal/direct/nested/extra-model/action/resume 路徑；定義 result identity、scope、legacy coverage、query/answer schema 與 CLI 契約 | 缺口有可重現 fixture；完成與撤回結果能從 canonical sources 區分；不建立 competing truth store |
| HRQ 01 歷史讀取與保存 | event projection、必要的 canonical result reference 補強、team-scoped list/show/latest/invocation、JSON/redaction、terminal/TUI/JSONL result identity | 不需 run-id 即可查歷史；每筆結果保存後即時顯示 ID；跨 invocation 多 run 正確；--new/no-journal/journal compaction 後仍可讀可驗證的結果 |
| HRQ 02 LLM 自然語言查詢 | structured resolver、Unicode candidate retrieval、bounded relevance selection、grounded answer/source map、provider/預算/clarification | 中文自然問題直接取得結果；參數不猜測；scope 不擴大；回答引用實際保存的結果 |
| HRQ 03 索引與性能 | 若 benchmark 需要，新增可重建索引與明確 build 操作；cursor/snapshot、一致性與 crash recovery | index/replay 結果一致；查詢不自行寫入；partial/stale/missing index 真實顯示；性能報告可重現 |
| HRQ 04 文件與發布 | 使用指南、generated command metadata/completion、operator reference、回歸與評估報告 | 指令範例可重現；既有 CLI 相容；文件、tests、vet、lint gates 全部通過 |

HRQ 02 是本案必要交付，不能把離線列表完成當成整個改善完成。HRQ 03 的索引設計以 HRQ 01/02 的 benchmark 為依據；無性能需要時明確保留 replay 實作與量測證據，不為了索引而新增 schema。

建議責任分界：`internal/inspect` 負責已授權的 historical projection 與 result binding；`internal/team` 僅處理確有缺口的 canonical result durability/reducer；`internal/runtimestore` 處理必要的 schema/index；共用 analysis service 承接 provider 邊界與 typed query/answer；`cmd/hufu` 負責 Cobra flags、格式、clarification 與導覽。不加入具體 team 名稱、review 專用 severity 判斷或 per-team launcher。

## 7 驗收矩陣

| 案例 | 必須觀察到的行為 |
| --- | --- |
| 不知道任何 ID，查「昨天完成的任務」 | 以固定 clock/timezone 得到正確區間與歷史結果；清楚列出 scope |
| 問「之前的 timeout 解法」但保存文字是中文同義表達 | fixture oracle 能召回並引用正確 decisions/details，不能只通過 ASCII 搜尋 |
| 一個 invocation 包含多 team、多 run | exact invocation lookup 與 LLM lookup 都包含授權範圍內全部關聯結果，不能偷取最後一個 run |
| 每個 results 子指令指定 --team、未知 team、result-id 與 team 不符 | 正確篩選與拒絕 scope conflict；不讀其他 team 的結果內容，也不把它傳給 LLM |
| 問題要求另一個 team，但 CLI 已指定 --team | 明確 conflict/clarification，LLM 不覆蓋 CLI scope |
| 一個任務完成但整個 invocation 還在執行 | terminal/TUI/JSONL 立即顯示相同 result-id；可用所示 team-scoped show 指令讀回該結果 |
| terminal result 保存失敗、quiet/JSON 輸出、resume 通知重送 | 未 durable 就不發布 ready；stdout 不污染；重送身份穩定，task status 不由顯示層偽造 |
| task-id 在不同 run/session 重複、同任務 retry/resume | ID 無碰撞，winning result 與失敗 attempt 分開，resume 不重複算成果 |
| 完成後被 canonical transition 撤回 | 歷史內容仍可讀，但有效性與最新 lifecycle 正確，回答不宣稱仍被接受 |
| fork inheritance 與 sibling branches | inherited result 去重、new occurrence 分開；預設不跨 sibling，all-branches 顯式生效 |
| --new、no-journal、cache expiry、journal >5 MiB | 已保存 canonical 結果可查；不得依賴 task Markdown 或 cache entry 還在 |
| 缺少 typed result／舊 invocation_id／artifact 消失 | 明確顯示缺失，不填造 ID、內容或驗證狀態 |
| LLM schema 錯誤、時間倒置、未知 ID、scope 擴大、SQL/path 注入 | query validation 拒絕；不執行模型產生的 action 或越權讀取 |
| 歷史 result 含提示注入或 secret | 不變更工具/授權，redaction 後才送模型與輸出；精確 raw payload 不被重寫以致 hash 失效 |
| 問「全部 high findings 是否已修」而只取到部分候選 | 回報 partial coverage；沒有明確修復/verification 證據就說 unavailable |
| 結果為 0、provider 不可用、timeout、Ctrl+C | 零命中與分析失敗區分，呼叫有界且可取消，離線命令仍可用 |
| index 損壞／落後、查詢期間 writer 完成新任務 | 綁定一致 snapshot，index/replay 不混資料；不偷偷 migrate 或修復 |
| 查詢前後比較 workspace | runtime/context DB、event chain、session、branch 與 project artifacts 沒有查詢造成的修改 |

品質評估建立至少 30 個中文／英文固定問題，覆蓋 find、summarize、compare、日期、跨語言同義表達、零命中、歧義與不足證據。fake provider 驗證 deterministic 契約與故障路徑；另以固定本地模型做端到端自然語言品質評估，記錄模型版本、問題集、oracle 與完整輸出。若本地模型不可用，保留品質評估未完成的狀態，不能以 fake provider 假裝通過語意品質。

建議 release gate：scope violation 與 invented source ID 為零；deterministic 時間/身份/保存 fixture 全部通過；find 題正確結果進入最終 top-10 的 recall 至少 90%；由 oracle 判定的 answer facts/source support 至少 95%；不足證據題全部明確回報缺失。這些是待驗收門檻，不是目前已達成的成果。

性能以 1K、10K、100K terminal results 的固定資料集量測，報告 index build、完整 chain validation、candidate search、memory、cold/warm query、LLM latency 與 tokens。單獨報告 query search 的 p95 與全流程耗時，不能排除 integrity validation 後宣稱整體低延遲；記錄環境後再定 SLO。

實作時按變更執行 boundary unit/integration、故障注入與受影響 race checks，並完成 `go test ./...`、`go vet ./...`、`golangci-lint run` 與 `bin/check-docs`。Go 修改使用 Modern Go Guidelines；runtime 修改使用 `harden-hufu-runtime`；TUI presentation 使用既有 behavioral specification 與 tui-integration-testing，驗收依 behavior matrix，不只測 implementation helper。

## 8 交付完成定義

一般使用者能在目前 project 直接執行 `hufu results query "之前處理 timeout 的結果是什麼"`，取得符合問題的歷史任務、保存的具體結果、引用及涵蓋程度，全程不必先找 run-id。模型不可用時，仍可用列表、精確搜尋與 show 閱讀已保存結果。

同時保證查詢不改變 execution state、模型不能擴大讀取範圍、完成狀態取自 canonical sources，且歷史資料不因 session refresh 或 cache compaction 消失。既有永久資料已缺失時如實回報，不能由 LLM 補造過去發生的事。
