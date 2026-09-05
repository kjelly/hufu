# Hufu Model Metadata & Runtime Introspection Specification

**Status:** Proposed — verified against codebase 2026-09-03
**Target:** hufu
**Primary goal:** Replace fragile static model assumptions with catalog-backed metadata plus provider/runtime introspection.
**Audience:** Coding agents / maintainers
**Language:** English identifiers and API names; explanatory text in Traditional Chinese.

> 2026-09-03 審查備註：本規格對「現況」的描述已逐項比對實際程式碼
> （`internal/team`、`internal/agent`、`cmd/hufu`）並確認準確；§1、§3.1、
> §13、§14、§17、§18.1 已補上精確的 file:line 依據與少量現況澄清，其餘
> 內容未變動。建議實作順序仍照 §30，第一個 PR 聚焦 §1 item 2（`IsEstimated`
> 誤判導致 probe 被跳過）與 Ollama native `/api/show`/`/api/ps`。

---

## 1. 背景

目前 hufu 已有 `ModelContextSpec`、context admission、provider metadata probe、runtime overflow learning 等機制，但模型參數的來源仍不夠可靠，尤其是 Ollama。

目前主要問題：

1. hufu 內建部分模型 family 的靜態 context window，例如 `qwen3 = 128K`
   （`internal/team/token_counter.go` `initDefaults()`，共 8 個 exact-match
   entry：`gpt-4o`、`gpt-4`、`claude-3-5-sonnet`、`claude-3-7-sonnet`、
   `qwen2.5`、`qwen3`、`llama3.1`、`llama3.2`；另有一組 substring fallback
   matcher在 `GetSpec()` 內，對未登記模型比對 `claude`/`qwen`/`llama`/
   `gpt-4`|`o1`|`o3`，這組已正確標記 `IsEstimated = true`，不受本項影響）。
2. 上述 8 個 exact-match entry **沒有設定 `IsEstimated`**，Go zero value 為
   `false`，因此被視為「非估計值」；`DetectAndCacheProviderContextLengths`
   （`token_counter.go:220`）內 `if !spec.IsEstimated { continue }` 導致這些
   entry 永遠跳過 provider metadata probe。此為已驗證存在的真實 bug，非假設。
3. OpenAI-compatible `/v1/models` 不一定會提供 `context_length`、`max_context_window`、`max_input_tokens`。
4. Ollama 真正有價值的資料位於 native API：
   - `POST /api/show`
   - `GET /api/ps`

   目前 hufu **完全沒有**呼叫這兩支 API（`grep` 全庫為 0 命中）。且這不是
   「尚未實作」，而是**曾被刻意收斂掉**：`internal/agent/agent.go:875-877`
   與 `:957-961` 的註解明確寫著 `DetectOllamaContextLength`／
   `OllamaShowContextTimeout` "is retained for source compatibility" 並
   "now delegates to the provider-neutral /models metadata probe"——也就是
   函式名字仍叫 Ollama，但實際邏輯已經是純 OpenAI-compatible `/v1/models`
   探測，沒有可以直接復用的 native 呼叫邏輯。
5. 模型的「理論最大 context」不等於 backend 的「實際可用 context」。
6. hufu 尚未統一管理：
   - context window
   - max output tokens
   - tool calling
   - vision / attachment
   - reasoning
   - temperature support
   - model family
7. hufu 自行維護完整 model table 不具可持續性。

因此需要建立一個完整的 **Model Metadata + Runtime Introspection** 子系統。

---

## 2. 設計目標

### 2.1 必須達成

hufu 應能針對實際使用模型，自動建立有效的 model profile，至少包含：

- Model ID
- Model family
- Theoretical/static context window
- Provider advertised context window
- Configured context window
- Runtime effective context window
- Maximum output tokens
- Tool-calling support
- Attachment / vision support
- Reasoning support
- Temperature support
- Metadata source / provenance
- Confidence / authority level

對 Ollama，hufu 必須優先利用 native API，而不是只依賴 `/v1/models`。

### 2.2 不應達成

本規格不要求：

- hufu 自己成為完整模型資料庫網站。
- hufu 自行維護所有廠商的模型清單。
- hufu 在 runtime 強制修改 Ollama 的 `num_ctx`。
- hufu 以 benchmark 自動決定最佳 temperature/top-p。
- hufu 將模型理論能力錯當 backend 實際能力。

