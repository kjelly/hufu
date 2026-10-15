# 可重播貝式信念狀態：A–D 實作計畫

> Status: active implementation plan
> Authority: reference（程式、測試及 accepted ADR 優先）
> Baseline: `c03b785788fbe9b1f636e8b8baca745b6345114e`
> 日期：2026-10-15
> 本計畫依使用者最新需求取代原唯讀 inspector 計畫。每階段驗證後各 commit；不提交 docs/tmp。

## 1. 交付目標與已確認範圍

`RuntimeState = execution state + Bayesian belief state`。執行狀態繼續由既有 receipt、verification、task reducer、authorization 與 recovery owner 管理；信念狀態是同一持久化事件 lineage 上、綁定不可變模型及 epoch 的可重建狀態分量。

使用者已選定先用明確標為 synthetic 的模型完成 A–C，以及 D 的策略建議。通用 runtime 支援 owning team 提供版本化模型檔；本輪不啟用實際候選排序或機率控制 gate。正式模型數字與適用條件由使用者／team 維護者提供，不捏造真實診斷的 prior／likelihood。

A–D 都須有離線、自動化測試，不以真人操作時間、live provider、實務校準研究或模型成功自述作為驗收。

## 2. 不可破壞條件

- 信念不是執行收據、授權、verification 或副作用未發生的證明。
- 只沿用 `runtime.sqlite` journal 與 immutable ArtifactStore，不建立 belief DB、第二套 scheduler 或 recovery。
- 未配置時不產生新事件、不改模型 prompt/schema、封存 evidence hash 或執行結果。
- Replay／inspect 不讀當前 team 模型替代歷史 artifact，不呼叫 LLM/tool，不重跑 verification、mutation 或已完成 task。
- branch/fork cursor、task occurrence、attempt、subject/environment/model revision 必須隔離；不同 invocation 不自動等於獨立證據。
- 缺來源／缺模型／不支援版本回報 unavailable；未知數值不填 0、0.5 或 uniform。
- domain 名稱、假設、數值、candidate vocabulary 放 team/model artifacts；core 不解析工作 prose。
- belief 更新失敗不回寫已完成 task 成失敗；記錄可見診斷，resume 只補 observation bookkeeping。

## 3. A：有限離散計算核心

新增純套件 `internal/belief`，不 import team/context，不做 I/O、不讀時鐘、不產生隨機值。

### 3.1 模型 JSON 合約

- `schema_version: 1`、`reducer_version: finite_bayes_v1`、穩定 `id`、`version`。
- `synthetic` boolean；`prior_source`、`independence_basis`、`applicability` 明確文字說明。
- `hypotheses`: ID、meaning、prior_units，互斥且完整。
- `experiments`: ID、correlation_group、source（第一版 enum 只有 verification）、outcomes。
- outcome: ID、likelihood_units（依 hypotheses 的模型順序，表示每個 H 下此 outcome 的機率）。同 experiment 的完整 outcomes 對每個 H 加總為 scale。
- 模型所有文字是說明資料，不參與執行路由；不從 wording 推導 action 或權限。

scale 固定 1,000,000,000（Q9）。Prior exact sum = scale，likelihood 每項在 [0, scale]。不得先轉 float64；拒絕未知 JSON member、重複 ID、不完整 outcome、越界值、負數與非整數。

資源上限：模型 1 MiB、2–32 hypotheses、1–32 experiments、每 experiment 2–16 outcomes、每 epoch 256 observations、中間整數最多 16,384 bits。所有上限先驗證，超限回傳 typed error，不截斷輸入。

### 3.2 投影

輸入：固定版本 Model、Epoch、有效 Observation set。Epoch 至少包含 ID、scope identity、subject_revision、environment_revision；模型 digest 與 reducer version 固定綁定。

Observation 包含 ID、experiment ID、outcome ID、source identity。相同 ID、相同 canonical payload 冪等；相同 ID 不同 payload 是 identity conflict。

有效且可獨立的不同 correlation_group 才能相乘。同 group 有多個不同 observation 時回報 dependence_unresolved，不隱含選第一筆／最後一筆或當成獨立樣本。重播同一 ID 100 次不產生此問題。

