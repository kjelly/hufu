# hufu DecisionPrimitive 規格

> Status: active
> Authority: normative
> Verified-Commit: `4e40ed0`
> Supersedes: —
> Superseded-By: —
> Target: implemented runtime contract
> Baseline repository: `kjelly/hufu`
> Baseline branch: `main`
> Baseline commit: `e02a7ef`
> Scope: small, backend-agnostic typed decision primitive covering Phase 0–3
> below only (core, sidecar adapter, standalone CLI).
> Non-goal: reimplement any specific third-party logits-based decision
> technique, or replace hufu's existing DecisionEngine
> Relationship: distinct from and does not modify
> `docs/architecture/decision-runtime.md`（既有 Decision-Aware Runtime /
> DecisionEngine 規格，normative、已大量實作）。本文件的產物一律稱為
> **DecisionPrimitive**，不得在程式碼註解、PR 標題或文件中簡稱為
> 「decision runtime」，以避免與該既有系統混淆。
>
> 新增程式碼註解引用本規格時，一律使用檔名
> `docs/architecture/decision-primitive.md`，不得寫 bare `spec.md`
> （本 repo 過去已因 `spec.md` 檔名重複覆寫造成程式碼註解指向錯誤規格，
> 見 `docs/architecture/decision-runtime.md` 開頭的檔名說明）。

---

## 1. Objective

新增一個小型、通用、backend-agnostic（後端無關）的 `DecisionPrimitive`，讓 hufu 可以對有限答案集合做 bounded typed decision（型別化的有界決策）。MVP 的 backend 是 deterministic rule 與既有 sidecar 模型。可決策的形狀例如：

- choice：`small | medium | large`
- boolean：`true | false`
- bounded integer score：例如 `0..4`

`DecisionPrimitive` 必須只產生受 `DecisionSpec` 約束的值，不得把任意自然語言當作決策結果。

核心定位：

```text
LLM      -> reasoning
Tool     -> action
Memory   -> state
Decision -> control
```

`DecisionPrimitive` 是 control-plane primitive（控制面原語），不是新的 agent、workflow 或 DecisionEngine。

---

## 2. Current architecture constraints

目前 hufu 已有完整的高階 `DecisionEngine`，相關實作主要位於：

```text
internal/team/decision_engine.go
internal/team/decision_runners.go
internal/team/decision_runtime_types.go
internal/team/decision_*...
internal/agent/decision_config.go
internal/agent/decision_profiles.go
internal/sidecar/sidecar.go
cmd/hufu/decisioncmd.go
```

現有 `DecisionEngine` 已負責：

- evidence sealing
- isolated judges
- deterministic aggregation
- challenge
- premortem
- revision
- finalization
- artifact persistence
- durable event/replay
- decision outcome/calibration data

本規格不得建立與上述功能重疊的第二套 decision engine。

### 2.1 Required layering

目標分層：

```text
+--------------------------------------------------+
| Existing DecisionEngine                          |
| evidence / judge / challenge / revision / audit |
+-------------------------+------------------------+

              no dependency or call in MVP

+--------------------------------------------------+
| DecisionPrimitive                                |
| choice / boolean / bounded score / abstain       |
+-------------------------+------------------------+
                          |
                  +-------+-------+
                  |               |
                  v               v
               rule           sidecar
              backend          backend
```

### 2.2 Hard boundary

MVP 禁止：

1. 取代 `DecisionEngine`。
2. 修改既有 judge/challenge/revision 的語意。
3. 讓 `DecisionPrimitive` 直接依賴 `internal/team`。
4. 讓 `DecisionPrimitive` 自己管理 session、artifact store、event store 或 task lifecycle。
5. 把任何特定 logits inference backend、llama.cpp、Ollama、Lemonade 或任一 provider 寫死在 core API。
6. 新增另一套 `DecisionRecord`。
7. 把 arbitrary rationale（任意推理文字）放入決策結果 contract。

---

## 3. Design principles

### P1. Typed output first

模型只能回傳規格允許的值。

錯誤：

```json
{"value":"maybe_retry_later"}
```

若合法 options 為：

```text
retry
escalate
abort
human
```

則上述結果必須被 runtime 拒絕。

### P2. Backend-independent contract

呼叫端只依賴：

```go
Runtime.Decide(...)
```

不得知道底層使用：

- deterministic rule
- existing sidecar

### P3. Decision is not explanation

Decision plane（決策面）與 explanation plane（解釋面）分離。

`DecisionPrimitive` 不應輸出：

```json
{
  "choice": "escalate",
  "reasoning": "I think...",
  "analysis": "..."
}
```

若未來需要解釋，必須由另一個 explicit explanation path 處理，不得污染 decision contract。

### P4. Uncertainty is a first-class result

低信心不是 technical error（技術錯誤）。

必須區分：

```text
DECIDED
ABSTAINED
ERROR
```

- `DECIDED`: 有合法結果。
- `ABSTAINED`: backend 有回應，但 runtime/policy 不接受。
- `ERROR`: transport、timeout、invalid protocol、backend crash 等技術失敗。

### P5. Deterministic system owns policy

模型可以提供候選分數或機率，但：

- threshold
- fallback
- allowed values
- timeout
- side-effect permission

全部由 Go runtime 決定。

### P6. Confidence semantics must be explicit

不得把未校準的 softmax/self-reported confidence 當成「實際正確率」。

結果必須標示：

```text
none
raw
calibrated
```

只有 `calibrated` 可以宣稱具 calibration semantics（校準語意）。

### P7. Explicit invocation only

MVP 不將 `DecisionPrimitive` 接入 coordinator、retry、routing、guard、
memory 或 `DecisionEngine`。只有 Go caller 明確呼叫 `Runtime.Decide`，
或使用者明確執行 `hufu decisionrt`時，才會呼叫 backend。因此新功能
對現有 hufu 執行路徑是零語意變化。

---

## 4. Package layout

新增：

```text
internal/decisionrt/
    architecture_test.go
    digest.go
    digest_test.go
    errors.go
    metrics.go
    runtime.go
    runtime_test.go
    types.go
    validate.go
    validation_test.go

    backend/
        rule/
            rule.go
            rule_test.go

        sidecar/
            sidecar.go
            sidecar_test.go

```

本規格不建立其他 backend 目錄，也不新增 Python 或外部推論服務依賴。

---

## 5. Core API

### 5.1 Decision kind

```go
package decisionrt

type Kind string

const (
    KindChoice       Kind = "choice"
    KindBoolean      Kind = "boolean"
    KindIntegerRange Kind = "integer_range"
)
```

MVP 僅支援以上三種。

不要在 MVP 支援：

- arbitrary string
- arbitrary JSON object
- free-form text
- floating-point continuous generation
- nested schema

這些會讓 primitive 重新退化成普通 structured-output LLM。

---

## 6. DecisionSpec

```go
type Option struct {
    ID          string `json:"id"`
    Description string `json:"description,omitempty"`
}

type IntegerRange struct {
    Min int64 `json:"min"`
    Max int64 `json:"max"`
}

type Spec struct {
    ID      string `json:"id"`
    Version string `json:"version"`

    Kind     Kind   `json:"kind"`
    Question string `json:"question"`

    Options []Option      `json:"options,omitempty"`
    Range   *IntegerRange `json:"range,omitempty"`
}
```

### 6.1 Validation

`Spec.Validate()` 必須 fail closed（失敗即拒絕）。

共同規則：

- 所有 string field 必須是 valid UTF-8。
- `ID`、`Version` 必須符合 ASCII machine-ID grammar
  `[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}`。
- `Question` trim 後不得為空，UTF-8 長度不得超過 4096 bytes；保留原始文字。
- `Kind` 必須已知。

`choice`：

- 2 到 21 個 option。
- option ID 必須符合相同 machine-ID grammar。
- option ID 必須 unique。
- option description 長度不得超過 1024 UTF-8 bytes。
- option ID 必須視為 opaque stable identifier，不得從 description 自動推導。
- 不得設定 `Range`。

`boolean`：

- 不得設定 `Options`。
- 不得設定 `Range`。

`integer_range`：

- 必須有 `Range`。
- `Min <= Max`。
- domain size 必須滿足數學上的 `Max-Min <= 20`（最多 21 個值）；計算時不得
  讓 `int64` overflow，超過時 fail closed。
- 不得設定 `Options`。

---

## 7. Request

```go
type Request struct {
    Purpose string `json:"purpose"`

    Spec Spec `json:"spec"`

    // Caller supplies only information needed for this bounded decision.
    Context map[string]any `json:"context,omitempty"`
}
```

`Request.Validate()` 必須先驗證 `Purpose` 符合相同 machine-ID grammar；再
呼叫 `Spec.Validate()` 與 §7.3 Context validation。

### 7.1 Purpose

`Purpose` 是 stable machine identifier，例如：

```text
size-policy@v1
model-route@v1
guard-disposition@v1
memory-promotion@v1
```

禁止使用自然語言當 Purpose。

用途：

- telemetry aggregation
- idempotency/digest

### 7.2 Context boundary

`DecisionPrimitive` 不得自行讀：