---

## 3. 核心原則

### 3.1 Static metadata 與 runtime metadata 必須分離

以下三個概念不可混用：

```text
ModelMaxContext
    模型架構或 catalog 所知的理論最大能力

ConfiguredContext
    backend / model configuration 所設定的 context

EffectiveContext
    本次 runtime 真正允許使用的 context
```

Context admission 必須使用：

```text
EffectiveContext
```

不能直接使用：

```text
ModelMaxContext
```

**現況說明（避免誤解為從零開始）：** 目前 `internal/team/provider_admission.go`
的 `AdmitProviderRequest` 只讀取單一 `spec.ContextWindow` 欄位做 admission，
表面上match「概念混用」的疑慮。但實務上 `ContextWindow` 這個欄位本身會被
provider probe（`token_counter.go:236`）與 runtime-overflow 觀測
（`token_counter.go:260-273` `RegisterObservedContextWindow`）覆寫，所以現況
已經有一個「事實上的 effective context」在運作，只是：

- 沒有把 static / configured / runtime 三個來源建模成獨立、具名的欄位；
- 沒有明確的 authority ordering（覆寫順序目前隱含在呼叫順序裡，不是資料模型的一部分）；
- 無法同時保留「這個模型理論上多大」與「這次 runtime 實際可用多少」兩個數字。

因此本規格的工作是**把既有的隱含覆寫邏輯顯性化、加上 provenance**，而不是建立一個全新的機制。

---

## 4. Metadata Sources

hufu 應支援以下 metadata source。

### 4.1 Operator override

最高優先權。

來源例如：

```bash
hufu --context-window 65536
```

或 team config：

```yaml
generation:
  context-window: 65536
```

這個值只代表：

```text
Hufu operator-declared effective admission capacity
```

除非 provider adapter 明確支援 runtime reconfiguration，否則 **不得假設 backend 已被修改**。

---

### 4.2 Provider runtime introspection

Provider-specific introspector 應具有比 static catalog 更高的權威。

#### Ollama

使用：

```text
POST /api/show
GET  /api/ps
```

#### Generic OpenAI-compatible

使用：

```text
GET /v1/models
```

並檢查常見欄位：

```text
context_length
max_context_window
max_input_tokens
max_output_tokens
max_completion_tokens
```

---

### 4.3 Model catalog

建議直接同步：

```text
https://models.dev/api.json
```

資料可包含：

- model id
- name
- family
- attachment
- reasoning
- tool_call
- temperature
- context limit
- output limit
- pricing

Catwalk 可作為 cross-check 或 secondary reference，但不應成為 hufu 的必要 runtime dependency。

---

### 4.4 Runtime observation

若 provider 回報 context overflow，hufu 可從錯誤訊息學習有效 context window。

這只能是 fallback / corrective source，不應是主要 discovery mechanism。

---

### 4.5 Conservative fallback

未知模型、未知 backend、metadata 不完整時使用。

Fallback 必須：

- 明確標記 `estimated`
- 不得標記為 exact
- context admission 採保守策略
- 產生可觀察 telemetry

---

## 5. 建議架構

```text
                  +------------------+
                  | Operator Config  |
                  +--------+---------+
                           |
                           v
+-------------+    +----------------------+    +----------------------+
| models.dev  | -> | Model Profile Resolver| <- | Provider Introspector|
| static data |    +----------+-----------+    +----------+-----------+
+-------------+               |                           |
                              |                           |
                              v                           v
                     +------------------+         +----------------+
                     | Effective Model  |         | Runtime Backend |
                     | Profile Registry |         | Ollama / APIs   |
                     +---------+--------+         +----------------+
                               |
                               v
                     +--------------------+
                     | Context Admission  |
                     | Tool Capability    |
                     | Model Routing      |
                     +--------------------+
```

---

## 6. Data Model

建立新的 canonical type：

