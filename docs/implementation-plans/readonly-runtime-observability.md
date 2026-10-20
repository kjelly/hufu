# Hufu 唯讀路由分析、成本統計與工具整合實作計畫

> **狀態**：Ready for implementation；從 WP-00 開始，依依賴順序完成 WP-01～WP-05。
> **修訂日期**：2026-10-20
> **核對基準**：`kjelly/hufu`，HEAD `3715d61001b9122a3828e3202487f7446598f894`。
> **交付性質**：本文件列出尚待實作的功能與驗收契約，不代表功能已存在。全部工作可由 Coding Agent 使用本機程式碼、合成 fixture 與離線測試完成。
> **使用者確認的驗收範圍**：本次不含真實試跑；日後另立任務。
> **文件位置**：依使用者指示移入 `docs/implementation-plans/readonly-runtime-observability.md` 並納入版本控制；每完成一個工作包，驗證後獨立提交。

## 1. 交付目標與範圍

交付一套能回答以下問題的唯讀工具：

1. 指定 run 的 worker 實際用了哪些 route、target、attempt，以及何時 retry／fallback？
2. 哪些任務有可核對的完成證據，哪些仍是未驗證、失敗或資料不足？
3. 這個 run 的生成成本、失敗成本與未結算用量是多少？哪些金額仍未知？
4. 既有 CLI／TUI 與本機 MCP client 能否讀到一致、具範圍限制的資訊？
5. 指定團隊有哪些已宣告的技能與 action provider，還缺少哪些可靜態判定的依賴？

預期效益是縮短排錯與人工選模型的時間、讓失敗及重試成本可見、減少外部工具解析 CLI 文字的需求。本計畫的完成條件是工具的正確性、相容性與可重播性；不以真實模型品質提升、節費比例或正式環境採用率作為驗收。

### 1.1 範圍邊界

以下均不屬於實作工作：自動／學習式選路、線上 shadow 推論、bandit、路由政策 promotion、真實 provider 對照試跑、跨模型實驗 runner、MCP 寫入操作、HTTP transport、多租戶服務、套件簽章／marketplace、統計式異常偵測、新資料庫或新的 runtime scheduler。不要為這些項目新增旗標、設定、佔位介面或 TODO。

本計畫不需要帳號、API key、付費模型、真實使用者任務或人工審核資料。測試資料由 Coding Agent 在測試暫存目錄建立。真實使用資料的成效驗證屬於另一個使用者任務，不是本文件的前置工作或未完成項。

## 2. 已核對的現況與重用入口

以下連結均相對於本文件。開始工作時再次記錄 HEAD；若已前進，以實際程式與既有測試校正入口，不重做已存在的能力。

