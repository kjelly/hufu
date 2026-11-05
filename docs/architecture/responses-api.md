# Hufu × Fantasy：Responses API 明確協定設定實作規格

> Status: active
> Authority: normative
> Validation: [offline implementation evidence](../reference/responses-api-validation.md)
> Supersedes: —
> Superseded-By: —
> 狀態：WP 0–6 已完成離線實作與驗證；真實端點驗證仍待另行授權。
> 修訂日期：2026-11-05。
> 程式查核基準：`9420c2f58118cb5647e582ace02cc840452cdfea`。
> SDK 基準：`charm.land/fantasy v0.45.5`，以 repository 的 `go.mod`／`go.sum` 為準。
> 交付範圍：由 coding agent 完成程式、離線測試、CI 與使用文件；不包含真實帳號設定、付費呼叫、外部發布或上游套件發布。
> 本次保留明確協定選擇，移除自動判斷、探測與灰度 rollout 工作。真實端點驗證由使用者之後另行授權，並非本文件完成條件。

## 0. 目標與固定決策

新增 provider-scoped `api-protocol: chat-completions | responses`。省略或空值維持歷史 Chat Completions 行為。沿用 Fantasy 的 HTTP／JSON／SSE adapter，所有模型請求繼續經 Hufu 的 provider boundary、admission、durable invocation 與 usage settlement。

效益是支援 Responses 端點、明確控制請求格式，以及在 resume 時防止協定漂移。這項變更不承諾降低成本、延遲或提升回答品質。

固定決策：

- 不新增 `auto`、模型名稱選協定、endpoint capability discovery、付費 probe 或 capability cache。
- 不新增逐模型協定覆寫。需要同一 URL 混用兩種協定時，使用不同 provider key，並按現有規則分別管理配額；不宣稱它們共享 provider quota。
- 不新增 hosted tools、`store: true`、`previous_response_id` 或服務端 conversation chain。
- 不新增 reasoning items 的保存／重送能力；首版只支援 SDK 現有的可見 history 與 function call/result 往返，以及可驗證會送出的 reasoning effort。不能宣稱支援 reasoning 狀態延續。
- 不自行維護 Responses parser、不修改 module cache、不引入本地 SDK fork、不要求上游發布。保留 v0.45.5，透過明確支援邊界和 preflight 錯誤處理其限制。
- 不在 request 送出後換協定重播；不修改歷史 receipts、事件或 snapshot hashes。
- Responses 在設定、transport、admission、結算、durable binding 與離線整合測試全部完成前不得對 production 呼叫開放。

## 1. 查核基準與 SDK 限制

| 位置 | 原始查核基準現況 | 實作要求 |
|---|---|---|
| `internal/agent/agent.go` | `newOpenAICompatProvider` 未傳 Responses options；boundary 啟用時會重新建 provider | 初建與 proxy rebuild 使用同一份不可變協定設定 |
| `internal/config/config.go`、`cmd/hufu/backend_config.go` | provider 無 `api-protocol`；backend 的 `protocol` 供 agent backend 使用 | 新增獨立欄位並涵蓋 canonical／legacy、global／team merge |
| `internal/agent/provider_ref.go` | identity hash 未包含 API 協定 | 新設定納入 identity，空值保留歷史 hash |
| `internal/agent/reasoning_options.go` | Chat options 以 `model.Provider()` 為 key | Responses 使用 `openai.Name` 和 `*openai.ResponsesProviderOptions` |
| `internal/agent/provider_request.go` | admission serializer 使用 Chat `messages`／tools | 加入 Responses 專用估算及 immutable protocol context |
| `internal/agent/tool_call_index.go` | 所有 SSE 都先經 Chat tool index wrapper | 限定 Chat 路徑；Responses bytes 不變；保留 transport observers |
| `internal/scripted/provider.go`、`boundary.go` | 只支援 Chat／models 路徑 | 新增 Responses fixtures 與 allowlist，保留既有腳本語意 |
| `internal/team/execution_policy_snapshot.go` | 支援多個歷史 snapshot 版本，含 selected dependency view | 遍歷實作當下所有受支援版本，不只測 v3／v4／v5 |

Fantasy v0.45.5 的已知限制必須寫成離線回歸測試與使用文件：