```go
type ModelProfile struct {
    ModelID  string
    Provider string

    Family string

    // Catalog / theoretical limits.
    ModelMaxContext int
    MaxOutputTokens int

    // Provider configuration/runtime.
    ProviderContext   int
    ConfiguredContext int
    RuntimeContext    int
    EffectiveContext  int

    SupportsTools       CapabilityState
    SupportsAttachments CapabilityState
    SupportsReasoning   CapabilityState
    SupportsTemperature CapabilityState

    Sources ModelProfileSources
}
```

### 6.1 CapabilityState

不可只用 `bool`，因為 unknown 與 false 不同。

```go
type CapabilityState string

const (
    CapabilityUnknown CapabilityState = "unknown"
    CapabilityYes     CapabilityState = "yes"
    CapabilityNo      CapabilityState = "no"
)
```

---

### 6.2 Source metadata

```go
type MetadataSource string

const (
    SourceOperator         MetadataSource = "operator"
    SourceProviderRuntime  MetadataSource = "provider_runtime"
    SourceProviderMetadata MetadataSource = "provider_metadata"
    SourceModelConfig      MetadataSource = "model_config"
    SourceCatalog          MetadataSource = "catalog"
    SourceObserved         MetadataSource = "provider_observed"
    SourceFallback         MetadataSource = "fallback"
)
```

```go
type ResolvedValue[T any] struct {
    Value      T
    Source     MetadataSource
    Confidence string
}
```

至少對 context 相關欄位應保留 source。

---

## 7. Effective Context Resolution

Context resolution 必須有明確 authority ordering。

### 7.1 Ollama

```text
1. Operator explicit override
2. /api/ps runtime context_length
3. /api/show parameters -> num_ctx
4. /api/show model_info -> *.context_length
5. Generic /v1/models metadata
6. Runtime observed context-overflow limit
7. models.dev / catalog context
8. Conservative fallback
```

### 7.2 Generic provider

```text
1. Operator explicit override
2. Provider runtime metadata
3. Provider model metadata
4. Runtime observed context-overflow limit
5. Catalog model metadata
6. Conservative fallback
```

---

## 8. Ollama Introspector

新增：

```go
type ModelIntrospector interface {
    InspectModel(
        ctx context.Context,
        provider ProviderRef,
        modelID string,
    ) (RuntimeModelInfo, error)
}
```

Ollama implementation：

```go
type OllamaIntrospector struct {
    BaseURL string
    Client  *http.Client
}
```

### 8.1 `/api/show`

Request：

```json
{
  "model": "qwen3:8b"
}
```

應解析至少：

```text
parameters
model_info
capabilities
details.family
details.parameter_size
details.quantization_level
```

### 8.2 `parameters`

`parameters` 可能為文字：

```text
temperature 0.7
num_ctx 32768
```

必須實作 generic parser：

```go
func ParseOllamaParameters(raw string) map[string]string
```

至少辨識：

```text
num_ctx
temperature
top_p
top_k
num_predict
```

`num_ctx` 對 context resolution 有直接權威。

其他 generation parameters 可以先保存 metadata，但本規格不要求自動覆蓋使用者設定。

---

### 8.3 `model_info`

不能 hardcode 單一 key，例如：

```text
qwen3.context_length
```

應掃描所有 key：

```go
func FindOllamaContextLength(modelInfo map[string]any) (int, bool)
```

匹配：

```text
*.context_length
```

若有多個 context field：

- 取最明確的 architecture-level context field
- 若無法唯一判定，標記 ambiguous，不直接覆蓋較高權威來源

---

### 8.4 `capabilities`

映射至少：

```text
tools       -> SupportsTools
vision      -> SupportsAttachments
completion  -> basic completion capability
thinking    -> SupportsReasoning
```

未知 capability 必須保留，以便未來擴充。

```go
type RuntimeModelInfo struct {
    ModelID string

    ConfiguredContext int
    ModelMaxContext   int

    Capabilities []string

    Family       string
    ParameterSize string
    Quantization string

    Raw map[string]any
}
```

---

## 9. `/api/ps` Runtime Detection

`GET /api/ps` 用於取得目前已載入模型狀態。

必須匹配 model name。

可解析：

```text
name
model
context_length
size
size_vram
expires_at
```

當模型已載入，`context_length` 應視為比 `/api/show` 的 theoretical/configured metadata 更高權威。

### 9.1 模型尚未載入

若 `/api/ps` 找不到模型：