- workspace
- memory store
- current agent conversation
- environment
- filesystem
- network
- task journal

需要的資料必須由 caller 明確放入 `Context`。

這確保：

```text
same request -> same semantic input
```

並避免隱性 authority expansion（權限擴張）。

### 7.3 Context value domain

`Context` 的 canonicalization 規則必須明確、機械化，不得留給實作者自行決定：

1. 合法 value 型別只有：`string`、`bool`、所有 Go signed/unsigned
   integer 型別（值必須落在 `int64` 範圍）、finite `float32`/`float64`、
   以及 `json.Number`。`nil` 不合法，`Validate()` 必須拒絕。
   String value 另必須是 valid UTF-8。
2. 不支援 nested map 或 slice/array value；MVP 的 Context 必須是單層
   flat map。遇到 nested value 一律 fail closed。
3. CLI/JSON decoder 必須使用 `json.Decoder.UseNumber()`，禁止先將 JSON
   number 轉成 `float64` 而遺失大整數精度。`json.Number` 若可解為
   `int64` 就使用整數十進位表示；若 lexical form 不含 `.`/`e`/`E` 且
   `Int64()` 失敗，必須直接拒絕（不得轉 float）；其餘才解為 finite
   `float64`。無法解析者必須拒絕。
4. 數值 canonicalization：integer 用 base-10 整數字串；floating-point
   先拒絕 `NaN`/`Inf`，再用 `strconv.FormatFloat(f, 'g', -1, 64)`。
   若值是落在 `int64` 範圍內的數學整數，必須 canonicalize 為整數字串，確保
   `3`、`3.0`、`int64(3)` 和 `json.Number("3.0")` 的 digest 相同。
5. Generic JSON CLI 必須拒絕 duplicate object keys，不得依賴
   `encoding/json` 的 last-key-wins 行為。
6. Map key 順序不得影響 digest（沿用 §15 的要求）。
7. Context 最多 64 個 entries。Key 必須符合相同 machine-ID grammar；string
   value 長度不得超過 4096 UTF-8 bytes。
8. Canonicalized Context 的 JSON encoding 不得超過 64 KiB；超過就回
   `ErrorInvalidRequest`。

---

## 8. Typed value

禁止使用 unconstrained `any` 作為 runtime output。

型別固定為：

```go
type Value struct {
    Choice  string `json:"choice,omitempty"`
    Boolean *bool  `json:"boolean,omitempty"`
    Integer *int64 `json:"integer,omitempty"`
}
```

必須依 `Spec.Kind` 驗證 exactly one（恰好一個）有效欄位。

例如：

```json
{
  "choice": "escalate"
}
```

boolean：

```json
{
  "boolean": true
}
```

integer range：

```json
{
  "integer": 3
}
```

---

## 9. Candidate distribution

```go
type Candidate struct {
    Value       string  `json:"value"`
    Probability float64 `json:"probability"`
}

type ConfidenceSemantics string

const (
    ConfidenceNone       ConfidenceSemantics = "none"
    ConfidenceRaw        ConfidenceSemantics = "raw"
    ConfidenceCalibrated ConfidenceSemantics = "calibrated"
)
```

`Candidate.Value` 使用 canonical representation：

- choice: option ID
- boolean: `"true"` / `"false"`
- integer: decimal string，例如 `"3"`

規則：

- probability 必須 finite。
- probability 必須位於 `[0,1]`。
- `Candidates` 非空時一律代表完整 distribution；必須恰好包含
  domain 的每個合法值一次，不多也不少。
- 完整 distribution 的 probability 總和必須滿足
  `abs(sum-1) <= 1e-6`。
- backend 無法提供可靠 probability 時，必須回傳 nil/empty
  `Candidates`；不支援 partial distribution。
- 不得偽造 uniform probability。

---

## 10. Result

```go
type Status string

const (
    StatusDecided   Status = "decided"
    StatusAbstained Status = "abstained"
)

type Result struct {
    Status Status `json:"status"`

    Value Value `json:"value"`

    Candidates []Candidate `json:"candidates,omitempty"`

    Confidence          float64             `json:"confidence,omitempty"`
    ConfidenceSemantics ConfidenceSemantics `json:"confidence_semantics"`

    Backend string `json:"backend"`
    Model   string `json:"model,omitempty"`

    FallbackUsed bool `json:"fallback_used"`

    // Runtime-owned machine reason code only.
    ReasonCode string `json:"reason_code,omitempty"`
}
```

### 10.1 No model-authored rationale

`Result` 不得包含：

```text
reason
analysis
thoughts
rationale
explanation
```

MVP 的 `ReasonCode` 只能由 runtime 產生且固定為：

```text
low_confidence
backend_abstained
```

### 10.2 Result invariants

`Runtime` 回傳前必須驗證（fail closed，違反者一律轉成
`invalid_backend_output` 並觸發 §14 fallback，或若已是最後一手則回
`ERROR`）：

1. `Status == StatusAbstained` 時，`Value` 必須是零值（沒有任何欄位被設
   定），`Candidates` 必須為空、`ConfidenceSemantics` 必須是
   `ConfidenceNone`、`Confidence` 必須為 `0`。
2. `Status == StatusDecided` 時，`Value` 必須通過 §8 的 exactly-one-field
   規則，且該值必須屬於 `Spec` 宣告的 domain（`choice` 必須是 `Spec.Options`
   其中一個 ID；`integer` 必須落在 `Spec.Range` 內；`boolean` 無額外限制）。
3. `Candidates`（若存在）：每個 `Candidate.Value` 必須 unique，必須屬於
   同一個 domain，且集合必須完整覆蓋 domain（§9）。
4. `ConfidenceSemantics == ConfidenceNone` 時，`Confidence` 欄位必須是零值
   且 caller 不得讀取；`Raw`／`Calibrated` 時 `Confidence` 必須是 finite
   且落在 `[0,1]`。
5. `Candidates` 非空且 confidence 不是 `none` 時，`Confidence` 必須與被選
   `Value` 對應 candidate 的 probability 相差不超過 `1e-6`。
6. `Model` 若非空，必須是 valid UTF-8、不得含 Unicode control rune，且不得
   超過 256 UTF-8 bytes；違反視為 invalid backend output。

---

## 11. Backend interface

```go
type BackendResult struct {
    Status Status

    Value Value

    Candidates []Candidate

    Confidence          float64
    ConfidenceSemantics ConfidenceSemantics

    Model string
}

type Backend interface {
    Name() string

    Decide(
        ctx context.Context,
        req Request,
    ) (BackendResult, error)
}
```

Backend 責任：

- 將 `Request` 轉成自己的 inference protocol。
- 回傳 bounded output。
- 不決定 fallback policy。
- 不修改 task/session state。
- 不執行 side effect。

Backend protocol/parser rejection 必須回 `*RuntimeError{Kind:
ErrorInvalidBackendOutput}`；transport/provider/generator failure 必須回
`ErrorBackendFailure`；無法建構/解析指定 backend 必須回
`ErrorBackendUnavailable`。Runtime 遇到 backend 回傳的其他 error 時一律包成
`ErrorBackendFailure`，並填入實際 backend name。

`BackendResult.Status` 只能是 `StatusDecided` 或 `StatusAbstained`。
`StatusAbstained` 時 `Value`/`Candidates` 必須為零值；backend 不回傳
runtime-owned `ReasonCode`，runtime 將其標記為 `backend_abstained`。

Runtime 責任：

- spec validation
- backend output validation
- timeout/cancellation propagation
- acceptance policy
- fallback
- abstain
- receipt

---

## 12. DecisionPrimitive interface

```go
type Runtime interface {
    Decide(
        ctx context.Context,
        req Request,
    ) (Result, Receipt, error)
}

type RuntimeConfig struct {
    Primary  Backend
    Fallback Backend // nil = no fallback
    Policy   AcceptancePolicy
    Timeout  time.Duration
    Metrics  Metrics // nil = no-op
}

func NewRuntime(cfg RuntimeConfig) (Runtime, error)
```

`Primary` 必須非 nil。`Timeout == 0` 時使用 `2s`；負值或超過
`10s` 是 configuration error，建構時立即拒絕。`Fallback` 若與
`Primary` 是同一個 backend name 也必須拒絕，避免假 fallback。
每個 backend 的 `Name()` 必須固定、trim 後非空、無 leading/trailing
whitespace，並符合 `[a-z][a-z0-9._-]{0,127}`；`NewRuntime` 在建構時讀取並保存名稱，
每次 request 不重新求值。Final `Result.Backend` 與 `Receipt.Backend` 都使用
實際產生 final result 的保存名稱。

`error` 僅用於無法完成 runtime operation 的 technical failure。
`Decide` 收到 nil `context.Context` 時回 `ErrorInvalidRequest`，不得 panic 或
替換成 `context.Background()`。

Semantic uncertainty 必須回：

```go
Result{
    Status: StatusAbstained,
    ReasonCode: "low_confidence",
}
```

而不是：

```go
return Result{}, errors.New("not sure")
```

### 12.1 Typed errors

Core 必須提供可以 `errors.As` 分類的 `RuntimeError`：