| 領域 | 現有實作與限制 | 本計畫的新增內容 |
|---|---|---|
| Execution route | [execution_route.go](../../internal/team/execution_route.go)、[execution_route_admission.go](../../internal/team/execution_route_admission.go)：team load 時綁定；task admission 固定 route；1–4 個 ordered LLM targets | 只投影既有 route 與實際 attempt；不修改選路 |
| Backend 類型 | `executionRouteCandidate` 拒絕 agent backend；Codex agent backend 不能當成現行 execution-route candidate | 非 route worker 仍可列示實際 target；不得為報表放寬 backend 限制 |
| Retry／fallback | [execution_fallback.go](../../internal/team/execution_fallback.go)、receipt 記錄 candidate index 與 failure class | 重用既有事件語意；retry 與 fallback 分開計數 |
| Snapshot | [execution_policy_snapshot.go](../../internal/team/execution_policy_snapshot.go)：v5，保留 v3／v4 read compatibility | 不修改 canonical snapshot bytes、版本或 resume 行為 |
| 唯讀 inspector | [binding.go](../../internal/inspect/binding.go)、[source.go](../../internal/inspect/source.go)、[run.go](../../internal/inspect/run.go) | 重用 exact scope、validated lineage、error envelope |
| Worker attempt | [workers.go](../../internal/inspect/workers.go)：已有 attempt identity、target、route name、retry／fallback 與 usage 投影 | 補 route digest、證據狀態及分組統計；不另寫競爭中的 attempt reducer |
| 完成證據 | [explain.go](../../internal/auditverify/explain.go)、[witness.go](../../internal/auditverify/witness.go)、[run_result.go](../../internal/team/run_result.go) | 從持久化證據產生診斷；不得執行 verifier 或改寫 outcome |
| 成本 | [cost_view.go](../../internal/team/cost_view.go)、[view.go](../../internal/cost/view.go)、[cost.go](../../internal/inspect/cost.go) | 新增 derived economics；沿用唯一 reservation／settlement ledger |
| 操作介面 | [types.go](../../internal/operator/types.go)、[actions.go](../../internal/operator/actions.go)、[tui.go](../../internal/tui/tui.go) | 延伸既有 Snapshot／panel，沿用非同步查詢與 generation 防護 |
| 實驗框架 | [paired/config.go](../../internal/improve/paired/config.go)、[materializer_test.go](../../internal/improver/materializer_test.go) | 現有受控比較主要允許 prompt／skill body 變更，會拒絕 model 變更；本計畫不擴充此框架 |
| MCP | [agent_server.go](../../internal/mcp/agent_server.go)、[manager.go](../../internal/mcp/manager.go) 是內部工具與 client；未找到公開 run 查詢 server | 增加獨立 stdio 薄轉接層，不直接公開內部 worker tools |
| 團隊診斷 | [teamcheckcmd.go](../../cmd/hufu/teamcheckcmd.go)、[teampkg/inspect.go](../../internal/teampkg/inspect.go)、[mcp/descriptor.go](../../internal/mcp/descriptor.go) | 新增靜態能力清單；沿用既有 compiler、lint、descriptor hash |
| 套件信任 | [teampkg/archive.go](../../internal/teampkg/archive.go) 只接受 `authenticity: unverified`，既有 hash 驗證的是完整性 | 清單必須保留 integrity 與 authenticity 的區別，不宣稱有簽章驗證 |

實作前閱讀以下權威文件的相關章節：

- [Execution runtime](../architecture/execution-runtime.md)
- [Run outcome](../architecture/run-outcome.md)
- [Cost governance and portable team packages](../architecture/cost-governance-and-portable-team-packages.md)
- [Operator experience](../architecture/operator-experience.md)
- [MCP worker policy](../reference/mcp-worker-policy.md)
- [Action providers](../reference/action-providers.md)
- [Skill discovery](../reference/skill-discovery.md)

## 3. 共用契約

### 3.1 唯讀資料流與 ownership

```text
runtime.sqlite 的 validated event lineage + 持久化 receipt／evidence
    → internal/inspect 範圍綁定與既有 replay
    → route／economics 純投影
    → CLI／Operator Snapshot／TUI／stdio MCP

團隊定義與既有 skill／action provider metadata
    → 現有 compiler／lint／discovery
    → 靜態 capability 清單
    → team capabilities CLI
```

- 新分析功能的組裝與純投影放在 `internal/inspect`；只有真正共用的 cost 解碼／投影 helper 才放在 `internal/cost` 或既有 `internal/team/cost_*.go`。
- 不建立 analytics DB、不回寫 runtime event、不開啟 Coordinator、不啟動 provider、worker、MCP child server、verifier、migration 或 recovery。
- 不新增自然語言分類器。分組只用已持久化的 agent、contract、route、target、team／policy revision；缺欄位列為 unavailable。不得從 prompt 關鍵字猜 task family。
- 若缺少可公開重用的唯讀 helper，可以抽取現有邏輯並保留原測試；不可複製另一套 outcome／授權／成本判定。
- 新 JSON 投影使用自身 `schema_version: 1`，既有 envelope 欄位及數值語意不變；新增 optional 欄位須保留舊資料的讀取能力。
- 所有新增固定詞彙使用小寫 snake_case；出現在 tool 參數時必須宣告 enum。既有 canonical enum 原樣輸出，不改名。