- 不視為 error
- 使用 `/api/show`
- profile 標記 `RuntimeContext = 0`
- EffectiveContext 退回下一權威來源

禁止為了 introspection 而主動 preload 模型，除非未來有明確 opt-in 設定。

---

## 10. Model Catalog

新增 package，例如：

```text
internal/modelcatalog/
```

建議：

```text
catalog.go
models.go
update.go
embedded_models.json
```

### 10.1 Catalog schema

hufu 只保留必要欄位：

```go
type CatalogModel struct {
    Provider string `json:"provider"`
    ID       string `json:"id"`
    Name     string `json:"name"`
    Family   string `json:"family"`

    Attachment bool `json:"attachment"`
    Reasoning  bool `json:"reasoning"`
    ToolCall   bool `json:"tool_call"`
    Temperature bool `json:"temperature"`

    Context int `json:"context"`
    Output  int `json:"output"`
}
```

### 10.2 Update command

增加：

```bash
hufu models update
```

或：

```bash
hufu model update
```

建議統一使用：

```bash
hufu models update
```

行為：

1. Download `https://models.dev/api.json`
2. Validate JSON
3. Normalize needed fields
4. Write local cache
5. Atomic replace
6. Preserve embedded snapshot as fallback

---

### 10.3 Offline behavior

Runtime 不應要求 Internet。

啟動時：

```text
local cache
    ↓
embedded snapshot
    ↓
fallback
```

`models update` 才需要外網。

---

## 11. Catwalk Integration Policy

Catwalk 不應成為必要 runtime dependency。

原因：

1. Catwalk 核心 model type 很適合參考。
2. provider configs 位於 `internal` package，hufu 無法直接 import。
3. Catwalk client 預設需要 HTTP service。
4. 對 Ollama 沒有 native provider catalog。
5. hufu 已可直接使用 models.dev 作主要 catalog。

Catwalk 可用於：

- cross-check generated metadata
- CI validation
- model capability reference
- provider-specific edge cases

不建議：

```text
hufu -> mandatory Catwalk HTTP service
```

---

## 12. Kit Integration Policy

不直接依賴整個 Kit。

可借鏡：

- models.dev → embedded snapshot
- runtime local cache override
- ModelInfo / Limit abstraction
- unknown model tolerance
- explicit model capability API

hufu 應自行建立較小的 model catalog package。

---

## 13. Existing `ModelContextSpec` Migration

目前：

```go
type ModelContextSpec struct {
    ModelID
    ContextWindow
    ContextWindowSource
    MaxOutputTokens
    SafetyMarginTokens
    Estimator
    IsEstimated
}
```

應逐步改成由 `ModelProfile` 派生。

過渡期可：

```go
func (p ModelProfile) ToContextSpec() ModelContextSpec
```

避免一次改動所有 admission code。

**Blast radius（實作前務必確認）：** `ModelContextSpec` 是具體型別（非
interface），直接被 `internal/team` 底下約 198 個非測試檔案，以及
`cmd/hufu/{report.go, model_overrides.go, display.go, team_setup.go}`
使用。這代表上面的 adapter 過渡策略**不是保守選項，而是唯一可行路徑**——
大爆炸式替換會同時觸及全部這些檔案。第一版實作不應嘗試移除
`ModelContextSpec`，只需讓 `ModelProfile` 成為新的權威來源、透過
`ToContextSpec()` 餵給既有 admission code。

---

## 14. Static Registry Policy

目前硬編碼：

```text
qwen2.5
qwen3
llama3.1
llama3.2
...
```

不得再視為 exact provider capacity。

處理方式：

### Phase 1

保留 registry，但所有 family-based entry：

```text
IsEstimated = true
Source = catalog/fallback
```

因此 **不得阻止 provider introspection**。

### Phase 2

model catalog 穩定後移除大部分硬編碼模型 entry。

只保留：

- bootstrap defaults
- tokenizer estimator mapping
- emergency conservative fallback

---

## 15. Max Output Tokens

`MaxOutputTokens` 的 authority order：

```text
1. Per-call explicit max output
2. Agent/team/CLI explicit max output
3. Provider runtime/model metadata
4. models.dev catalog output limit
5. Existing safe default
```