```go
type ErrorKind string

const (
    ErrorInvalidRequest      ErrorKind = "invalid_request"
    ErrorConfiguration       ErrorKind = "configuration"
    ErrorBackendUnavailable  ErrorKind = "backend_unavailable"
    ErrorBackendFailure      ErrorKind = "backend_failure"
    ErrorInvalidBackendOutput ErrorKind = "invalid_backend_output"
)

type RuntimeError struct {
    Kind    ErrorKind
    Backend string
    Err     error
}
```

所有 typed error 都以 `*RuntimeError` 回傳。`Error()` 與 `Unwrap()` 必須
實作；`Unwrap()` 必須保留原始 error，但 `Error()` 不得包含 request Context、
prompt、backend raw output 或 wrapped error text。`Error()` 固定為
`"decisionrt: " + string(Kind)`；`Backend` 與 `Err` 只供程式化檢查。
CLI 依 §48 將 `InvalidRequest` 映射為 2，
`BackendFailure`/`InvalidBackendOutput` 映射為 4，
`Configuration`/`BackendUnavailable` 映射為 5。

---

## 13. Acceptance policy

```go
type AcceptancePolicy struct {
    MinConfidence *float64

    RequireCalibratedConfidence bool
}
```

`NewRuntime` 必須驗證 `MinConfidence` 是 finite 且在 `[0,1]`；否則回
`ErrorConfiguration`。`RequireCalibratedConfidence=true` 即使沒有設定
`MinConfidence`，也表示結果必須有 `ConfidenceCalibrated`（threshold 視為
`0`）。

規則：

1. 未設定 `MinConfidence`：
   - 合法 bounded output 可以接受。
2. 設定 `MinConfidence` 且 `RequireCalibratedConfidence=false`：
   - 可對 `raw` 或 `calibrated` confidence 套 threshold。
   - 此模式必須被視為 heuristic。
3. `RequireCalibratedConfidence=true`：
   - 非 `calibrated` confidence 一律不得通過 threshold。
4. backend 沒有 confidence：
   - 若 policy 要求 threshold，結果為 `ABSTAINED`。

任何 acceptance policy rejection 的 final `ReasonCode` 固定為
`low_confidence`；backend 主動 abstain 的 final code 固定為
`backend_abstained`。兩者不得使用自由文字。

預設不得聲稱 logits inference backend 的 softmax、LLM self-reported confidence 或 logprobs 已校準。

---

## 14. Fallback chain

`NewRuntime` 內部實作一個最多兩個 backend 的 ordered chain，不公開第二套
runtime/chain API，也不做複雜 orchestration。

每次 `Decide` 的固定順序是：檢查 ctx → `Request.Validate` → `Digest` → primary
attempt → backend-result validation → acceptance policy →（需要時）fallback
attempt/validation/policy → final Result/Receipt。Validation 或 digest 失敗時不得
呼叫任何 backend 或 Metrics attempt hook。

執行：

```text
validate request
      |
      v
primary backend
      |
      +-- technical error ----------+
      |                             |
      +-- invalid bounded output ---+--> fallback
      |                             |
      +-- backend abstained --------+
      |                             |
      +-- policy rejects ----------+
      |
      v
accepted
```

Fallback 為 nil 時，primary 的 technical/invalid-output failure 直接回傳
typed error，primary abstain/policy rejection 直接回 `ABSTAINED`。

fallback 也失敗：

- technical failure -> return error
- semantic rejection -> `ABSTAINED`

### 14.1 No same-backend semantic retry

若 backend 成功回傳：

```text
small   0.43
medium  0.39
large   0.18
```

且 policy 不接受，不得因為「信心不足」重跑同一 backend。

應：

```text
primary -> abstain -> fallback
```

避免把非確定性假裝成 transient failure（暫態故障）。

---

## 15. Digest and idempotency support

Core 必須提供 deterministic request digest：

```go
func Digest(req Request) (string, error)
```

digest input 固定涵蓋：

```text
purpose
spec.id
spec.version
spec.kind
question
option IDs + descriptions
range
canonical context
```

Canonical bytes 必須由 private struct（不是 `map[string]any`）以
`encoding/json.Marshal` 產生。Top-level 欄位依序是 `purpose`、`spec`、
`context`；`spec` 欄位依序是 `id`、`version`、`kind`、`question`、`options`、
`range`，且 `options`/`range` 不使用 `omitempty`。`Options` 保留原始順序。
Context 先按 key byte-wise 升冪排序成 entries，每項欄位依序是 `key`、
`type`、`value`；`type` 只能是 `string`、`bool`、`number`，`value` 一律是依
§7.3 產生的 string。不得 Unicode normalize、trim 或 HTML unescape 使用者
值。Hash 是 canonical bytes 的 SHA-256。
Nil 與 empty `Options` 都 canonicalize 為 `[]`；nil 與 empty Context 也都是
`[]`；沒有 range 固定為 JSON `null`。

格式：

```text
sha256:<hex>
```

要求：

- map key order 不得影響 digest。
- 依上述 canonical rules 相同的 request 必須得到相同 digest。
- Context 必須拒絕不能 canonicalize 的值。
- 禁止 pointer address、time.Now、randomness 等非語意資料進入 digest。

注意：MVP 只提供 digest，不在 `decisionrt` core 自建 cache/event store。

caller 可使用 digest 做：

- idempotency
- receipt dedupe
- durable reuse

---

## 16. Receipt

```go
type Receipt struct {
    SchemaVersion int `json:"schema_version"`

    Purpose       string `json:"purpose"`
    RequestDigest string `json:"request_digest"`

    SpecID      string `json:"spec_id"`
    SpecVersion string `json:"spec_version"`

    Backend string `json:"backend"`
    Model   string `json:"model,omitempty"`

    Status     Status `json:"status"`
    ReasonCode string `json:"reason_code,omitempty"`

    FallbackUsed bool `json:"fallback_used"`

    DurationMS uint64 `json:"duration_ms"`
}
```

Receipt 是 observability/audit metadata（觀測與稽核中繼資料），不是新的 `DecisionRecord`。
成功 receipt 的 `SchemaVersion` 固定為 `1`。

`DurationMS` 是整次 `Runtime.Decide` 從完成 request validation 後到 final result
完成的 wall-clock milliseconds，包含 primary 與 fallback；使用 monotonic duration
並向下取整。`Backend`/`Model` 必須與 final `Result` 相同。

`Runtime.Decide` 只有在成功回傳 `DECIDED` 或 `ABSTAINED` 時才回傳有效
`Receipt`。任何 non-nil error 都必須同時回傳零值 `Result` 與零值
`Receipt`；caller 不得持久化部分完成的 receipt。

### 16.1 Privacy

Receipt 預設只存 digest，不存 raw Context。

不得把 secret、完整 prompt 或 arbitrary model output 寫入 receipt。

---

## 17. Sidecar backend

Phase 2 新增：

```text
internal/decisionrt/backend/sidecar
```

目的不是模擬 logits，而是先驗證 `DecisionPrimitive` contract 能安全接上 hufu 既有 model infrastructure。

### 17.1 Sidecar output protocol

Backend prompt 必須：

1. 列出合法候選。
2. 使用 `A0`、`A1`、… 的 canonical token 代表候選。
3. 要求只回傳 machine payload。
4. runtime 對結果做 strict validation。
5. 以 `encoding/json` 編碼一個 input object，恰好包含 `purpose`、`spec_id`、
   `spec_version`、`question`、canonicalized `context` 與按 mapping 順序排列的
   `candidates`（每項含 `token`、canonical string `value`、`description`；
   boolean/integer 的 description 固定為空字串）。不得
   用字串串接未 escaped 的 question/description/context。

例如 choice：

```text
Choose exactly one candidate token from this JSON input.
Input:
{"purpose":"size-policy@v1","spec_id":"size-policy","spec_version":"v1","question":"Select a size.","context":{},"candidates":[{"token":"A0","value":"small","description":""},{"token":"A1","value":"medium","description":""},{"token":"A2","value":"large","description":""}]}

Return only one JSON object. TOKEN must be replaced by exactly one candidate token:
{"token":"TOKEN"}
```

不要依賴自然語言 label 做 parser matching。

送給 `Generator.Execute` 的完整 prompt 上限為 8000 Unicode runes。超過時不得
截斷或呼叫 generator，adapter 回 `ErrorBackendFailure`；error text 只說
`sidecar prompt exceeds 8000 runes`，不得包含 prompt。

### 17.2 Mapping

候選順序是 normative：

- `choice`：依 `Spec.Options` 原始順序，`A0` 對應第 0 個 option。
- `boolean`：`A0=false`、`A1=true`。
- `integer_range`：由 `Range.Min` 升冪到 `Range.Max`，`A0=Min`。

mapping 由 backend 依 `Spec` 每次建立；回傳未知 token 必須視為
`ErrorInvalidBackendOutput`。

Strict decoder contract：

- response 必須是 valid UTF-8，大小上限 4096 bytes；無效或超過就拒絕。
- 允許 JSON 前後 whitespace，但只允許一個 object。
- object 必須恰好只有一個 string 欄位 `token`。
- 使用 `json.Decoder.DisallowUnknownFields()`，拒絕 duplicate key、
  trailing JSON value、markdown fence 和任何額外 prose。