### 3.2 Scope、identity 與重播

每次查詢綁定一個 workspace、session、branch、run；CLI 解析後使用 canonical ID。既有 integrity validator 可以讀同 workspace 的全域 event chain 以驗證完整性，但結果與統計只能使用選定 lineage；不得輸出 sibling branch 的內容、掃描其他 workspace，或將 active session 的最新結果當歷史 run 的結果。

資料單位分兩層：

- **Task row**：選定 lineage 中一個 durable task occurrence。保留原始 task-created event ID、origin run、task ID；retry／resume 的 reset 事件不建立新的統計任務。跨 run 延續但無法證明完整範圍時標記 `cross_run_scope`，不猜測完整生命週期成本。
- **Attempt row**：重用 `WorkerAttemptView.AttemptKey` 與 started event ID，另保留 run ID、task ID、occurrence attempt、dispatch attempt、model execution ID、receipt hash。不能只用 `(task_id, attempt)` 去重，因為 dispatch attempt 會重設，extra-model 也可能並行。

保留查詢 branch 與來源 event branch 的區別。繼承自祖先 branch 的相同事件在一次查詢只計一次；不同查詢的報表不自動加總。event 重複、hash chain 損壞、scope 衝突或相同身份內容矛盾，沿用 inspector integrity／scope error，不能跳過壞資料產生看似完整的結果。

相同 validated input 與同一 projection version，必須產生相同語意 JSON。排序使用來源 event ordinal、task identity、attempt identity；不得依 map iteration。運行中 duration 另列且不進固定 latency 統計，重播測試不依現在時間。

### 3.3 證據與成功的表示

原始 `task_status`、`run_outcome`、`acceptance_state` 分欄保留。新增 `completion_evidence` 是分析資格標記，enum 為 `verified`、`not_verified`、`unavailable`，不取代 runtime outcome。

`verified` 的最小條件：task 為 canonical done；可唯一定位與完成證據綁定一致的 winning 成功 receipt；有已持久化的客觀 verification passed；適用的 result-contract、invariant、artifact／evidence binding 均可由既有唯讀 validator 核對。多個歷史成功 receipt 不必然是衝突，但 winner 必須明確。若資料不足以判定某條件是否適用，使用 `unavailable`。只有 model 自述、exit 0、schema valid 或整個 run completed 都不充分。

沿用 `ExplainLineageRun`、既有 receipt／binding validator；如現有 API 僅能回報 run 層級，抽取不執行 recheck 的 task 證據 helper。找不到客觀驗證是 `not_verified`；必要的歷史資料缺失是 `unavailable`；證據互相矛盾則回 integrity error。failed／blocked／skipped 任務不能進 verified 分子。

唯讀查詢只核對已存紀錄、hash 與 artifact refs，不重跑 shell verifier，也不改寫 receipt、secret redaction 或 completed 狀態。

### 3.4 成本與 latency 定義

金額採 integer USD micros；成本類別沿用現有 `usage`、`admission_bound`、`not_metered`、`subscription`、`unknown`。`usage` 是價格表乘用量的估算，不稱為帳單實付。未知不得填零。