從原 prior × 全部有效 likelihood 重算 BigInt raw weights，最後一次 largest-remainder Q9 量化；相同餘數以 hypothesis ID byte order 破同分。中間不能用前次四捨五入的 posterior 繼續更新。

State 保存 model/epoch/reducer identity、有效 observation IDs、distribution、status、digest。digest 使用固定版本 canonical JSON，排序集合；排除 digest 自己、wall clock、非確定性 diagnostics。相同觀測集合以不同批次到達，投影相同。

所有 raw weights 為零：model_inconsistent，distribution null。正 raw weight 量化為零：resolution_limited。無觀測：明示 prior 狀態。數學可計算不等於模型已校準，synthetic 標記必須傳遞到 State。

### 3.3 驗收

- 合成 prior (0.4, 0.6)、likelihood (0.2, 0.8) → Q9 (142857143, 857142857)。
- 同一 E 重播 100 次結果相同；同源不同 ID 不被額外累乘。
- zero prior、zero likelihood、全零分母、量化殘差 tie、資源限額、輸入 permutation、未知版本、非法 JSON 邊界。
- 結果 digest 可重現，投影不修改輸入；property/fuzz 與 benchmark 使用固定合成數字。

## 4. B：模型鎖定、epoch 與 durable replay

在 `internal/team` 新增 generic belief journal adapter。新增事件名稱固定為：

- `belief_model_bound`: immutable model ArtifactRef、digest、schema/reducer version。
- `belief_epoch_opened`: epoch identity、subject/environment revision、model binding ref、source cursor。
- `belief_observation_admitted`: observation identity、typed experiment/outcome、來源 event/receipt identity、epoch。
- `belief_observation_retracted`: observation identity、合法 runtime authority、reason code、來源 ref。
- `belief_epoch_closed`: epoch identity、trigger ref、reason code。

事件 metadata 不放 raw output、prompt、command 或自然語言說明全文。模型內容經既有 ArtifactStore.Put 成為 immutable artifact，先 artifact 後 event reference；不另建儲存格式。

source admission 必須從已驗證 journal 中的真實 source 取值；caller 提供的 event ID／receipt 字串只作 lookup，不作權威。worker 解釋不變成 verification。相同 receipt occurrence、experiment、epoch 的引用只對應一次 admission。

撤回 append-only，保存 tombstone；晚到的 admission 不能復活已撤回資料。由 prior 與剩餘 set 重算，不使用反向除法。不存在目前 artifact 時回報 unavailable，不讀現在設定或重新下載補歷史。

投影先驗證完整 journal 及 branch lineage，再解析 model binding、epoch、admissions、retractions、closure，最後呼叫純核心。snapshot 可丟棄；不把 cache 當 truth。未知事件版本或 model/reducer version 有明確不可用原因。

crash windows：source durable/admission 缺失時 runtime 以固定 key 補 admission；artifact 已寫/event 未 commit 可留 orphan；event commit durability unknown 必須以相同 key 查證；replay 不再 append 或執行 source。

驗收：模型 digest 驗證、missing artifact、tombstone、source forgery、同 receipt 多次引用、同 bytes 不同 occurrence、branch/fork/private scope、crash gap、閉合 epoch、重播零 tool/provider 呼叫。

## 5. C：真實狀態機接線與 inspect

### 5.1 opt-in team 設定

新增 team-owned `belief` 設定，至少 `model-file`；LoadTeam 使用現有 team resource loading 邊界驗證與編譯，缺省不啟用。拒絕路徑越界、symlink escape、過大檔案或 schema 不合法。compiled model 與 model artifact identity 進 immutable execution-policy snapshot，resume 不 silent reload 新模型。

第一版 source adapter 只讀取已持久化 `ExecutionReceipt.VerifyResult`：真實 verification 的 exit code／timeout 等 typed 結果，對應模型宣告的 pass/fail outcome。不以 stdout、worker success 或 judge opinion 判定。

epoch 鎖定 run/branch/task/occurrence/attempt、已持久化 subject/input revision 與 verification/environment contract identity；identity 缺失就 diagnostic unavailable，不補假值。跨 revision／環境／模型／attempt 不沿用 posterior。初期採窄的每 occurrence epoch，避免默認跨 attempt 獨立。