禁止將 catalog 的 theoretical output limit 自動視為每次 request 都應使用的 output token 數。

它只代表：

```text
upper bound
```

實際 request 可更低。

---

## 16. Tool / Vision / Reasoning Capability

ModelProfile 應成為 tool exposure 的參考訊號。

例如：

```text
SupportsTools = no
```

且 agent 要求 tool calling 時：

- 啟動時產生 clear warning 或 validation error
- 不要等第一次 tool call 才失敗

但 capability metadata 不是 authorization。

必須維持：

```text
Model capability != Hufu tool permission
```

也就是：

```text
模型支援 tool calling
        +
agent/team policy 允許 tool
        =
tool 才可暴露
```

---

## 17. Model Validation

目前 hufu 已有 model existence validation，但強度不一，實作前應知道現有兩處：

- `internal/team/model_validation.go` 的 `ValidateConfiguredModels`：對每個
  configured model 呼叫 provider 的 `/models` 清單做比對，找不到時提供相近
  拼字建議（typo suggestion）；provider 無法查詢時視為「無法驗證」而非
  「不存在」，會跳過而非報錯。
- `cmd/hufu/team_loader.go` 的 `resolveAndCheckModel`：較弱的一層，主要檢查
  model 名稱字串非空。

兩者都只驗證「model ID 是否存在／拼對」，**完全不驗證 capability**（是否支援
tools/vision/reasoning）。本節要擴充的正是這一塊，應擴充為：

```text
Model existence
Model effective context
Required tool capability
Required attachment capability
Required reasoning capability
```

例如 agent requirement：

```yaml
requirements:
  model:
    tools: true
    reasoning: true
```

未來可由 ModelProfile 驗證。

本規格第一階段不強制加入 YAML schema，但 ModelProfile 必須為此做好結構準備。

---

## 18. Startup Flow

建議 team setup：

```text
Load team
   ↓
Resolve provider/model IDs
   ↓
Collect models in use
   ↓
Load catalog metadata
   ↓
Run provider introspection
   ↓
Merge operator overrides
   ↓
Build ModelProfile
   ↓
Register effective profile
   ↓
Validate model/capabilities
   ↓
Create coordinator
```

### 18.1 no-net

`--no-net` 的 provider semantics 需要區分：

- 禁止 Internet
- 是否禁止 localhost provider introspection

目前 hufu 將 `no-net` 直接禁止 context detection：`cmd/hufu/team_setup.go`
的 `detectContextLengths`（約 36-41 行）在 `noNet == true` 時直接
`return`，不做任何 localhost/remote 區分；同檔案約 199-201 行的註解明確寫著
「in no-net mode even this best-effort probe is forbidden」，確認這是刻意設計而非疏漏，因此需要一個新的、明確的政策取代它，而不是單純修 bug。

建議新增明確政策：

```text
no-net = block external network
local provider introspection = allowed by default
```

若 hufu 現有 security semantics 將 localhost 也視為 network，則至少加入：

```yaml
provider-introspection: false
```

或 CLI：

```bash
--no-provider-introspection
```

不可模糊。

---

## 19. Caching

Runtime introspection 不應每一個 request 重複呼叫。

Cache key：

```text
provider base URL
provider identity
model ID
```

至少使用 process-level cache。

建議：

```go
type ModelProfileCache struct {
    ...
}
```

### 19.1 TTL

Static catalog：

```text
直到 models update 或 process restart
```

Ollama `/api/show`：

```text
process lifetime or moderate TTL
```

Ollama `/api/ps`：

```text
short TTL
```

因為 runtime context allocation 可能因 reload 改變。

建議預設：

```text
/api/show: 10 min
/api/ps:   5-30 sec
```

具體值可實作時調整。

---

## 20. Runtime Refresh

在以下情況 refresh runtime profile：

1. First use
2. Model switch
3. Retry after context overflow
4. Backend reconnect
5. Model reload detected
6. Explicit diagnostics command

例如：

```bash
hufu models inspect ollama/qwen3:8b
```

輸出：

```text
Model: ollama/qwen3:8b
Family: qwen3

Catalog context:       131072
Ollama model context:  131072
Configured num_ctx:     65536
Runtime context:        32768
Effective context:      32768

Tools:       yes
Vision:      no
Reasoning:   yes
Temperature: yes

Effective source: ollama:/api/ps
```

