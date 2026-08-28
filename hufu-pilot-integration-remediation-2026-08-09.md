# Hufu / Pilot 整合事故修正方案 — 2026-08-09

## 結論

事故報告顯示，前九輪主要是 Hufu 的 dispatch、session phase、typed
result 與 worker finalization 契約缺陷；這些通用缺陷已有對應修補。第十輪
剩下的問題是：Hufu 只會在 runtime 強制執行 `initial-batch` 與
`no-redispatch-after-success`，卻沒有可解析、可驗證的「初始批次之後狀態
轉移」契約。因此 coordinator 在 discovery 成功後仍選錯
`surface-explorer`，而不是唯一合法的 `plan-author`。Hufu 正確拒絕了重複
派工，但整個流程因而終止。

修正必須維持明確的產品邊界：Hufu 只提供通用 orchestration primitives，
不能知道 Pilot command、action schema、inventory 語意、Pilot 路徑或此 team
的 worker 名稱；Pilot team 只使用 Hufu 公開的通用設定格式宣告自己的流程。
兩個專案不得互相 import、以名稱分支，或加入只對另一方成立的特殊判斷。

## 解耦原則

Hufu runtime 唯一可以理解的概念如下：

- stable task/contract identity；
- worker capability 與 side-effect class；
- pending、running、terminal 等 task lifecycle；
- typed result 的 success、partial、blocked、error；
- dependency、允許的下一批 task、cardinality 與 one-shot constraint；
- checkpoint、resume、replay 與 branch lineage。

Hufu 不得理解或比對下列內容：

- `pilot` executable、Pilot action 或 inventory command；
- Pilot repository 路徑或檔案格式；
- `surface-explorer`、`plan-author`、`config-applier`、
  `inventory-verifier` 等 team-local 名稱；
- discover、plan、apply、verify 的業務意義；
- 特定 prompt 句子、輸出關鍵字或報告檔名。

相對地，Pilot team 不得依賴 Hufu 未公開的 Go type、workspace 內部檔案或
未解析 YAML 欄位。整合面只能是正式的 team schema、typed task result 與
CLI exit/status contract。

## 立即緩解（僅修改 team 宣告，不修改 Hufu 特例）

目前 team 設定方向正確。這些是 consumer-side configuration，不是將 Pilot
邏輯加入 Hufu：

1. `team.yaml` 的 `delegation.initial-batch` 固定為唯一且有序的
   `[surface-explorer]`，並維持 `exact: true`、`first-tool: agent`、
   `bind-contracts: true`。
2. `no-redispatch-after-success` 保留
   `[surface-explorer, plan-author]`。不可為了讓第十輪通過而移除此限制；移除
   只會把可診斷的路由錯誤變成可能重複執行的工作。
3. `coordinator.md` 必須只以 Hufu 的 Task Status 與 typed task result 判斷
   phase，不能使用 STM、LTM、歷史對話或 `context_files` 推斷目前進度。
4. discovery 成功後的下一個 `agent` call 必須是且只能是一次
   `plan-author`；不得以補資料、改格式、寫報告或取回結果為由再次呼叫
   `surface-explorer`。
5. `surface-explorer` 與 `plan-author` 都必須將完整交接內容放在 typed
   `details`，最後一步呼叫 `submit_result`；不要再以共享報告檔或
   `stm_write` 當作交接管道。
6. coordinator 或 worker 遇到 dispatch contract、tool policy、typed result
   或直接工具錯誤時應立即失敗，不在同一次 invocation 中自行改寫 payload
   或換 worker 重試。

這些 prompt/config hardening 可以降低錯誤率，但不是充分的安全保證。不要
修改 Hufu 加入 team 名稱特判，也不要新增 Hufu 尚未解析的自創 YAML phase
欄位；未解析的欄位不會形成 runtime policy。

## 根本修正（Hufu 通用且與 domain 無關）