| 指標 | 固定計算規則 |
|---|---|
| `task_count` | 選定 run 新建或有持久化狀態變更的 distinct task occurrences；包括失敗、取消、零 attempt 的跳過與未完成，另列各狀態數。只從祖先繼承且在本 run 無變更的 completed task 另列 reused，不進本分母／分子 |
| `verified_task_count` | 上述 task 中 `completion_evidence=verified` 的 distinct 數；同 task 多 attempt 最多計一次 |
| `verified_task_rate` | verified_task_count / task_count；分母 0 時為 null。unavailable 仍留在分母並另列 coverage，避免只挑成功樣本 |
| `usage_cost_micros` | canonical ledger 中已結算、來源為 usage 的生成成本總和；含失敗呼叫與 retry／fallback，不含未結算 reservation |
| `cost_per_verified_task_micros` | 選定 run 的全部 usage_cost_micros / verified_task_count；向上取整。只有 terminal run、完整已知計價與 run 歸屬、無跨 run task 成本缺口、verified_task_count > 0 時可提供，否則 null + reason。retry／fallback 細分不可用本身不妨礙完整 run 總額 |
| `known_cost_coverage` | 已結算且來源為 usage 的 invocation 數 / 所有 reservation 對應 invocation 數；沒有 ledger 或分母 0 時 null。明確回報這是「已記帳 invocation」的 coverage，不能證明舊 run 沒漏記呼叫 |
| `retry_cost_micros`／`fallback_cost_micros` | 僅累計可唯一連到相應執行的 invocation；採互斥分類：fallback 優先，其次 retry，再其次 initial。無法唯一歸因列入 unassigned，相關完整總額為 null |
| `p50_attempt_latency_ms`／`p95_attempt_latency_ms` | 完成且有合法起訖時間的 attempt duration，按 nearest-rank 計算；空樣本為 null。附 sample_count／excluded_count；不是使用者端完成延遲 |

成本以 provider invocation ID 去重，並保留 run／task／occurrence 身份。不得把 receipt token 和 ledger token 再相加。coordinator、guard、judge 等共用成本列為 run overhead，不任意平均灌入某 route；它們仍納入 run 層級每件完成任務的成本。

若現有 cost event 只能連到 occurrence，無法區分其內部 retry／fallback，不增加 runtime telemetry 來補造歷史；提供 occurrence 總額與 `attribution_unavailable`。route／target 分組只展示直接可歸因成本，明示不含 overhead，不宣稱組間差異是因果節省。

完整計價資格還須證明該 run 的 frozen cost policy 啟用記帳，以及執行範圍沒有未記帳的生成路徑；不能僅因現存 reservation 全結算就當完整。無法證明時保留已知小計，完整指標為 null。integer 加總溢位、負數或非有限比例回 validation error。

共用 reason codes：`no_samples`、`missing_receipt`、`missing_verification`、`verification_failed`、`evidence_unavailable`、`unknown_cost`、`open_reservation`、`attribution_unavailable`、`cross_run_scope`、`run_in_progress`、`historical_coverage_unknown`。這些表示資料狀態，不觸發任何 runtime 操作。

## 4. 工作包與依賴

```text
WP-00 契約 fixture
  ├─ WP-01 route／attempt 唯讀投影
  │    └─ WP-02 economics 與資料完整性診斷
  │         └─ WP-03 CLI／TUI 整合
  │         └─ WP-05 固定 scope 的 stdio MCP
  └─ WP-04 靜態能力清單
```

WP-04 與 runtime 分析互不依賴。WP-05 只依賴 WP-01／02 與既有 inspector，不需要 WP-04。工作包是可單獨 review 的變更邊界；全部完成才代表整份計畫完成。

### WP-00：鎖定既有語意與建立離線 fixture

**Owner／範圍**：`internal/inspect/*_test.go`、`internal/inspect/testdata/`；必要時補現有 `internal/team/*execution*test.go`、`*cost*test.go`。

**工作**：

1. 重用現有 route、fallback、cost、branch、audit tests；記錄已覆蓋與缺少的契約，不為同一 assertion 重複建測試。
2. 以既有 event writer／test helper 建立有效 hash chain、task transition、receipt、cost events、artifact evidence；資料只在 `t.TempDir()`，不讀使用者 workspace 或真實 secrets。
3. 最小案例：verified task、done 但缺客觀驗證、verification failed、provider fallback、一般 retry、retry 與 fallback 同時存在、unknown／subscription／local cost、open reservation、跨 branch 繼承、resume／跨 run、extra-model、多個同號 dispatch attempt、損壞事件。
4. fixture 有明確預期計數與金額；至少一例失敗後成功、一例所有 task 都未驗證，證明成本分子不漏失敗費用，分母 0 不除零。
5. 建立唯讀檢查 helper：在靜止的 fixture 上比較查詢前後資料庫內容、schema、event head、目錄內容；檢查不存在 workspace 不會被建立。外部執行入口使用 spy／fake，任何呼叫立即使測試失敗。