- 空 response 與 malformed JSON 都是 `ErrorInvalidBackendOutput`。

### 17.3 Confidence

§17.1 的 strict protocol 只要求 backend 回傳單一 canonical token，
不要求、也不解析任何 confidence 或 per-candidate probability。因此：

- sidecar backend 的 `BackendResult.Candidates` 在 MVP 必須留空
  （不得偽造或估算一個完整分布）。
- sidecar backend 固定回傳 `ConfidenceNone` 和 `Confidence == 0`。
- sidecar raw response 只用於當次 strict parsing，不得寫入 receipt 或 log。

Adapter 建構契約：

```go
type Generator interface {
    Execute(context.Context, string) (string, error)
    ModelID() string
}

func New(generator Generator) (decisionrt.Backend, error)
```

`*internal/sidecar.Sidecar` 可透過薄 adapter 滿足此介面；unit tests 使用
fake `Generator`，不呼叫 live provider。
`New` 必須拒絕 nil generator，以及空白、有 leading/trailing whitespace 或
超過 256 UTF-8 bytes 的 `ModelID()`；回 `ErrorConfiguration`。成功 backend 的
`Name()` 固定為 `sidecar`。

---

## 18. Rule backend

提供 minimal rule backend：

```go
type DecideFunc func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error)

type Func struct {
    BackendName string
    DecideFunc  DecideFunc
}

func (f Func) Name() string
func (f Func) Decide(context.Context, decisionrt.Request) (decisionrt.BackendResult, error)

func AlwaysAbstain() decisionrt.Backend
```

`Func.Decide` 在 `BackendName` 空白或 `DecideFunc == nil` 時回
`ErrorConfiguration`；不得 panic。`AlwaysAbstain().Name()` 固定為 `rule`。

用途：

- deterministic fallback
- unit tests
- gradual migration
- preserve existing hufu behavior

它不是 rule engine。

禁止加入：

- DSL
- expression language
- policy graph
- dynamic scripts

### 18.1 Rule backend via CLI

CLI（Phase 3, §40）只註冊 `AlwaysAbstain()` 這個固定內建 rule
backend。它對所有合法 request 回傳
`BackendResult{Status: StatusAbstained}`，由 runtime 產生
`Result{Status: StatusAbstained, ReasonCode: "backend_abstained"}`。它不透過
CLI flag 動態組出任意規則。任何自訂 `Func`
必須由呼叫端在 Go 程式碼中直接注入 `Runtime`，不經由 CLI。這避免
在沒有 rule 語言/DSL（本節已明文禁止）的前提下，還要求 CLI 能「建立」通用
規則。

---

## 19. Configuration

MVP 只有兩個明確的建構面：

1. Go caller 以 §12 的 `RuntimeConfig` 直接建構 runtime。
2. Standalone `hufu decisionrt` CLI 只讀取該 command 自己的 flags
   （§40–§57）。

本規格不新增 `hufu.yaml` 或 `team.yaml` key，不修改
`internal/config.Config`、`agent.TeamConfig` 或 team manifest schema，也不定義
任何 runtime mode。CLI 的生產用 backend registry 固定只註冊：

- `rule`：§18.1 的 `AlwaysAbstain()`，永遠 available。
- `sidecar`：§17 adapter；只有在 CLI 提供有效 model/provider
  設定時 available。

除了上述兩個名稱，任何 backend name 都是
`ErrorBackendUnavailable`。

---

## 20. No coordinator integration in this scope

本規格不在 coordinator、retry、routing、guard、memory、session、event
store 或 `DecisionEngine` 增加任何呼叫點。不新增 recovery disposition、
runtime telemetry sink、team config 或 control-flow feature flag。

因此 coding agent 不需要、也不得修改：

```text
internal/team/**
internal/agent/agent.go
internal/agent/decision_*.go
docs/architecture/decision-runtime.md
```

現有 `*internal/sidecar.Sidecar` 已提供 §17 所需的 `Execute` 與 `ModelID`；
本實作不得修改 `internal/sidecar`。

---

## 21. Existing behavior preservation

因為 MVP 沒有生產 runtime call site，現有 team 執行、retry、routing、
provider admission、budget、session/replay 與 DecisionEngine 行為必須完全不變。
`hufu decisionrt` 是新的顯式 standalone command；只有執行該 command
才會建構 backend 或發出 provider request。

---

## 22. Relationship with existing DecisionEngine

`DecisionEngine` 不依賴、不呼叫、不持久 `DecisionPrimitive`。本實作
不修改 `DecisionServices`、`DecisionRecord`、decision profiles、`hufu decision`
或 `hufu decide`。這是硬性 compatibility boundary，不是後續 phase。

---

## 23. External inference backends are excluded

本實作不建立 logits、llama.cpp、Python/HTTP model service 或其他新
inference backend，也不建立這些 backend 的空目錄、registry row、CLI
help 或範例。Core API 只保持 backend-agnostic。

---

## 24. Calibration is excluded

本實作不建立 training、calibration、Brier/ECE 或 dataset subsystem。
§9/§13 的 `ConfidenceCalibrated` 只是 API 可表達的語意；MVP 的
`rule` 與 `sidecar` backend 都回傳 `ConfidenceNone`。

## 25. Observability

本 repo 目前沒有任何通用 metrics/telemetry 系統（已查核：無 Prometheus、無
OpenTelemetry、無 StatsD、無任何可掛載的 counter 介面）。因此本節**不得**
假設有既有 telemetry 可以「整合」，只能在 `decisionrt` 內部定義一個小型、
無外部相依的 counter 介面，由 caller 自行決定要不要接到別的地方。

```go
type Metrics interface {
    IncCalls(purpose, backend string)
    IncDecided(purpose, backend string)
    IncAbstained(purpose, backend, reasonCode string)
    IncErrors(purpose, backend string)
    IncFallback(purpose, backend string)
    ObserveDurationMS(purpose, backend string, ms uint64)
}
```

規則：

- `Runtime` 建構時接受一個 optional `Metrics`；未提供時使用
  package 內建的 no-op 實作，行為不得因此改變。
- `decisionrt` core 只定義介面與 no-op 實作，**不得**內嵌任何 exporter、
  不得開 HTTP endpoint、不得新增 metrics 相關的第三方依賴。
- MVP 不提供任何 Metrics adapter；production CLI 使用 no-op，單元測試注入
  fake Metrics。
- `IncCalls` 與 `ObserveDurationMS` 每個實際開始的 backend attempt 各一次。
- `IncFallback` 在 primary 後確實要開始 fallback 時一次，`backend` 傳 fallback
  name。
- `IncErrors` 在某個 attempt 產生 technical/invalid-output error 時一次，即使
  fallback 後成功仍計數。
- `IncDecided` 或 `IncAbstained` 只對 final result 呼叫一次，`backend` 是 final
  result backend。
- 不得把 model output、question、Context、digest 或 task text 傳給 Metrics。

### 25.1 測試

必須測試：

```text
Runtime 未提供 Metrics 時使用 no-op，行為與提供時一致
提供假 Metrics 時，各計數方法在對應情境被呼叫且次數正確
```

---

## 26. Security and authority

`DecisionPrimitive` 必須是 pure decision surface（純決策介面）：

不得：

- call tools
- write files
- execute shell
- mutate memory
- invoke MCP tools
- perform network except through configured backend transport
- directly commit side effects

模型只選擇候選。

真正 side effect 必須由 caller 依既有 authorization/policy 執行。

因此：

```text
DecisionPrimitive says: "human"
```

不代表 runtime 自己可以啟動 HITL workflow；caller 決定如何處理。

---

## 27. Failure model

- Invalid request：不呼叫 backend，回 `ErrorInvalidRequest`。
- `ErrorConfiguration` 或 `ErrorBackendUnavailable`：立即回 error，不進入
  fallback；fallback 不得掩蓋錯誤設定。
- Backend technical failure、attempt timeout、invalid protocol/output：primary
  時若有 fallback 就進入 fallback，否則回 typed error；fallback 發生時直接回
  typed error。
- Caller 原始 context 已取消或 deadline 已到：立即停止且不得進入 fallback；
  回傳 error 必須能以 `errors.Is` 匹配 `context.Canceled` 或
  `context.DeadlineExceeded`。
- Backend abstention 或 policy rejection：primary 時若有 fallback 就進入
  fallback；否則回 `ABSTAINED`。Fallback 仍拒絕時回 `ABSTAINED`。
- Runtime 不做相似字串修補、同 backend retry 或第三次嘗試。

---

## 28. Concurrency

`Runtime`、rule backend 與 sidecar adapter 必須 safe for concurrent use。所有
per-request state 都放在 local scope；shared object 不保存 prompt、raw response、
token mapping 或前一次結果。注入的 `Generator` 也必須由實作者保證
concurrency-safe；adapter 不額外序列化呼叫。注入的 `Metrics` 和自訂
`rule.DecideFunc` 也必須由 caller 保證 concurrency-safe。