此命令非常建議納入第一版，方便除錯。

---

## 21. Telemetry

每次 resolved profile 應至少可觀察：

```json
{
  "model_id": "ollama/qwen3:8b",
  "provider": "ollama",
  "effective_context": 32768,
  "effective_context_source": "provider_runtime",
  "catalog_context": 131072,
  "configured_context": 65536,
  "runtime_context": 32768,
  "max_output_tokens": 8192,
  "supports_tools": "yes",
  "supports_reasoning": "yes"
}
```

不應記錄：

- API key
- bearer token
- secret provider header
- full unredacted provider response if it might contain secrets

---

## 22. Error Handling

Provider introspection 必須是 bounded。

所有 HTTP call：

- context timeout
- body size limit
- status validation
- JSON validation

若 introspection 失敗：

```text
provider unavailable
    ↓
catalog
    ↓
fallback
```

只有當 admission 無法得到安全正值且策略要求 fail-closed 時才阻止執行。

---

## 23. Security

Ollama introspector：

- 不接受任意 redirect 到未授權網域
- 遵守既有 provider URL policy
- request timeout
- response body upper bound
- 不 log API key
- raw metadata 若保存必須經過 redaction policy

---

## 24. Testing

### 24.1 Unit Tests

必須覆蓋：

```text
ParseOllamaParameters
FindOllamaContextLength
/api/show parser
/api/ps parser
context source precedence
capability tri-state merge
catalog lookup
unknown model fallback
```

### 24.2 Precedence tests

至少：

```text
operator > runtime
runtime > configured
configured > model_info
model_info > catalog
catalog > fallback
```

### 24.3 Ollama test cases

#### Case A

```text
catalog = 131072
show.num_ctx = 65536
ps.context_length = 32768
```

預期：

```text
EffectiveContext = 32768
```

#### Case B

```text
catalog = 131072
show.num_ctx = 65536
ps missing model
```

預期：

```text
EffectiveContext = 65536
```

#### Case C

```text
show.num_ctx absent
show.model_info qwen.context_length = 131072
```

預期：

```text
EffectiveContext = 131072
Source = model_config/provider_metadata
```

#### Case D

```text
all provider introspection unavailable
catalog = 131072
```

預期：

```text
EffectiveContext = 131072
Source = catalog
Confidence != exact-runtime
```

#### Case E

```text
all metadata unavailable
```

預期：

```text
conservative fallback
IsEstimated = true
warning/telemetry emitted
```

---

## 25. Integration Tests

使用 httptest 模擬 Ollama：

```text
/api/show
/api/ps
/v1/models
```

驗證：

1. team startup 可建立 profile
2. known static qwen/llama model 仍會 introspect
3. provider runtime context 會覆蓋 catalog
4. operator override 可覆蓋 runtime
5. context admission 使用 EffectiveContext
6. overflow observation 可觸發 refresh / correction

---

## 26. Regression Tests

必須確保：

- Existing OpenAI-compatible providers 仍可工作
- Existing `--context-window` 行為保持相容
- Existing context overflow parser 保留
- Existing context telemetry 不遺失
- Existing coordinator admission 不因 catalog 缺少模型而崩潰
- `--no-net` 行為依新定義有測試
- Unknown custom provider 仍可使用 conservative fallback

---

## 27. Migration Plan

### Phase 1 — Foundation

新增：

```text
internal/modelcatalog
internal/modelprofile
internal/providerintrospection
```

建立：

```go
ModelProfile
ModelIntrospector
ProfileResolver
```

保留舊 `ModelContextSpec` adapter。

---

### Phase 2 — Catalog

導入：

```text
models.dev/api.json
```

加入：

```bash
hufu models update
hufu models inspect
```

將 static registry 降級為 estimated fallback。

---

### Phase 3 — Ollama

實作：

```text
POST /api/show
GET /api/ps
```

Context admission 改為使用 EffectiveContext。

---

### Phase 4 — Capabilities

將：

```text
tool_call
attachment
reasoning
temperature
```

納入 model validation / routing signal。

---

### Phase 5 — Cleanup

移除大部分 handcrafted model context table。

只保留：