**完成條件**：fixtures 可離線生成與重播；原有路由／snapshot／resume 測試通過；後續工作不依賴真實模型或人工標註。差異紀錄與 baseline 結果寫入同目錄的 `readonly-runtime-observability-progress.md`。

### WP-01：route／attempt 唯讀投影

**Owner／範圍**：`internal/inspect/` 新增 `routes.go` 與測試；延伸 `types.go`、既有 worker projector；`cmd/hufu/` 新增 inspect routes wiring／tests。

**公開介面**：

```text
hufu inspect routes <run-id> --workspace <path> [--branch <id>] [--session <id>] [--task <id>] --output text|json
```

1. 新增 `KindRoutes` 與 `InspectRoutes(ctx, query)`，沿用 `InspectQuery`／`Envelope`／既有 exit-code mapping。run 必填；task 可選；拒絕 attempt、all-branches、project／team／agent filters。
2. 一次綁定 scope、讀取 validated lineage；pure projector 接收同一份已驗證資料。需要追加讀取 artifact 時沿用既有 CAS／scope validator。
3. 輸出 scope、source event head、task rows、attempt rows、route／target groups、evidence coverage 與 diagnostics。新增欄位依 §3，不輸出 prompt、工具 arguments、transcript、原始 error text 或環境變數。
4. route digest 取自 durable occurrence；actual target 取自該 attempt receipt／既有 lifecycle projector，不把 frozen primary 當每次的實際 target。無 route 的 worker 以 `route_kind=unbound` 分桶；route-bound 為 `named`；歷史資料不足為 `unavailable`。
5. 同一 task 可以涉及多 target：各 target 的 attempt 分組保留嘗試數；成功只連到唯一被證據綁定的完成 attempt，不能把一次完成 credit 給所有曾參與模型。無法辨識 winner 則不產生 target success credit。
6. 不新增推薦分數、排序選路、跨 run 匯總或 task prose 分類。

**完成條件**：重播兩次 JSON 語意相同；branch／resume／extra-model 不重複 credit；CLI text／JSON 計數一致；所有錯誤情境都不修改 workspace。

### WP-02：生成成本、完成證據與完整性診斷

**Owner／範圍**：`internal/inspect/` 新增 `economics.go` 與測試；必要的既有 cost pure helper；`cmd/hufu/` inspect economics wiring／tests。

**公開介面**：

```text
hufu inspect economics <run-id> --workspace <path> [--branch <id>] [--session <id>] --output text|json
```

1. 新增 `KindEconomics` 與 `InspectEconomics(ctx, query)`；只接受 workspace／session／branch／run，避免把局部 task 成本標成 run 全成本。
2. 重用 WP-01 task／attempt 投影與 canonical cost reducer，以同一 event head 組裝 §3.4 指標、overhead、directly-attributed route totals、known subtotals、coverage、null reasons。
3. 原有 `inspect cost`／`cost.View` 的欄位語意不變；新衍生指標放在 economics DTO，不把已知小計改造成完整金額。
4. 診斷只含可直接證明的事實：open reservation、缺 receipt／verification、cost unknown、歸因不足。不要建立需要人選門檻的 burn-rate／retry-spike／stalled anomaly 演算法。
5. 每份輸出有 `comparison_scope=single_run_observed`；route／target 表格是觀測分組，不能輸出最佳路由、promotion eligible、品質非劣或節費建議。這不影響呈現 WP-01 已存在的 evidence-bound winning attempt。

**完成條件**：至少涵蓋失敗費用、未知計價、部分記帳、重複 settlement、重播、沒有 ledger、所有任務未驗證、overhead、跨 run、無法分配 fallback cost、overflow。各小計可對回 canonical ledger；fixture 的完整範圍能產生非 null 指標，避免全部回 unknown 的空實作。