---

## 29. Timeout

每次 backend attempt 都使用 `context.WithTimeout`：

- `RuntimeConfig.Timeout == 0`：`2s`。
- `0 < Timeout <= 10s`：使用指定值。
- `Timeout < 0` 或 `Timeout > 10s`：`NewRuntime` 回
  `ErrorConfiguration`，不得 clamp。
- Caller deadline 較早時由 Go context 自然採較早 deadline。
- Primary 進入 fallback 時，fallback 取得新的 attempt timeout，但仍受原始
  caller context 的 deadline/cancellation 約束。

---

## 30. Tests

### 30.1 Core validation

必須測試：

- empty ID
- empty version
- unknown Kind
- duplicated options
- choice with <2 options
- boolean with options
- integer range missing
- integer outside range
- wrong typed value
- multiple Value fields simultaneously set
- NaN/Inf confidence
- probability outside `[0,1]`
- invalid distribution sum
- distribution missing/adding a domain value
- abstained result with non-zero `Value` rejected（§10.2）
- decided result whose `Value` is outside `Spec` domain rejected（§10.2）
- duplicated `Candidate.Value` rejected（§10.2）
- Context 含 `nil`／nested map／NaN／Inf value 被拒絕（§7.3）
- Context 數值型別（`int`/`int64`/`float64`）canonicalize 成相同 digest（§7.3）

### 30.2 Digest

必須測試：

```text
same semantic map with different key order -> same digest
changed option -> different digest
changed version -> different digest
changed context -> different digest
```

### 30.3 Runtime fallback

必須測試：

- primary accepted
- primary `ErrorBackendFailure` -> fallback
- primary invalid -> fallback
- primary abstains -> fallback
- fallback abstains -> final ABSTAINED
- fallback error -> error
- context cancellation propagates
- attempt timeout -> fallback; caller deadline -> no fallback
- configuration/unavailable error -> no fallback
- fallback flag is correct
- invalid timeout/policy and same-name fallback rejected by `NewRuntime`
- non-nil error returns zero Result and zero Receipt
- caller cancellation never invokes fallback
- fake Metrics call counts follow §25

### 30.4 Sidecar backend

必須測試：

- known token maps correctly
- unknown token rejected
- prose around payload rejected
- malformed JSON rejected
- duplicate/unknown key rejected
- trailing JSON/prose/markdown fence rejected
- response larger than 4096 bytes rejected
- model cannot return arbitrary option ID
- boolean mapping
- integer range mapping
- empty response
- cancellation
- fixed `ConfidenceNone`, zero confidence, empty candidates
- `ModelID()` copied to result

### 30.5 CLI

CLI 測試的完整矩陣見 §54。Unit tests 必須注入 fake
registry/backend/generator；process-level contract test 可使用本機
`httptest.Server` 模擬 OpenAI-compatible response。任何測試都不得連接外部或
live provider。

### 30.6 Repository gates

完成後必須：

```bash
go test ./...
go vet ./...
golangci-lint run
```

---

## 31. Architecture test

必須新增以 Go import graph（例如 `go list -deps`）為準的 dependency guard，
確保：

```text
internal/decisionrt
```

core 不 import：

```text
github.com/kjelly/hufu/internal/team
github.com/kjelly/hufu/internal/agent
github.com/kjelly/hufu/internal/sidecar
github.com/kjelly/hufu/cmd/hufu
```

允許 backend subpackage import其所需 adapter dependency，例如：

```text
internal/decisionrt/backend/sidecar
    -> internal/decisionrt
    -> internal/sidecar
```

core 必須維持可獨立測試。不得用全文字串 grep 實作 guard，避免註解或測試資料
造成 false positive。

---

## 32. Implementation phases

### Phase 0 — Baseline

先確認 baseline：

1. `go test ./...`
2. 記錄 baseline commit 與任何既存失敗。
3. 不修改 runtime semantics。

Deliverable：

```text
baseline test result + recorded commit
```

---

### Phase 1 — Core DecisionPrimitive

新增：

```text
internal/decisionrt/
```

完成：

- Kind
- Spec
- Request
- Value
- Result
- Backend
- Runtime
- validation
- digest
- receipt
- fake/rule backend
- unit tests

不得：

- 改 team DecisionEngine
- 加 model dependency
- 加 CLI
- 改 execution behavior

Exit criteria：

```text
core can decide through fake/rule backend
all invalid outputs fail closed
go test ./... passes
```

---

### Phase 2 — Sidecar adapter

新增：

```text
internal/decisionrt/backend/sidecar
```

完成：

- strict bounded prompt protocol
- candidate token mapping
- choice/boolean/integer support
- cancellation
- strict parsing
- tests

不得：

- 修改 `internal/sidecar`（既有 `Sidecar` 已滿足 `Generator`）
- coordinator/control-flow integration
- logits inference backend dependency

Exit criteria：

```text
existing hufu sidecar can act as a DecisionPrimitive backend
without exposing arbitrary model text as the result
```

---

### Phase 3 — Standalone CLI

新增：

```text
cmd/hufu/decisionrtcmd.go
cmd/hufu/decisionrtcmd_test.go
cmd/hufu/decisionrt_input.go
cmd/hufu/decisionrt_output.go
cmd/hufu/decisionrt_registry.go
```

完成 §40–57 定義的 `hufu decisionrt` 全部子指令（`choice`／`boolean`／
`integer`／`run`／`validate`／`backends`），backend 只需支援 `rule` 與
`sidecar`（`logits`／`local` 不存在，不得出現在 help text 或範例）。

不得：

- 依賴 `internal/team` 或建立 agent team／session
- 讀取任何 `team.yaml` 或 `hufu.yaml`（見 §19）
- 建立 `DecisionRecord` 或啟動既有 `DecisionEngine`

Exit criteria：

```text
hufu decisionrt choice/boolean/integer/run/validate/backends 全部可用
CLI 不需要 team.yaml 或 agent team 即可執行
exit code / stdout-stderr contract 測試通過（§48/§49/§54）
```

---

## 33. Minimal Go usage

```go
primary := rule.Func{
    BackendName: "size-policy",
    DecideFunc: func(_ context.Context, _ decisionrt.Request) (decisionrt.BackendResult, error) {
        return decisionrt.BackendResult{
            Status:              decisionrt.StatusDecided,
            Value:               decisionrt.Value{Choice: "medium"},
            ConfidenceSemantics: decisionrt.ConfidenceNone,
        }, nil
    },
}

runtime, err := decisionrt.NewRuntime(decisionrt.RuntimeConfig{Primary: primary})
if err != nil {
    return err
}

result, receipt, err := runtime.Decide(ctx, decisionrt.Request{
    Purpose: "size-policy@v1",
    Spec: decisionrt.Spec{
        ID:       "size-policy",
        Version:  "v1",
        Kind:     decisionrt.KindChoice,
        Question: "Select a size.",
        Options: []decisionrt.Option{
            {ID: "small"},
            {ID: "medium"},
            {ID: "large"},
        },
    },
})
```

`rule.Func` 的確切 fields 由 §18 定義；不得改成 DSL 或自然語言 parser。

---

## 34. Anti-patterns

Coding agent 不得實作以下設計。

### 34.1 Generic JSON schema engine

錯誤：

```go
Decide(schema map[string]any) (map[string]any, error)
```

原因：會失去 typed bounded semantics。

### 34.2 New agent type

錯誤：

```text
DecisionAgent
```

這不是 agent；它沒有 autonomous loop、tools 或 memory。

### 34.3 New workflow subsystem

錯誤：

```text
DecisionWorkflow
DecisionGraph
DecisionPipeline DSL
```

MVP 不需要。

### 34.4 Model-generated fallback

錯誤：

```text
small model uncertain
   ->
ask same model what fallback should be
```

fallback 是 runtime policy。

### 34.5 Silent coercion

錯誤：

```text
"Retry" -> "retry"
"probably retry" -> "retry"
```

Backend 必須使用明確 candidate mapping。未知結果 fail closed。

### 34.6 Confidence inflation

錯誤：

```text
softmax 0.94 == 94% chance of correctness
```

沒有 calibration evidence 時只能標記 `raw`。

### 34.7 DecisionPrimitive owns persistence

錯誤：

```text
decisionrt -> EventStore
decisionrt -> ArtifactStore
decisionrt -> TaskJournal
```

Core 只回 `Receipt`；durability 由 integration layer 決定。

---

## 35. Compatibility

不得改變：

- existing team YAML `decision:` semantics
- existing decision profile resolution
- existing `hufu decision` commands
- DecisionRecord schema
- existing `DecisionServices` in MVP
- existing sidecar default behavior
- existing provider/model identity semantics
- existing retry/routing/resume behavior

本規格不新增或讀取任何 `hufu.yaml`／`team.yaml` 設定，因此沒有 config
migration。

---

## 36. Documentation

Phase 3 完成時更新 `README.md` 與 `README.tw.md` 的 command reference，僅說明：

1. `DecisionPrimitive` 與既有 `DecisionEngine` 不同。
2. `hufu decisionrt` 是明確呼叫、decision-only 的 standalone command。
3. Backend 只有 `rule` 與 `sidecar`；rule 固定 abstain，sidecar 需要明確
   model/provider flags。