- tokenizer estimator mapping
- fallback defaults
- compatibility aliases

---

## 28. Acceptance Criteria

功能完成必須符合：

### AC-1

對：

```text
ollama/qwen3:*
```

hufu 不得因 static `qwen3` registry entry 而跳過 provider introspection。

### AC-2

若 Ollama `/api/ps` 回報：

```text
context_length = 32768
```

則 context admission 使用：

```text
32768
```

而非 catalog / static 128K。

### AC-3

若 `/api/ps` 無模型但 `/api/show parameters` 有：

```text
num_ctx 65536
```

則：

```text
EffectiveContext = 65536
```

### AC-4

若 Ollama native API 都無法取得有效資料，才 fallback 至 catalog。

### AC-5

模型 catalog 不得要求 runtime Internet access。

### AC-6

`models.dev` 的：

```text
tool_call
attachment
reasoning
temperature
limit.context
limit.output
```

不得在 normalization 過程被丟棄。

### AC-7

每個 EffectiveContext 必須能說明 provenance。

例如：

```text
operator
provider_runtime
model_config
provider_metadata
catalog
observed
fallback
```

### AC-8

未知模型必須能執行，不應因不在 catalog 直接拒絕；但 metadata 必須標示 unknown/estimated。

### AC-9

現有 context overflow runtime learning 保留。

### AC-10

新增功能不得使 Catwalk 或 Kit 成為 hufu runtime mandatory dependency。

---

## 29. Recommended File Layout

```text
internal/
  modelcatalog/
    catalog.go
    models.go
    embedded_models.json
    update.go
    normalize.go
    catalog_test.go

  modelprofile/
    profile.go
    resolver.go
    cache.go
    source.go
    resolver_test.go

  providerintrospection/
    introspector.go
    openai_compat.go
    ollama.go
    ollama_parser.go
    ollama_test.go
```

若 maintainer 希望避免 package 過度拆分，也可合併為：

```text
internal/modelmeta/
```

但 **catalog knowledge 與 runtime introspection 邏輯仍需保持明確分層**。

**檔案大小提醒：** `internal/team/coordinator_task_run.go` 目前已達 4463
行，遠超 `CLAUDE.md` 訂的 800 行上限。第 4 階段（capability validation）
的邏輯**不得**加進這個檔案，應放進上方新建的 `internal/modelprofile` 或
`internal/providerintrospection`，或 `internal/team` 底下的獨立新檔案。

---

## 30. Suggested Initial Implementation Order

Coding agent 建議依序：

```text
1. ModelProfile + source/provenance
2. Adapter to existing ModelContextSpec
3. Static registry 改成 estimated
4. Ollama /api/show
5. Ollama /api/ps
6. EffectiveContext resolver
7. Wire into startup/admission
8. models.dev embedded catalog
9. models update
10. models inspect
11. capability validation
12. cleanup old registry
```

優先解決：

```text
Ollama effective context correctness
```

再擴展完整 model capability registry。

---

## 31. Source References

設計參考：

- hufu: https://github.com/kjelly/hufu
- Catwalk: https://github.com/charmbracelet/catwalk
- Kit: https://github.com/mark3labs/kit
- models.dev: https://models.dev
- Ollama API show: https://docs.ollama.com/api-reference/show-model-details
- Ollama API ps: https://docs.ollama.com/api/ps

Catwalk 提供的模型 metadata 結構值得參考：

```text
ContextWindow
DefaultMaxTokens
CanReason
ReasoningLevels
SupportsImages
ModelOptions
pricing
```

Kit / models.dev 提供的 schema 值得直接保留：

```text
family
attachment
reasoning
tool_call
temperature
limit.context
limit.output
```

---

## 32. Final Design Decision

hufu 應採用：

```text
models.dev catalog
        +
provider-specific runtime introspection
        +
operator override
        +
runtime observation
        =
Effective Model Profile
```

其中 Ollama 必須實作 native introspection：

```text
/api/show
/api/ps
```

Catwalk 與 Kit 應作為設計與 metadata source 的參考，不應成為 hufu 的必要 runtime dependency。

核心原則：

> hufu 應知道「這個模型理論上能做什麼」，更要知道「這個 backend 現在實際允許它做什麼」。