在 Hufu 新增通用的 delegation state/transition schema。以下刻意使用無業務
意義的識別字，表示 runtime 只解析 graph 與 task state；實際 team 可將
contract ID 映射到任何 worker。欄位名稱可依現有命名規則調整，但 parser、
validator、prompt/schema shaping 與 dispatch gate 必須同時實作：

```yaml
delegation:
  states:
    - id: s0
      dispatch:
        contracts: [c0]
        exact: true
      on:
        success: s1
    - id: s1
      dispatch:
        contracts: [c1]
        exact: true
      on:
        success: s2
    - id: s2
      dispatch:
        contracts: [c2]
        exact: true
      on:
        success: s3
    - id: s3
      dispatch:
        contracts: [c3]
        exact: true
      terminal: true
```

`c0` 至 `c3` 是 team-local contract ID。Hufu 只驗證 contract 是否存在、其
worker/capability 是否有效，以及狀態轉移是否合法；Hufu 不得根據 contract
名稱、worker 名稱或 goal 文字推斷 domain 意義。

### Runtime 行為

- 狀態來源只能是 durable TODO/task-result state；不得從 prompt、summary、
  STM/LTM 或 conversation history 推斷。
- state 內所有必要 contract 都必須具有合法且成功的 terminal typed result，
  才可啟用下一條 transition。
- transition 啟用時，model-visible `agent` tool schema 只暴露合法的下一個
  worker；這可減少模型選錯，但真正的授權仍由 server-side validation 決定。
- `validateDelegationPolicy` 必須在建立 TODO、啟動 worker 或計入 retry 前，
  拒絕 worker、順序、批次大小或 cardinality 不符的呼叫。
- one-shot 必須以 canonical contract/task identity 與 terminal receipt
  執行，不能按 worker 名稱、goal 或描述文字判斷。resume、crash recovery 與
  session checkout 後仍須保持相同結果。
- blocked、partial、error、protocol-incomplete 與 verification failure 不得
  被當成 success transition。每一種非成功結果應有明確 fail-closed 或另行
  設定的 recovery edge；未設定時預設停止。
- 若多條 edge 同時可用或無法唯一決定下一步，team validation 必須報錯，
  runtime 不可要求 LLM 自行消除歧義。
- transition contract 必須適用所有 team；core package 不得 import Pilot
  package、讀取 Pilot 設定，或以 repository/team/worker 名稱分支。

### 建議修改位置

- `internal/agent/agent.go`：擴充 `DelegationPolicy` 的 domain-neutral typed
  state/transition 定義。
- `internal/team/parse.go`：解析 YAML，拒絕未知狀態、空 worker、重複或歧義
  edge。
- `internal/team/delegation_policy.go`：只由 durable contract/task/result state
  計算目前 edge，並在 dispatch 前強制執行。
- `internal/team/coordinator_tools.go`：依目前合法 edge 縮小 `agent` tool 的
  worker enum 與說明。
- `internal/team/coordinator_prompt.go`：顯示 runtime 已計算出的 canonical
  transition；prompt 只用於說明，不作為 policy authority。
- `internal/team/coordinator_session.go`、`internal/team/session.go`：確保
  transition state/receipt 在 checkpoint、resume、corrupt-session recovery
  與 branch checkout 後可重建且不倒退。
- `internal/team/contract_compile.go`：將目前只處理 initial batch 的 compiler
  泛化成任意 state 的 immutable contract compiler；API 不得接收 domain
  名稱或 Pilot-specific option。

### 依賴方向

```text
Hufu core <- generic team schema <- team configuration
```

Hufu core 定義並驗證通用 schema；team configuration 單向依賴該 schema。
Hufu core 不反向依賴 Pilot，Pilot executable 也不必依賴 Hufu Go package。
兩者若需整合，只能透過 process boundary、公開 CLI contract 與 typed data。

## 必要測試

新增功能至少應覆蓋以下案例：

1. fresh session 只接受 state `s0` 的 contract `c0`，拒絕 `c1`。
2. `c0` 成功後只接受一次 `c1`，並拒絕再次派 `c0`。
3. `c0` 為 blocked、partial、error 或 protocol-incomplete 時，不啟用
   success edge。