4. `ABSTAINED`、technical failure 與 exit code 的差異。

不得宣稱第三方 protocol compatibility、校準能力或 coordinator integration。

---

## 37. Acceptance criteria

功能完成必須同時滿足：

- [x] 新增 backend-agnostic `internal/decisionrt`。
- [x] 支援 `choice`。
- [x] 支援 `boolean`。
- [x] 支援 bounded integer。
- [x] 任意值無法通過 validation。
- [x] `ABSTAINED` 與 technical error 分離。
- [x] confidence semantics 明確標記。
- [x] deterministic request digest。
- [x] core 不依賴 `internal/team`。
- [x] core 不依賴任何 logits inference backend/Python。
- [x] sidecar adapter 使用 strict bounded protocol。
- [x] `hufu decisionrt` CLI（Phase 3）不需要 team.yaml 或 agent team 即可
      獨立運作。
- [x] 不新增第二套 DecisionEngine。
- [x] 不修改 existing DecisionRecord schema。
- [x] 不修改 existing DecisionServices in MVP。
- [x] `go test ./...` 通過。
- [x] `go vet ./...` 通過。
- [x] `golangci-lint run` 通過。
- [x] 現有 decision/routing/resume/telemetry regression tests 通過。

---

## 38. Definition of Done

本工作完成的標準不是「成功呼叫一個小模型」。

Definition of Done：

```text
hufu has a small typed control-plane decision primitive
whose API is independent of any model/provider,
whose outputs are bounded and runtime-validated,
whose uncertainty can abstain,
whose backend failures fail closed,
and which has no call site in the existing team runtime or DecisionEngine.
```

---

## 39. Coding-agent execution instruction

實作時依 Phase 0 -> 1 -> 2 -> 3 順序進行（§32：0 baseline、1 core、
2 sidecar adapter、3 standalone CLI）。

每一 Phase：

1. 先新增/更新 tests。
2. 再做最小 implementation。
3. 執行該 package tests。
4. 執行 `go test ./...`。
5. 檢查 semantic regression（語意回歸：程式仍可編譯但既有行為被改變）。
6. 不順手進行無關 refactor。
7. 不提前實作下一 Phase。
8. 若現有 repository naming/abstraction 與本文件有衝突，優先維持現有 architecture invariant，再以最小差異調整名稱；不得以此為理由擴張 scope。

Phase 3 結束後必須額外執行 `go vet ./...` 與 `golangci-lint run`。Coordinator
integration、mode、external inference backend、calibration 與 persistence
subsystem 均不得實作，也不得建立其空目錄、config 或介面骨架。


---

## 40. Direct CLI for DecisionPrimitive

`DecisionPrimitive` 必須提供可直接使用的 CLI。

CLI 名稱：

```text
hufu decisionrt
```

不要使用：

```text
hufu decision
hufu decide
```

`hufu decision`（`cmd/hufu/decisioncmd.go`）已代表高階 `DecisionEngine` /
durable `DecisionRecord` 操作。`hufu decide`（`cmd/hufu/runcmd.go`
`newDecideCommand()`，於 `cmd/hufu/root.go` 註冊）已是現有指令：`hufu run`
的別名，intent 預設為 `decision`，與本規格要新增的 CLI 完全無關。兩者都不得
被本規格的新 CLI 佔用或取代——`hufu decide` 現有行為維持不變，不在本規格
異動範圍內。`hufu decisionrt` 與套件名 `internal/decisionrt` 對齊。

### 40.1 CLI goals

CLI 主要用途：

- 手動測試 `DecisionPrimitive`
- shell / script integration
- backend diagnostics
- 驗證 choice / boolean / integer range
- CI smoke test

CLI 不得：

- 建立 `DecisionRecord`
- 啟動完整 `DecisionEngine`
- 隱式執行 tool/action
- 自動改變 task/session state
- 將 decision result 直接視為 authorized side effect

CLI 只輸出 typed decision result。

---

## 41. CLI command structure

```text
hufu decisionrt <kind> [flags]
```

MVP subcommands：

```text
hufu decisionrt choice
hufu decisionrt boolean
hufu decisionrt integer
hufu decisionrt run
hufu decisionrt validate
hufu decisionrt backends
```

不得新增其他 alias 或 subcommand。`run` 從 JSON request 接收完整
`decisionrt.Request`；`validate` 只驗證同一種 JSON request；`backends` 不做
inference。

Root inherited flags `--workspace`、`--decision-profile`、`--profile`、
`--execution-profile`、`--goal-mode`、`--no-color` 都不適用；若其中任一
`Changed`，decisionrt leaf 必須回 usage/exit 2，而不是忽略或讀取 config。
Parent 沒有 subcommand或收到未知 subcommand 時也回 exit 2；`--help` 回 0。

---

## 42. `hufu decisionrt choice`

範例：

```bash
hufu decisionrt choice \
  --id size-policy \
  --version v1 \
  --purpose size-policy@v1 \
  --question "Select a size for this request." \
  --option small="Small" \
  --option medium="Medium" \
  --option large="Large" \
  --context workload=medium \
  --backend sidecar \
  --sidecar-model qwen3:1b \
  --provider-url http://127.0.0.1:11434/v1 \
  --json
```

Machine-output schema example（所選 value 僅供示意；`sidecar` backend 依 §17.1/§17.3 只回傳單一 token，
不產生 candidate distribution，因此 `candidates` 省略）：

```json
{
  "status": "decided",
  "value": {
    "choice": "medium"
  },
  "confidence_semantics": "none",
  "backend": "sidecar",
  "model": "qwen3:1b",
  "fallback_used": false
}
```

Human-readable default output 可為：

```text
medium
```

但 `--json` 必須輸出完整、stable schema。

### 42.1 Choice flags

`choice` 的 flags 恰好是以下 kind-specific flags 加 §42.2 common flags：

```text
--id string
--version string
--purpose string
--question string
--option key=description        repeatable
--context key=value             repeatable
--context-json string
--context-file path
```

`--id`、`--version`、`--purpose`、`--question` 都是 required；不得提供空字串。
`--option` 必須出現 2 到 21 次，以第一個 `=` 分隔 ID 與 description；ID
不可空，description 可空，沒有 `=` 是 usage error。

若：

```text
--context
--context-json
--context-file
```

可以同時使用，固定 merge 順序為：

```text
context-file < context-json < repeated --context
```

後者覆蓋前者相同 key。

`--context-file` 只接受 UTF-8 JSON object，不接受 YAML。`--context-json`
也必須是 JSON object。兩者與 generic JSON input 都必須拒絕 duplicate key，
並使用 `UseNumber()`。`--context-file` read limit 為 1 MiB；超過即 usage error。

每個 `--context key=value` 的 `key` 必須非空且不得重複。CLI 先嘗試把
`value` 解成單一 JSON scalar（string、boolean 或 number）；不是合法 JSON
scalar 時才當 literal string。`null`、object、array 一律拒絕。需要數字外觀的
字串時使用 JSON 引號，例如 `--context code='"001"'`。

### 42.2 Common execution flags

`choice`、`boolean`、`integer` 與 `run` 共用：

```text
--backend string            default "rule"; only rule|sidecar
--timeout duration          per attempt; default 2s; (0,10s]
--min-confidence float      optional; [0,1]
--require-calibrated        default false
--no-fallback               default false
--sidecar-model string      required when sidecar is selected
--provider-url string       default http://127.0.0.1:11434/v1
--provider-api-key string   optional; otherwise HUFU_PROVIDER_API_KEY
--json                      default false
--receipt                   default false
```

這些 flags 不得從 `hufu.yaml` 或 `team.yaml` 補值。API key 不得出現在
usage、diagnostics、result 或 receipt。`--require-calibrated` 必須隱含
`MinConfidence=0`，因此 MVP 的 rule/sidecar 都會 abstain；若同時提供
`--min-confidence` 則使用該 threshold。

`--sidecar-model` 不得有 leading/trailing whitespace，長度上限 256 UTF-8
bytes。`--provider-url` 必須符合 §46；API key 可以為空且不得做格式猜測。

---

## 43. `hufu decisionrt boolean`

範例：

```bash
hufu decisionrt boolean \
  --id requires-review \
  --version v1 \
  --purpose requires-review@v1 \
  --question "Does this request require review?" \
  --context risk=high \
  --backend sidecar \
  --sidecar-model qwen3:1b \
  --provider-url http://127.0.0.1:11434/v1 \
  --json
```

Machine-output schema example（value 僅供示意）：

```json
{
  "status": "decided",
  "value": {
    "boolean": true
  },
  "confidence_semantics": "none",
  "backend": "sidecar",
  "model": "qwen3:1b",
  "fallback_used": false
}
```

Human-readable default：

```text
true
```

`boolean` 的 kind-specific flags 是 `--id`、`--version`、`--purpose`、
`--question` 與 §42.1 的三種 context flags；不得接受 `--option`、`--min` 或
`--max`。前四個 flags 都是 required。

---

## 44. `hufu decisionrt integer`