### 5.2 canonical 整合點

從已 durable 的 task transition／receipt 產生 observation；放在共用 canonical transition owner 後方，涵蓋 normal/direct/action/extra-model/resume paths，不在各 worker success branch 另寫實作。

Coordinator 維護可重建的 belief projection；以獨立鎖保護快照，不在 EventStore transaction 中執行長計算或讀外部內容。bootstrap/resume 從原 model binding 與 selected lineage 重建，修復 source/admission gap，不重跑 task。

### 5.3 CLI

新增 `hufu inspect beliefs <run-id>`，支援 common workspace/branch/session/format/output 與 optional task filter。顯示 epoch、model digest/version/synthetic、hypothesis distribution、source refs、retractions、status/unavailable reason、state digest。執行狀態與信念分欄，unknown 不渲染為數值 0。

text/JSON 同義；唯讀 inspector 不補 admission、不 migration、不寫 workspace，不執行 replay side effects。沿用 inspect error/exit contract與 theme/plain 行為。

驗收：真正 Coordinator transition 產生 observation／state；SQLite journal 與模型 artifact 端到端重建相同 digest；resume、source/admission crash、late result、revision change、無配置 compatibility、缺模型、合成標記、正常驗證結果不受投影錯誤影響。

## 6. D：固定策略建議（advisory only）

team 宣告固定版本 policy 與既有 candidate IDs；所有候選必須在既有已授權集合內。policy 是資料與確定性映射，不從 task prose 或模型自填 shell command 授權。

新 observation 更新 State 後，計算建議並 append `belief_policy_suggested`：snapshot/state digest、policy version/digest、selected candidate ID（或 unavailable）、decision generation、epoch/source cursor。

本輪只記錄建議，不執行 candidate、不改原 routing、budget 或 gate。不支援 active mode 的設定明確拒絕，不能接受卻悄悄降成 advisory。

相同 state/policy/generation key 冪等。resume 使用原 policy/model/snapshot binding；changed policy 不能改寫同 generation 的歷史理由。未知 distribution、未校準／合成 model 可以顯示 advisory，但不得被解讀為授權。

驗收：記錄完整理由、同 snapshot 重播相同建議、零新增 tool/LLM/dispatch、budget 不重設、舊 generation 結果不污染新 generation、缺候選不擴增權限、歷史 policy 不讀現在設定。

## 7. Commit 與驗證順序

1. 文件搬移 commit（已完成 `de84101e`）；確認 baseline 完整測試／vet／lint 的實際結果。
2. 重編 A–D 計畫與 baseline 紀錄 commit。
3. A 計算核心、合成 fixture、邊界測試，驗證後 commit。
4. B journal／artifact／replay，驗證後 commit。
5. C team config／snapshot／state machine／inspect，驗證後 commit。
6. D advisory policy 及完整驗證／使用指南，驗證後 commit。

每個程式階段必跑 targeted tests 與 `golangci-lint run`；階段 B/C 的持久化／concurrency 改動另跑相應 race tests。最後執行：

```bash
go test ./internal/belief/... ./internal/team/... ./internal/inspect/... ./cmd/hufu/...
go test -race ./internal/belief/... ./internal/team/... ./internal/inspect/...
go test ./...
go vet ./...
golangci-lint run
```

測試需本地 HTTP/socket 時用環境允許的 executor，不能把 sandbox 拒絕誤報為產品缺陷或 skip 測試。記錄實際命令、exit code、baseline failure 與新增 regression 的差別。

## 8. 完成證據

- A–D 各有可辨識 commit，沒有停在純 helper 而宣稱 runtime 已接線。
- 模型、epoch、source binding、撤回、branch、snapshot digest、policy/generation 皆有實際 integration tests。
- runtime 執行狀態與 belief State 可分別由 durable sources 重建；刪 cache 不改結果。
- 無配置路徑保持相容；無新 team-name 分支；新輸出沒有 secret/raw output 或 private-scope 洩漏。
- 文件明確標出 synthetic／advisory，指南命令可以執行；不宣稱正式模型已校準或 active 排序已啟用。
- 必跑測試／vet／lint成功，工作樹乾淨，沒有提交 docs/tmp 或暫存 artifact。