### WP-03：既有操作介面的呈現

**Owner／範圍**：`internal/operator/`、`internal/inspect/overview.go`、`internal/tui/` 既有 operator panel 與 reporter wiring、`cmd/hufu/` 既有摘要 renderer 與測試。

1. 在既有 Operator Snapshot 加 optional route／economics summary，使用 presentation-only DTO。不得讓 `internal/operator` import `internal/inspect` 造成循環；在 inspector／reporter 組裝時轉成 DTO，數字不在 renderer 重算。
2. summary 只含 route／target 分布、verified／unavailable 數、已知生成成本、coverage 與主要資料缺口；細節導向現有 inspect 命令，不另建 dashboard。
3. 各欄位帶共同 event head；一次組裝的 route、outcome、cost 必須來自同一 validated input。不同快照不得拼成「一致」數字。
4. 維持 TUI Update 純函數；I/O 透過現有非同步命令，繼續用 generation 拒絕舊 scope 回覆。新讀取失敗顯示 unavailable，不覆蓋 canonical task／run outcome。
5. 小終端、無色、plain、epaper 都能辨識數值與 unknown；不增加 provider 呼叫、動畫、常駐計時告警或按鍵 mutation。

**完成條件**：同一 fixture 的 CLI／JSON／TUI summary 與原始投影一致；測試 resize、無 ANSI、舊 generation、scope 切換與讀取錯誤。舊 Snapshot 缺新欄位仍可 render。

### WP-04：靜態能力清單

**Owner／範圍**：沿用 `internal/team/` 靜態 compiler／lint、`internal/skill/` discovery 與 `internal/mcp/descriptor.go`，增加 metadata projector；`cmd/hufu/` 新增 team capabilities 命令與測試。

**公開介面**：

```text
hufu team capabilities <team> --output text|json
```

1. 沿用 team discovery／skill precedence；只解析團隊與本地已存在的 metadata，不啟動 coordinator、provider、MCP server、不建立 workspace、不安裝 package。
2. 固定 descriptor 欄位：`schema_version`、`id`、`name`、`origin`、`owner`、`content_digest`、`availability`、`declared_tools`、`declared_grants`、`effect_class`、`diagnostics`。清單按 `(origin, owner, id)` 穩定排序。
3. origin enum 為 `skill`、`action_provider`、`mcp_declaration`。package 不是另一個可執行能力類型；已安裝團隊沿用原始能力類型，套件 provenance 只有資料存在時才附加。archive inventory 繼續使用既有 `team package inspect`。
4. availability enum 為 `available`、`draft`、`unavailable`、`unknown`；`available` 只表示本地定義可讀且靜態有效，不代表 runtime 授權或 remote server 可用。draft 技能不因清單列示而進入 runtime discovery。
5. declared_tools／declared_grants 保留來源聲明；effective authorization 仍由既有 runtime gate 決定。缺資訊使用 null／unknown；不得將 skill allowed-tools 直接當授權。
6. effect_class enum 為 `none`、`read_only`、`workspace_write`、`external`、`unknown`；只從既有已驗證 descriptor／policy 的明確對應推導，shell 指令與自然語言描述不做語意猜測。
7. hash 重用既有 descriptor hash；缺少 canonical hash 的本地技能可對正規化 metadata 與 body 計算有 version/domain 的 content digest，輸出不含 body。mtime 不參與 digest；digest 不代表簽章或作者可信。
8. 重用 `team check`／compiler 既有 collision、schema、grant 診斷。正常 precedence 遮蔽只列來源，不誤判為錯誤。無法離線確定的 remote tool schema／grant 顯示 unknown，不連線補查。

**完成條件**：同名技能 precedence、draft、缺失檔案、remote MCP unknown、已知／未知 effects、重複名稱與 secret redaction fixtures 通過；舊 team 格式不需改寫。JSON 不含 endpoint、command arguments、環境變數或使用者絕對路徑。