範例：

```bash
hufu decisionrt integer \
  --id priority-score \
  --version v1 \
  --purpose priority-score@v1 \
  --question "Rate this request's priority." \
  --min 0 \
  --max 4 \
  --context impact=high \
  --backend sidecar \
  --sidecar-model qwen3:1b \
  --provider-url http://127.0.0.1:11434/v1 \
  --json
```

Machine-output schema example（value 僅供示意）：

```json
{
  "status": "decided",
  "value": {
    "integer": 3
  },
  "confidence_semantics": "none",
  "backend": "sidecar",
  "model": "qwen3:1b",
  "fallback_used": false
}
```

Human-readable default：

```text
3
```

`integer` 的 kind-specific flags 是 `--id`、`--version`、`--purpose`、
`--question`、required `--min`、required `--max` 與 §42.1 的 context flags；
不得接受 `--option`。`--min`/`--max` 使用 `int64` 且必須依 flag Changed 狀態
判斷是否提供，不能把數值 `0` 當成未提供。

---

## 45. Generic JSON CLI

供程式、benchmark、eval 使用：

```bash
hufu decisionrt run --file request.json \
  --backend sidecar \
  --sidecar-model qwen3:1b
```

或：

```bash
cat request.json | hufu decisionrt run --stdin \
  --backend sidecar \
  --sidecar-model qwen3:1b
```

`request.json` 直接對應 public CLI request schema：

```json
{
  "purpose": "size-policy@v1",
  "spec": {
    "id": "size-policy",
    "version": "v1",
    "kind": "choice",
    "question": "Select a size for this request.",
    "options": [
      {"id":"small","description":"Small"},
      {"id":"medium","description":"Medium"},
      {"id":"large","description":"Large"}
    ]
  },
  "context": {
    "workload": "medium"
  }
}
```

CLI：

```bash
hufu decisionrt run --file request.json \
  --backend sidecar \
  --sidecar-model qwen3:1b \
  --json
```

或 stdin：

```bash
printf '%s\n' "$REQUEST" | hufu decisionrt run --stdin \
  --backend sidecar \
  --sidecar-model qwen3:1b \
  --json
```

### 45.1 Input exclusivity

`run` 只接受 `--file path`、`--stdin` 與 §42.2 common execution flags。
`--file` 與 `--stdin` 必須互斥。

若沒有指定：

```text
--file
--stdin
```

且 stdin 非 TTY，可自動讀 stdin。

若 stdin 是 TTY 且沒有 input，顯示 usage error，不進入互動 wizard。

MVP 不做 interactive wizard。

Decoder 必須設定 `UseNumber()`、`DisallowUnknownFields()`，拒絕 duplicate
object keys，並確認第一個 object 後只有 whitespace/EOF。輸入上限為
`1 MiB`；超過即 `ErrorInvalidRequest`。`run` 不接受 kind-specific spec/context
flags，避免兩個 request source 合併。

---

## 46. Backend selection

CLI 可 explicit override backend：

```bash
hufu decisionrt choice ... --backend rule
hufu decisionrt choice ... --backend sidecar --sidecar-model qwen3:1b
```

MVP 只有這兩個名字有效（`local`／`logits` 未定義任何 backend，見 §23，
不得出現在 flag 說明或範例中；傳入未知名字必須 configuration error）。

但 backend 必須經 registry 解析：

```go
type BackendRegistry interface {
    Resolve(context.Context, string) (decisionrt.Backend, error)
    List() []BackendInfo
}

type RegistryOptions struct {
    SidecarModel  string
    ProviderURL   string
    ProviderAPIKey string
}

func NewDefaultRegistry(RegistryOptions) BackendRegistry

type BackendInfo struct {
    Name      string `json:"name"`
    Available bool   `json:"available"`
    Type      string `json:"type"`
    Reason    string `json:"reason,omitempty"`
}
```

`NewDefaultRegistry` 只註冊這兩個 backend：

- `rule`：§18.1 定義的固定內建 rule（無需任何 flag 建構）。
- `sidecar`：包裝既有 `internal/sidecar`。Registry factory 以 §42.2 flags
  呼叫 `agent.NewOpenAICompatibleProvider(opts.ProviderURL,
  opts.ProviderAPIKey, "local")`、`sidecar.NewSidecar` 與 §17 adapter；
  command handler 不得直接含 provider construction 或 response parsing。

CLI 不得：

```go
switch backend {
case "ollama":
case "openai":
...
}
```

把 provider-specific knowledge 寫入 command handler。

`--backend` 預設固定為 `rule`，不存在 config-derived default。`rule` 永遠
available。`List` 只有在 `--sidecar-model` 符合 §42.2 且 provider URL 是含
host、無 userinfo/query/fragment 的合法 `http`/`https` URL 時才把 sidecar
標為 available；它不得建立 provider、sidecar 或送出網路 request。
`Resolve("sidecar")` 才執行 constructors，失敗
回 `ErrorBackendUnavailable`。選擇 unavailable/unknown backend 也回同一 kind，
不得 silent 選擇其他模型。
`List()` 固定只回兩筆且順序為 `rule`、`sidecar`；`Type` 分別固定為
`deterministic`、`generative`。

---

## 47. CLI fallback behavior

CLI 依下列固定規則建立 runtime，不讀 config：

| `--backend` | default fallback | `--no-fallback` |
|---|---|---|
| `rule` | none | none |
| `sidecar` | `rule` (`AlwaysAbstain`) | none |

例如：

```bash
hufu decisionrt choice ... --backend sidecar --sidecar-model qwen3:1b
```

Sidecar 已成功建構、但 attempt 發生 technical failure、invalid output、backend
abstention 或 policy rejection時，才進入（§27 的 caller cancellation/deadline
例外仍不得 fallback）：

```text
sidecar -> rule
```

結果：

```json
{
  "status": "abstained",
  "value": {},
  "confidence_semantics": "none",
  "backend": "rule",
  "fallback_used": true,
  "reason_code": "backend_abstained"
}
```

因 MVP rule 固定 abstain，這條 fallback 的完整結果是
`StatusAbstained`、`ReasonCode="backend_abstained"`、`Backend="rule"`、
`FallbackUsed=true`，CLI exit code 為 3。Sidecar 在建構前就 unavailable
（例如缺少 model）屬 configuration/backend unavailable，直接 exit 5；不得
用 fallback 掩蓋錯誤設定。

提供：

```text
--no-fallback
```

方便 diagnostics：

```bash
hufu decisionrt choice ... \
  --backend sidecar \
  --no-fallback \
  --json
```

此時 backend technical failure 應直接 non-zero exit。

---

## 48. CLI exit codes

必須提供 stable exit code contract。

```text
0 = DECIDED
2 = usage / invalid request
3 = ABSTAINED
4 = backend/runtime technical failure
5 = configuration/backend unavailable
```

Mapping 是 exhaustive：flag/arg/input decoding、file/stdin read 與
`ErrorInvalidRequest` → 2；final abstention → 3；`ErrorBackendFailure`、
`ErrorInvalidBackendOutput`、context cancellation/deadline 或 output
encode/write failure → 4；`ErrorConfiguration`、`ErrorBackendUnavailable`、
registry/provider/sidecar construction failure → 5。不得回其他非零 code。

`ABSTAINED` 不等於 technical error，但 shell script 必須能區分：

```bash
hufu decisionrt choice ... --json
case $? in
  0) echo decided ;;
  3) echo abstained ;;
  *) echo failed ;;
esac
```

不要讓 `ABSTAINED` 回 exit code `0`，否則 automation 無法區分「有決策」與「runtime 刻意不決策」。

`cmd/hufu` 必須定義 command-local `decisionRTExitError`，實作 `Error()`、
`Unwrap()` 與 `ProcessExitCode() int`，讓既有 `main()` 保留 2/3/4/5。每個
`decisionrt` leaf command 都設定 `SilenceErrors=true`、`SilenceUsage=true`：
flag/arg/handler error 由 command 自己把一行 sanitized diagnostic 寫 stderr
後回傳對應 exit error；`ABSTAINED` 只寫 stdout result，不寫 stderr，再回
code 3。不得修改 `main()` 或其他 command 的全域 exit behavior。

---

## 49. CLI stdout/stderr contract

### stdout

只放 result。

例如 human mode：

```text
medium
```

JSON mode：

```json
{"status":"decided","value":{"choice":"medium"},"confidence_semantics":"none","backend":"sidecar","model":"qwen3:1b","fallback_used":false}
```

`ABSTAINED` 也必須先把 result（或 receipt envelope）寫到 stdout，再回 exit 3；
human mode 固定輸出 `abstained`。Exit 2/4/5 時 stdout 必須為空。

### stderr

只放：

- diagnostics
- warning
- backend errors
- configuration errors
- verbose/debug logs

這樣允許：

```bash
ACTION="$(hufu decisionrt choice ...)"
```

不會混入 log。

---

## 50. Receipt CLI

提供：

```text
--receipt
```

JSON mode：