1. `WithUseResponsesAPI()` 和 `WithResponsesAPIFunc()` 必須一起配置；後者固定依已綁定協定回傳，不呼叫 `IsResponsesModel()`。
2. `IsResponsesModel()` 包含寬鬆名稱 regex，不能作為端點能力或精確支援清單。首版不使用它選協定。
3. Responses `prepareParams()` 只讀 `ProviderOptions[openai.Name]` 的 Responses options。
4. `getResponsesModelConfig()` 的內部分類與 `IsResponsesReasoningModel()` 不是同一個判斷。一般 `gpt-6` 名稱、自訂 alias 可能不會送出 reasoning effort；不能只換 options key 就宣稱生效。
5. `toResponsesPrompt()` 略過 reasoning items；首版不提供 encrypted reasoning replay。可見文字與工具往返的測試成功，不代表 reasoning continuity 已支援。
6. SDK 預設 `store:false`；保留此值，且不允許本功能的其他 options 合併將它改為 true。
7. SDK 已停用底層自動重試；契約測試仍須斷言失敗 generation 的實際 HTTP 次數，避免之後升級改變行為。

## 2. 配置契約

```yaml
backends:
  openai:
    kind: llm
    type: openai-compatible
    base-url: https://api.openai.com/v1
    api-protocol: responses
  local-chat:
    kind: llm
    type: openai-compatible
    base-url: http://127.0.0.1:8000/v1
    api-protocol: chat-completions
```

Legacy 形式同樣接受欄位；與 canonical 範例分開使用，避免同名 backend 衝突：

```yaml
providers:
  openai:
    provider-url: https://api.openai.com/v1
    api-protocol: responses
```

API key 沿用現有 credential 機制，不新增 YAML 環境變數插值語法。Responses factory 明確傳入 Hufu 已解析的 key（含空值），禁止 SDK 自行從 `OPENAI_API_KEY` 引入未納入 provider identity 的 credential；legacy Chat 的既有行為不變。

| 輸入 | 有效協定 | 身分語意 |
|---|---|---|
| 省略／空字串／純空白 | `chat-completions` | legacy，保留舊 alias equivalence 與 policy hash |
| `chat-completions` | `chat-completions` | explicit，納入新 binding |
| `responses` | `responses` | explicit，納入新 binding |
| 其他值，含 `auto`、`chat`、`response` | 錯誤 | 載入／preflight 拒絕，零 generation |

- 正規化採 trim + lowercase。持久化與錯誤輸出使用 canonical 值。
- `api-protocol` 只適用 `kind: llm`；agent backend 填此欄位應拒絕。LLM backend 錯填 agent 用的 `protocol` 時給明確診斷。
- canonical backend → legacy provider adapter 必須傳遞此欄位。保留各層既有 merge 語意：`applyConfiguredBackends` 的 global／team 同 raw key 採整筆 entry 覆寫；config 檔案 loader 的 legacy providers 及 `MergeProviderConfigs` 採非空欄位覆寫。後兩者的空白 protocol 繼承前層，要取消前層 Responses 時明確填 `chat-completions`。不因本功能改變其他欄位的繼承方式。
- 不同 raw key 被 canonicalize 成同名時，沿用現有拒絕／equivalence 規則，不透過 map iteration 決定協定。測試 legacy 空值與 explicit Chat 的身分差異。
- 舊 exported constructors 保持原簽名與 legacy Chat 預設；新增 private factory/options 傳遞設定。
- 不新增 CLI、agent frontmatter 或 model-output 協定覆寫來源。

## 3. Transport 與 request binding

解析器是純函式，只將配置轉成 requested／effective protocol、source（`legacy` 或 `explicit`）和 binding version。保留 requested 空值資訊；不能提早改成 explicit Chat 而改變歷史 hash。

流程：配置正規化 → 選出實際 provider／wire model → 建立或核對 immutable protocol binding → 建 Fantasy model → protocol-aware options／admission → invocation commit → transport → settlement。