### WP-05：固定 scope 的唯讀 stdio MCP

**Owner／範圍**：新增 `internal/mcpserver/` 薄轉接套件與測試；`cmd/hufu/` 新增命令；重用目前 `go.mod` 的 MCP SDK。不改內部 `AgentMCPServer` 的角色。

**啟動介面**：

```text
hufu mcp serve --workspace <absolute-path> --session <session-id> --branch <branch-id> --run <run-id>
```

四個值必填。啟動前用 exact workspace resolver 與 `BindReadTarget` 驗證並固定 canonical scope；branch 必須是 ID，不接受會變動的 name／label。缺資料、歧義或 scope conflict 即退出，不進 picker、不讀其他 workspace。不要求使用者安裝任何 host 才能完成驗收。

**授權模型**：單一 OS 使用者啟動的本機 stdio process。啟動參數是讀取範圍的上限；client 不能傳入新的 workspace／session／branch／run，也不能擴大成 all-branches。這不是多租戶授權系統。

| MCP tool | 參數 | 輸出來源 |
|---|---|---|
| `hufu_run_status` | 空 object | 固定 scope 的 `InspectRun` metadata |
| `hufu_run_routes` | optional `task_id` | WP-01；task 必須屬於固定 scope |
| `hufu_run_economics` | 空 object | WP-02 |
| `hufu_capabilities` | 空 object | 本 server 的 tool 名稱、input schema、版本與唯讀 scope 類型；不是團隊或 workspace 掃描 |

第一版不提供 content-bearing result／artifact download；外部工具取得狀態、證據 opaque refs、路由與成本 metadata。schema 設 `additionalProperties: false`；task_id 以現有 ID validator 和 scope membership 驗證，不接受路徑。

1. 每次呼叫使用不可變的固定 scope；active branch／session checkout 改變不能改變 server 的可讀目標。重新驗證持久化身份，發生衝突即拒絕，不追隨最新 session。
2. 每個 response 原子地對應一個 validated input head；後續呼叫可看到同 scope 的新 head，回傳其 ref。client 要比較資料時依 head 區分，server 不宣稱跨次呼叫具交易一致性。
3. stdin 只讀 MCP protocol，stdout 只寫 SDK JSON-RPC；log 送 stderr。不得讀 ask_user、顯示 spinner／banner、呼叫 CLI renderer 或從 handler 啟動 subprocess。
4. 每次請求 15 秒 deadline，接受 caller cancellation；每個 process 最多 4 個 in-flight requests，多餘請求回 `resource_limit`，不無界排隊。
5. 使用既有 bounded lineage reader，限制每次讀取 100,000 events／32 MiB event payload，輸出上限 1 MiB。超限回 `resource_limit`，不截斷成無效 JSON，也不回傳部分成功統計。startup bind 也必須使用相同上限；若現有 API 無法傳 limits，先抽取可重用的 bounded binding seam。
6. 共用 read-only service 接收 context／limits，不能先無界讀取後才檢查。artifact 讀取只允許既有 validator 為完整性核對所需的 bounded hash／schema 檢查；每次請求累計最多讀 32 MiB artifact bytes，先檢查大小並對實際讀取設限，超限回 `resource_limit`。不得從正文推導任務語意或向 client 回傳正文。限制檢查屬 deterministic validation，不呼叫模型。
7. tool error codes 固定為 `invalid_argument`、`not_found`、`scope_conflict`、`integrity_error`、`resource_limit`、`cancelled`、`internal_error`。沿用 SDK 分開 protocol error 與 tool error；內部錯誤不回 raw exception、路徑或 secret。
8. EOF／signal／cancellation 關閉服務時須取消並等待 in-flight handlers 結束；不能遺留背景工作或資料庫資源。