4. `c1` 成功後只接受 `c2`；`c2` 成功後只接受 `c3`。
5. malformed task array、錯誤順序、額外 worker、空 batch 與重複 edge 都在
   worker 啟動前被拒絕。
6. successful typed `details` 可完整傳給下一 phase，不退化成 summary，也不
   需要 redispatch 或共享報告檔。
7. checkpoint/resume、舊 session migration、corrupt session、`--new` 與
   session branch checkout 不會讓 phase 倒退或重複執行 one-shot worker。
8. coordinator 看見的 `agent` schema 只列出目前合法 worker；即使模型送出
   schema 外 worker，server-side gate 仍會拒絕。
9. transition 設定有循環、不可達節點或兩條同時匹配的 edge 時，`hufu team
   validate` 回傳非零。
10. 以至少兩組完全不同的 worker/contract 名稱重跑相同測試，證明 policy
    不依賴名稱或 domain 關鍵字。
11. Hufu 測試 fixture 不需要 Pilot binary、Pilot repository 或 Pilot schema
    即可完整驗證 state machine。

## 驗證與再次整合的放行條件

完成 Hufu code change 後，在 `agent-team-cli` 執行：

```bash
go test ./internal/team -count=1
go test ./internal/... -count=1
go test ./cmd/hufu -count=1
go test ./...
go vet ./...
golangci-lint run
go build ./cmd/hufu
```

通用 Hufu 測試通過後，再由各 consumer project 自行驗證其 team 設定。此
步驟不是 Hufu unit/integration test 的依賴；Hufu CI 不得 checkout 或執行
Pilot：

```bash
/home/ubuntu/bin/hufu team validate \
  /home/ubuntu/nfs/github/pilot/.agent-teams/pilot-edit-workflow
bash -n /home/ubuntu/nfs/github/pilot/run.sh
```

在重新執行 consumer workflow 前，可於 consumer repository 做不執行 domain
mutation 的 bounded smoke test。對本次事故的 team，它應斷言既有設定宣告的
順序；這是 consumer acceptance test，不可加入 Hufu core test suite：

```text
surface-explorer -> plan-author -> config-applier -> inventory-verifier
```

smoke test 必須同時斷言：沒有成功 worker 被 redispatch、每個 phase 都有合法
typed result、錯誤會 fail closed，且 apply 前已產生可驗證的 policy-approved
plan。只有上述測試與 lint 全部通過，並另行取得對 Pilot 工作區 mutation
的明確授權後，才可再次執行整合流程。不得用直接執行 Pilot command 取代
Hufu orchestration contract test。

## 不應採用的修法

- 不要移除 `no-redispatch-after-success`。
- 不要增加 worker step budget 來掩蓋 result finalization 或路由錯誤。
- 不要要求 worker 寫共享報告檔來取代 typed `details`。
- 不要讓 coordinator 擁有 shell，藉此繞過 worker capability boundary。
- 不要依賴 STM/LTM 或舊 session 文字判斷目前 phase。
- 不要只改 coordinator prompt 就宣稱已有 runtime 保證。
- 不要在 Hufu source 中出現 Pilot path、command、action、inventory 或
  team-local worker 名稱。
- 不要讓 Hufu test suite 依賴 Pilot repository、binary 或 fixture。
- 不要讓 Pilot import Hufu internal package 或讀寫 Hufu 私有 session 格式。
- 不要在未授權下重跑 `run.sh` 或執行任何 Pilot mutation。

## 完成定義

本事故可在以下條件全部成立後關閉：Hufu runtime 以 domain-neutral contract
ID 可機器驗證完整後續 transition、通用 unit/integration/resume 測試完全不
依賴 Pilot、`golangci-lint run` 通過、consumer team validation 通過、consumer
端 bounded smoke 確認其宣告順序且無 redispatch，最後再由獲得授權的端到端
執行證明 consumer workflow 成功。
目前事故報告只證明 Hufu 邊界失敗，尚未證明 Pilot mutation 功能有缺陷或
端到端整合成功。