- 解析以實際選中的 provider 為準，涵蓋 default、named、local／ollama alias、unknown prefix fallback、lazy init、sidecar、coordinator、worker、collector 與 review 路徑。
- prefix 僅依現有 `modelName()`／routing 語意移除；provider identity 使用 upstream URL，不使用 loopback proxy URL。
- provider factory 在 Chat 模式固定選 Chat，在 Responses 模式固定選 Responses；不依模型名稱猜協定。
- 初建、subprocess proxy、in-process boundary rebuild 使用同一 binding，不能重新讀可變全域設定。
- `/responses` 與 `/chat/completions` 都由 Fantasy 拼接到配置的 base URL；測試 `/v1` 與 proxy capability prefix。配置其他 base path 時不可硬編碼覆蓋它。
- `toolCallIndexTransport` 只改寫 Chat Completions 的 POST SSE；Responses 的 events、payload、換行與 usage bytes 原樣轉發。
- transport identity、rejection 和 usage observers 仍在兩條路徑執行。不得因跳過 Chat rewrite 同時跳過 observer。
- 401／403／404／400／429／5xx、timeout、cancel 或中斷不觸發跨協定 fallback。保留既有有界 429 recovery，每次實際重試各有 invocation／settlement；其他錯誤不由本功能新增重送。

## 4. Reasoning options 與支援範圍

新增 protocol-aware options builder，保留既有 Chat helper 的呼叫相容性。Responses options 必須與既有 options 合併，不覆寫無關設定；強制 `store:false`，不設定 `previous_response_id`。

### 4.1 可由 coding agent 完成的 capability gate

- 建立 Hufu 私有的「SDK 能否傳送指定 reasoning effort」判斷。它只表示 pinned adapter 的實際行為，不宣稱上游一定接受。
- 用 v0.45.5 `getResponsesModelConfig()` 的分支建立 table-driven HTTP payload tests；將判斷和 pinned SDK 行為用同一組 fixtures 對照，涵蓋非 reasoning 分支、o1 特例、gpt-5-chat 特例、SDK 已辨識的 reasoning 分支、一般 gpt-6、自訂 alias 及大小寫。
- 所有實際選中模型均檢查，包括 fallback／retry／escalation 的新 target。已顯式配置 effort、但 SDK 不會傳送時，在該模型 request 的 invocation commit 前拒絕；錯誤說明設定無法由此 adapter 傳送。不可靜默移除 effort、換協定或換模型。
- 允許值維持 Hufu 的 `none | low | medium | high`；不新增值，也不把 `none` 當成未配置。未配置 effort 不強加預設 effort。
- SDK 會傳送但上游拒絕的 effort，正常回傳 provider error，不重送。離線測試不宣稱已驗證特定真實模型接受它。
- sampler 行為以 capture 為準。SDK 移除 temperature／top-p 時呈現既有 warning；不能僅憑 public reasoning model helper 假設它們被移除。

### 4.2 History 能力邊界

可見 assistant 文字、function calls、tool results 與 call IDs 必須完整往返。Reasoning metadata 依 SDK 現有語意處理，不新增 replay 路徑；文件明確寫出首版不保留 reasoning 狀態延續，不提供相關品質或成本保證。遇到 SDK 不支援的內容型態應在 admission 拒絕，不能把缺漏內容當成完整 request。

## 5. Responses admission 與 output budget

- 在 `ProviderAdmissionContext`／`ProviderRequest` 傳遞已綁定 effective protocol。空值相容既有 Chat 路徑；計數器不得重跑模型 heuristic。
- 保留 Chat serializer；新增明確標為 estimate 的 Responses serializer，不複製 Fantasy 私有 encoder。
- Responses estimate 涵蓋 system（包含 SDK 轉成 developer 的內容）／user／assistant 文字、function names／descriptions／完整 schema、tool arguments／results、call IDs、角色／item 邊界及 structured-output schema。`GenerateObject`／`StreamObject` 不得因使用不同 schema 位置而漏算。
- 首版 Responses 只接受文字及 function call/result 內容。image／audio／file 等 multimodal 請求在 transport 前拒絕；Chat 的既有媒體行為不變。刪除原計畫對 Responses multimodal 完整支援的承諾。
- estimator 明列版本與公式：對完整估算表示使用與 pinned SDK 一致的 HTML／JavaScript escaping，再以 JSON UTF-8 byte length 作保守文字基數，再加每個 input item／tool 的固定 overhead；與現有模型估算取較大值，最後使用現有 safety margin 與 output reservation。固定 overhead 常數須命名、註解來源與測試。不得繼續採 `len(json)/4` 作 Responses 的保守基數。
- 這是估算，不能宣稱為任意 provider tokenizer 的數學上界。未知 context capacity 沿用現有 fail-closed 規則，不能因協定改變而取得較寬鬆的容量。
- `max_output_tokens` 包含 reasoning 與可見輸出；reservation 與實際有效 max output 一致。
- 用 fake upstream capture 的實際 wire payload 建立獨立比較器：驗證上述每項內容均被估算涵蓋，且 estimate 至少覆蓋 captured input／tools／tool choice／output schema 的實際 JSON UTF-8 bytes 加上 base allowance。實際 wire JSON 已含 item/tool framing；logical estimate 的逐項 allowance 用來補足這些 framing 與 SDK defaults，不在 wire oracle 重複加一次。忽略不屬於 input token 的 transport 欄位，不能把 Chat estimate 當唯一 oracle；比較原始 wire bytes，禁止 decode 後以另一組 escaping 重新序列化來縮小 oracle。
- 邊界測試驗證 estimate + reserved output + safety margin 超過容量時零 generation；涵蓋中文、emoji、長 history、多工具、大 schema、structured output 與空輸入。錯誤不能靠縮小 fixture、跳過 assertion 或提高容量解決。