```json
{
  "result": {
    "status": "decided",
    "value": {"choice":"medium"},
    "confidence_semantics": "none",
    "backend": "sidecar",
    "model": "qwen3:1b",
    "fallback_used": false
  },
  "receipt": {
    "schema_version": 1,
    "purpose": "size-policy@v1",
    "request_digest": "sha256:...",
    "spec_id": "size-policy",
    "spec_version": "v1",
    "backend": "sidecar",
    "model": "qwen3:1b",
    "status": "decided",
    "fallback_used": false,
    "duration_ms": 37
  }
}
```

Human output 不應因 `--receipt` 混合難以 parse 的文字。

若使用 `--receipt`，無論是否同時有 `--json`，stdout 都固定輸出 JSON
envelope。Envelope 恰好只有 `result` 與 `receipt` 兩個欄位。

---

## 51. Dry / validation CLI

提供：

```bash
hufu decisionrt validate --file request.json
```

用途：

- schema validation
- canonicalization check
- request digest inspection
- CI validation
- 不呼叫 backend

輸出：

```json
{
  "valid": true,
  "request_digest": "sha256:..."
}
```

命令：

```bash
hufu decisionrt validate --file request.json --json
```

`validate` 只接受 `--file`、`--stdin` 與 `--json`；input selection 與 decoder
契約和 §45.1 相同。Human output 固定為：

```text
valid sha256:<hex>
```

`--json` 使用上面的 object。Invalid input 回 exit 2、stdout 為空、diagnostic
寫 stderr。此命令不得建立 registry/provider 或進行 inference。

---

## 52. Backend inspection CLI

提供：

```bash
hufu decisionrt backends
```

例如：

```text
NAME       AVAILABLE   TYPE            REASON
rule       yes         deterministic    -
sidecar    no          generative       missing_sidecar_model
```

（MVP 的 registry 只註冊這兩個 backend；沒有第三個 unavailable 的
`logits` row，因為那個 backend 本次不建立，見 §23。）

JSON：

```bash
hufu decisionrt backends --json
```

```json
{
  "backends": [
    {
      "name": "rule",
      "available": true,
      "type": "deterministic"
    },
    {
      "name": "sidecar",
      "available": false,
      "type": "generative",
      "reason": "missing_sidecar_model"
    }
  ]
}
```

不要把 secrets、API keys、provider credentials 顯示出來。

`backends` 只接受 `--sidecar-model`、`--provider-url` 與 `--json`；不接受
API key 或 decision/request flags，也不發出網路 request。`reason` 是
固定 machine code；available 時省略，unavailable 時只能是
`missing_sidecar_model`、`invalid_sidecar_model`、`invalid_provider_url`，不得
包含底層 error 或 secret。多個問題同時存在時固定依上述列出順序回第一個
reason。

---

## 53. CLI implementation layout

新增：

```text
cmd/hufu/
    decisionrtcmd.go
    decisionrtcmd_test.go
    decisionrt_input.go
    decisionrt_output.go
    decisionrt_registry.go
```

不要使用 `decidecmd.go`：這個名字與既有 `cmd/hufu/decisioncmd.go`
（`hufu decision`／`DecisionEngine` 的既有 CLI）只差一個字，且容易與
§40 已明文排除的 `hufu decide`（`cmd/hufu/runcmd.go` 的
`newDecideCommand()`）混淆。檔名一律以 `decisionrt` 為字首，與套件名
`internal/decisionrt`、指令名 `hufu decisionrt` 保持一致。

CLI layer 責任：

```text
parse flags / JSON
       |
       v
build decisionrt.Request
       |
       v
validate
       |
       v
construct Runtime from command flags
       |
       v
Runtime.Decide()
       |
       v
render result
```

CLI layer 不得：

- duplicate core validation
- parse model-specific result
- implement candidate scoring
- implement calibration
- know any specific logits inference backend's protocol
- mutate DecisionEngine records

`newDecisionRTCommand` 必須接受 dependency struct，至少可注入
`BackendRegistry` factory、stdin、stdout、stderr、`stdinIsTerminal` 與
`getenv`。Unit tests 只使用 fake dependencies；default dependencies 才能建立
真實 sidecar/provider 或讀取 `HUFU_PROVIDER_API_KEY`。
Command constructor 必須由 `newRootCommand()` 明確
`AddCommand(newDecisionRTCommand(defaultDecisionRTDeps()))`。每個 leaf 的
`SetFlagErrorFunc` 與 Args validator 都必須使用 §48 的 exit-2 wrapper，確保
Cobra parse error 不會退回 process exit 1。

Root 的既有 `PersistentPreRun` 會經 `configureOutputRendering()` 讀取
`hufu.yaml` presentation config。為遵守 §19，`decisionrt` parent 必須定義自己
的 no-op `PersistentPreRun`，讓其 leaf 不執行 root hook；這些 leaf 不使用全域
Lip Gloss styles。不得為此修改其他 command 的 root hook。

---

## 54. CLI tests

必須包含：

### choice

- successful choice through fake sidecar
- duplicated option
- one option only
- invalid option syntax
- context scalar parsing, duplicate rejection and file<json<flag precedence
- JSON output

### boolean

- successful true/false through fake sidecar
- invalid extra option flags rejected

### integer

- successful bounded value through fake sidecar
- missing min/max
- min > max
- range wider than 21 values
- backend returns out-of-range value

### run

- valid file
- valid stdin
- implicit non-TTY stdin and missing TTY input
- file + stdin conflict
- malformed JSON
- duplicate JSON key
- trailing JSON value
- input over 1 MiB
- unknown kind

### validate / backends / receipt

- validate success/digest and invalid input; registry factory call count remains zero
- backends stable order, availability/reason, no provider construction/network call
- `--receipt` always emits exactly the JSON envelope and includes schema version/digest

### exit codes

驗證：

```text
DECIDED -> 0
invalid request -> 2
ABSTAINED -> 3
backend error -> 4
backend unavailable -> 5
```

至少一組 command-constructor test 要對每個錯誤斷言 `ProcessExitCode()`；另以
既有 process-test pattern 驗證實際 binary 的 0/2/3/4/5 不被 `main()` 改成 1。

### stdout/stderr

必須測：

- successful human output stdout only
- JSON stdout contains only valid JSON
- error message stays stderr
- debug log does not contaminate stdout
- abstained output is stdout-only
- API key and wrapped provider error text never appear in either output

### CLI independence from team config

在 working directory 放入內容故意無效的 `team.yaml` 與 `hufu.yaml`，證明
`hufu decisionrt` 仍依 flags 執行且不讀兩者。另測：省略 `--backend` 時固定
使用 available 的 `rule`，輸出 `ABSTAINED` 並 exit 3；不得回 config error
或隱式建立 sidecar。

### backend and fallback

- unknown backend -> exit 5
- sidecar missing model -> exit 5, no fallback
- sidecar failure with default fallback -> rule abstains, exit 3
- sidecar failure with `--no-fallback` -> exit 4
- `backends` availability/reason 與 flags 一致且不做 network call
- explicit inherited global flags、missing/unknown subcommand -> exit 2
- `--help` -> exit 0

---

## 55. CLI safety

`hufu decisionrt` 是 decision-only command。

例如：

```bash
hufu decisionrt choice \
  --option delete="Delete database" \
  --option keep="Keep database"
```

即使輸出：

```text
delete
```

CLI 也不得自動執行 delete。

Unix composition 由使用者明確處理：

```bash
ACTION="$(hufu decisionrt choice ...)"

case "$ACTION" in
  delete)
    ./authorized-delete-command
    ;;
esac
```

DecisionPrimitive 本身永遠不擁有 action authority（執行權限）。

---

## 56. CLI acceptance criteria

額外驗收條件：

- [x] `hufu decisionrt choice` 可直接使用。
- [x] `hufu decisionrt boolean` 可直接使用。
- [x] `hufu decisionrt integer` 可直接使用。
- [x] `hufu decisionrt run --file` 可接收完整 JSON request。
- [x] `hufu decisionrt run --stdin` 可供 pipe 使用。
- [x] `hufu decisionrt validate` 不呼叫 backend。
- [x] `hufu decisionrt backends` 可列出 backend availability。
- [x] human-readable stdout 只輸出 decision value。
- [x] `--json` 提供 stable machine schema。
- [x] `--receipt` 可輸出 request digest 與 runtime receipt。
- [x] `--backend` 可 explicit 選 backend。
- [x] `--no-fallback` 可供 diagnostics。
- [x] ABSTAINED 與 technical failure 有不同 exit code。
- [x] CLI 不建立 `DecisionRecord`。
- [x] CLI 不觸發 tool/action/side effect。
- [x] CLI command handler 不包含 provider-specific inference implementation。
- [x] CLI tests 覆蓋 stdout/stderr 與 exit-code contract。

---

## 57. Updated Definition of Done

除了 Section 38 的 Definition of Done，還必須滿足：

```text
A user or shell script can invoke DecisionPrimitive directly through `hufu decisionrt`
without constructing an agent team or running DecisionEngine.

The CLI preserves the same typed/bounded semantics as the Go API,
supports machine-readable input/output,
distinguishes decision abstention from technical failure,
and never converts a model decision into an implicit side effect.
```