**完成條件**：用目前 SDK client 啟動真正的 server subprocess，驗證 initialize／tools/list／tools/call、所有 stdout frame、非法 extra properties、跨 task scope、兩個不同 scope 的 server、checkout 後仍固定 scope、deadline／cancellation、輸出與讀取上限、EOF／signal shutdown。全程只讀 fixture，無真實 host／provider／網路依賴。

## 5. 驗證與交付規則

### 5.1 必要測試矩陣

| 契約 | 必測案例 |
|---|---|
| Identity | occurrence reset、dispatch attempt 重號、extra-model、branch fork、origin run、歷史 run 查詢 |
| 完成證據 | done 無驗證、失敗驗證、成功 receipt 無證據、完整證據、衝突證據、缺 legacy 欄位 |
| 成本 | 失敗仍計價、local／subscription／unknown、未結算、部分記帳、overhead、重複 settlement、overflow、零分母 |
| Scope／integrity | exact binding、不存在或歧義、錯 session、錯 branch、壞 hash chain、未知 future schema |
| Read-only | 查詢前後資料與 event head 不變、不建立缺失 workspace、不啟動 provider／verifier／子程序 |
| Presentation | 同 head 一致、text／JSON／TUI、no-color／plain、resize、舊資料與 late response |
| Capability | precedence、draft、missing dependency、remote unknown、descriptor digest 穩定、secrets 不外洩 |
| MCP | 真實 process framing、固定 scope、參數驗證、bounded I/O、deadline、取消與結束 |

新增 feature 的 happy path 必須能產生有意義資料；unknown／拒絕測試不能取代正向驗收。fixture 證明程式行為，不宣稱代表真實模型效能。

### 5.2 必跑指令

每個修改程式碼的工作包先跑相關 focused tests；交付前分別執行並成功完成：

```bash
go test ./...
go vet ./...
golangci-lint run
```

`golangci-lint run` 是專案強制 gate，不是「環境有提供才跑」。若改到 concurrency／MCP lifecycle，另跑受影響套件的 `go test -race`。若改到 TUI，使用專案的 TUI 測試技能選擇必要的 view／update／process tests。Go 程式遵守 Modern Go Guidelines 技能與實際 toolchain。

環境失敗需記錄指令、exit code 與原因，修正可控環境後重跑；不可把 skip 或未執行標成通過。只修改此計畫文字時不需要程式 lint gate。

### 5.3 每個工作包完成條件

- 指出 modified paths、現有能力與新增缺口、實際測試結果。
- 新觀測值可回溯至固定 scope、event head、receipt／verification／cost invocation。
- 原有 routing、explicit override、fallback、budget、snapshot、resume、outcome 語意不變。
- 不新增使用者需手工準備的資料、帳號、批准或部署作為功能驗收條件。
- 新命令、JSON 欄位、unknown 語意與限制寫入既有適當 reference 文件；本計畫仍留原位。
- `git diff -- cmd/hufu internal` 沒有具體 team 名稱分支或只服務單一團隊的核心政策。
- 移除新命令／UI 入口即可停止使用新功能；既有 workspace 不需 migration、rollback 或資料清除。

## 6. Coding Agent 起始指令

```text
Read AGENTS.md and this plan at its current path.
Record HEAD and inspect existing implementation/tests before editing.
Implement WP-00 first, reusing characterization tests and fixture helpers.
Continue through WP-01 and WP-02, then WP-03, WP-04 and WP-05 in reviewable changes.
Use canonical read-only scope binding, event replay, evidence and cost projections.
Keep all validation offline with generated fixtures and the existing MCP SDK client.
Do not run real models, collect user data, add adaptive routing, or deploy services.
Do not weaken existing policy/snapshot/resume rules to make analytics easier.
Run focused tests and the full go test, go vet and golangci-lint gates.
Report actual evidence and remaining work; no fabricated performance claims.
Keep this plan at docs/implementation-plans/readonly-runtime-observability.md.
Commit each completed and validated work package separately.
```

完成上述工作包即完成本計畫；不以後續真實試跑、人工 promotion 或 production rollout 作為尚待完成的工作。