## 6. Usage、串流與結果契約

- 經現有 admitted model、`CommitProviderInvocation`／`SettleProviderInvocation` 路徑，每次實際 HTTP generation 有自己的 invocation，且至多 settle 一次。
- 核對 raw Responses usage、Fantasy normalized usage 和 ledger：API input 包含 cached input，reasoning 屬於 output；按照現有 normalized representation 結算，禁止再次扣除 cache 或再次加上 reasoning。
- 分別測 usage 缺席、明確全零、cached、reasoning、取消與有 terminal usage 的失敗。不能把 SDK 零值視為上游真的回報零。
- 必要時在 host-owned transport 保留 usage presence，覆用既有 trusted observation seam；不得以模型輸出作為用量證據。測 non-stream 與 stream，觀察器不能消耗掉下游的 body。
- 分別處理 `response.completed`、`response.incomplete`、`response.failed`、無終止事件 EOF、取消與 consumer 提早停止。異常結束不得被當作成功結果。以 bounded host-owned metadata observer 核對 terminal status，補足 pinned SDK 對部分 incomplete reason 仍回傳 tool-call finish 的限制；不以此 observer 組裝生成內容或取代 SDK parser。單一 JSON／SSE event data（含多行合併）超過既有 `maxObservedProviderBody` 上限而無法核對終止狀態時，observer 仍不得截斷或改寫回應 bytes，但須拒絕接受結果或派發工具，用量維持 unknown。
- 多個 parallel function calls 的 ID／name／arguments 不混淆。未完成、異常終止或不合法的 arguments 不得執行工具；測 local tool authorization 與各工具的執行次數。
- 驗證 `result-transport: runtime` 的 collector，以及 `tool` 的 `submit_result`。沿用 schema validation、runtime-owned fields、bounded repair、持久化／replay 與 idempotency，不修改語意判斷去迎合格式。

## 7. Durable identity 與 resume

- 新配置的 canonical requested／effective protocol 和 binding version 納入新 provider／route identity。legacy 空值保留原 hash 輸入與序列化形狀。
- 為新設定建立有版本的 route／attempt binding；static targets 在 preflight 固定，動態 target 在自身第一次 invocation commit 前固定。兩者都不能在送出後補記。
- 使用現有 snapshot versioning／compatibility materializer，涵蓋目前所有 supported versions（含 selected dependency view）。舊 snapshot 無欄位視為 legacy Chat，不改寫歷史 hash；不能在舊版本序列化時偷偷加欄位。
- 新 session 只寫入該模式需要的新版本／欄位：explicit protocol 使用 full-view v7／selected-view v8；legacy 仍使用 v5／v6。每次 invocation 的 profile event 在送出前保存 schema v2 的協定 binding；legacy event 保持 schema v1。explicit Chat 與 legacy Chat 的差異是配置來源身分，不是 wire format。
- resume 以 frozen binding 驗證目前 factory 可重建相同協定。配置／binding 變動先報 policy drift，零 generation，不重算既有 receipt。
- 必測 upstream URL、credential／TLS 的既有 identity 保護未退化，以及 sidecar、fallback、escalation 和 selected dependency view 的 binding 不遺漏。

## 8. 工作包與啟用順序

| WP | 工作 | 完成出口 |
|---|---|---|
| 0 | 建立 pinned SDK HTTP 契約 fixtures，記錄 options、reasoning gate、replay 限制與底層 retry 行為 | 四個 Fantasy entry points 的可重現基線；已知限制有 assertions |
| 1 | 新欄位、normalize、merge／alias、immutable explicit resolution | 非法值／不適用 backend 零 generation；舊 constructors 行為不變 |
| 2 | direct／proxy factory、Chat SSE path guard、Responses scripted fixtures | 正確 upstream path、boundary 無繞路、SSE 不被改寫 |
| 3 | options capability gate、Responses admission、usage presence／settlement | 顯式 effort 不靜默丟失、超量／媒體先拒絕、計費不重複 |
| 4 | snapshot／route／attempt binding 和 resume | 歷史版本相容、協定 drift 零 generation |
| 5 | coordinator／collector／CLI 整合矩陣及必要 CI gates | 下節測試全過；runtime/tool 結果契約無回歸 |
| 6 | doctor／team check 診斷、設定文件、release-note 草稿 | 明確揭露 chosen protocol、估算與 reasoning 限制 |

WP 0 → 1 → 2 → 3 → 4 → 5 → 6。可分 commit，但 production 啟用必須在 WP 0–5 全部通過後才接通。中間版本遇到 explicit Responses 應在配置 preflight 回報尚未啟用；不得忽略設定退回 Chat。最終版本移除這個開發期 gate，不新增使用者 feature flag。

所有工作都在本 repository 完成；不要求 coding agent 執行外部發布、帳號設定、真實 API smoke 或向上游提交 PR。

## 9. 必要離線驗收

| 測試組 | 必須斷言 |
|---|---|
| 配置 | legacy 空值、explicit Chat、Responses、大小寫／空白、非法 auto、global/team merge、canonical/legacy、alias conflict、agent backend 誤填 |
| Transport | default／named／fallback／sidecar、三種 boundary 路徑、base path、四個 SDK entry points、Responses SSE bytes 不變 |
| Options | Chat regression；已辨識模型 effort 實際送出；gpt-6／custom alias 顯式 effort 被 gate 擋下；none 不當空值；sampler warnings |
| History | 多輪文字、parallel function calls／results、call IDs；SDK 略過 reasoning 的能力限制被明確斷言，不能宣稱 replay 已支援 |
| Admission | captured wire 與 estimate 內容對照；中文／emoji／大 schema／長 history／structured output；容量邊界；Responses 媒體零 generation |
| 結算 | cached／reasoning／missing／zeros；raw → SDK → ledger 正規化；每 invocation 至多 settle 一次 |
| 異常 | incomplete／failed／EOF／cancel／consumer stop，無不完整工具執行；HTTP 次數符合原 quota recovery，無跨協定重播 |
| Resume | 新 explicit binding、協定變更、同 provider 不同 target、所有受支援歷史 snapshot、selected dependency view、動態 target |
| 結果契約 | runtime/tool collector、runtime-owned fields、防 override、bounded repair、持久化與 replay，包含空／混合來源 |
| 診斷 | doctor/team check 顯示 requested／effective protocol、source、binding version；不發 generation、不暴露 key／prompt |

### 執行命令與資源規則

從 repository root 執行，沿用 `.mise.toml`、`GOTOOLCHAIN=local`、既有 caches 與 `GOTMPDIR`。以下是實作驗收命令；實際結果與分階段重用的驗證記錄見 implementation evidence。

- 建立唯一私人 task scratch：`mktemp -d /tmp/codex-task.XXXXXX`。將其記為 `task_tmp`，各命令傳入 `TMPDIR="$task_tmp"`。不覆寫 `GOTMPDIR` 或另建 caches。
- heavyweight commands 循序執行，前後記錄 scratch／GOCACHE／GOTMPDIR 用量、相關 volumes free space 與 available RAM；遵守根目錄 AGENTS.md 的容量、cache provenance、清理規則。
- fixture 使用 `t.TempDir()`；Unix sockets／PTY 使用 repository 既有短路徑 helper。子程序須先關閉再清理。

```bash
# task_tmp 已依上述規則建立；跨命令期間由同一 coding agent 負責清理。
TMPDIR="$task_tmp" GOMAXPROCS=2 mise exec -- gofmt -w <changed-go-files>
TMPDIR="$task_tmp" GOMAXPROCS=2 mise exec -- go test -p 2 -timeout 20m ./internal/config ./internal/agent ./internal/providerproxy ./internal/scripted ./internal/sidecar
TMPDIR="$task_tmp" GOMAXPROCS=2 mise exec -- go test -p 2 -timeout 20m ./internal/team ./cmd/hufu
TMPDIR="$task_tmp" GOMAXPROCS=2 mise exec -- go test -race -p 2 -timeout 90m ./internal/agent ./internal/providerproxy ./internal/team
TMPDIR="$task_tmp" GOMAXPROCS=2 mise run test-contracts
TMPDIR="$task_tmp" GOMAXPROCS=2 mise run test
TMPDIR="$task_tmp" GOMAXPROCS=2 mise run vet
TMPDIR="$task_tmp" GOMAXPROCS=2 mise run lint
TMPDIR="$task_tmp" GOMAXPROCS=2 mise exec -- go build -p 2 -o /dev/null ./cmd/hufu
```

檢查結果依未變動 sources 重用，不重跑已成功的 heavyweight command。不得與 live agent-team run 同時跑 full tests／race／vet／lint。失敗須修正並重跑受影響 gate；lint 必須成功。完成時清除 task-owned disposable outputs、記錄 cache growth／回收量，保留 deliverables。

## 10. 診斷、文件與完成定義

- `doctor`／`team check` 在現有輸出中顯示 requested protocol（空值顯示 legacy）、effective protocol、source 和 binding version；延用既有 metadata checks，不新增 generation probe。
- 錯誤提供實際選用協定及修正配置的指引；不能將任意 400／404 宣稱為端點不支援 Responses 的確證。
- 使用文件包含 canonical／legacy 配置範例、文字／工具支援範圍、Responses 媒體拒絕、SDK reasoning effort 限制、無 reasoning replay、context 估算語意與 resume drift。
- 協定切換範例只針對新 session；既有 frozen session 改配置後應被拒絕。文件介紹現有 `--new` 行為，但實作／驗收不能刪除操作者的既有 workspace。
- 完成條件為 WP 0–6 全部交付、離線矩陣與所有 required gates 成功、無遺留開發期 Responses gate、legacy wire／hash 行為不變，以及沒有新的 boundary bypass／tool permission 擴張。
- 交付報告只能宣稱「離線契約及整合驗證完成」，不得宣稱真實 OpenAI 端點或任意相容服務已驗證。真實端點驗證不列入待辦或驗收；使用者已於本次對話選擇之後另行授權。

## 11. 可追溯來源

本地程式依 §1 查核基準；實作前確認當前檔案，不能沿用舊行號或舊 snapshot version 清單。

- Hufu：`go.mod`、`internal/config/config.go`、`cmd/hufu/backend_config.go`、`internal/agent/{agent,provider_ref,provider_request,reasoning_options,tool_call_index,provider_usage_observation}.go`、`internal/team/{token_counter,execution_policy_snapshot,result_transport}.go`、`internal/scripted/{provider,boundary}.go`。
- [Fantasy v0.45.5 openaicompat](https://github.com/charmbracelet/fantasy/blob/v0.45.5/providers/openaicompat/openaicompat.go)：Responses 啟用 options。
- [Fantasy v0.45.5 Responses encoder](https://github.com/charmbracelet/fantasy/blob/v0.45.5/providers/openai/responses_language_model.go)：`prepareParams`、`getResponsesModelConfig`、`toResponsesPrompt`、`responsesUsage` 與 stream terminal 行為。
- [Fantasy v0.45.5 Responses options](https://github.com/charmbracelet/fantasy/blob/v0.45.5/providers/openai/responses_options.go)：options 型別與 public model heuristics。
- [OpenAI Responses migration](https://developers.openai.com/api/docs/guides/migrate-to-responses)：function schema、storage 與 stateless history。
- [OpenAI reasoning guide](https://developers.openai.com/api/docs/guides/reasoning)：reasoning items 往返及 reasoning/output token 語意；首版無 reasoning replay，不宣稱涵蓋全部建議能力。

## Unpublished release-note draft

Added explicit `api-protocol: responses` for OpenAI-compatible LLM backends.
Workers, coordinator and auxiliary models share the same provider-owned
selection. Omitted configuration retains legacy Chat Completions behavior;
explicit selection and its version are frozen for resume admission.

Responses supports text and local function calls, conservative context
admission, usage-presence accounting and terminal-verified streaming tool
execution. Doctor and team check expose the selection without generation
probes. The pinned SDK's reasoning and content limitations are documented in
[Provider API protocol](../reference/api-protocol.md).

Validation uses fake HTTP and offline integration tests. Real OpenAI endpoint
verification and external publication are outside this draft's acceptance scope.
