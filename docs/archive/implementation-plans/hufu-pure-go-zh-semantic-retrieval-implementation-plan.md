# Hufu Pure-Go 語意檢索基礎實作計畫

> Status: archived — implemented and verified
> Target: hufu semantic retrieval infrastructure
> Baseline-Commit: `aba2164fdffb02e41a49b423543349849f3dd435`
> Completed: 2026-09-16
> Delivery constraint: 單一 coding agent 必須能在目前 repository、無網路、無 release credential、無人工標註或人工核准的條件下完成與驗收。
> Production behavior: 完成本計畫後新pure-Go semantic path仍不會成為可由production config啟用的功能；現有Exact/FTS與legacy vector caller行為保持不變。

## Implementation record

本計畫依可獨立merge與rollback的五個階段完成：

| Stage | Commit | Result |
|---|---|---|
| PR-LOCAL-01 | `904a270` | Pure-Go static embedding runtime、strict loader、WordPiece、pooling與synthetic fixture tests |
| PR-LOCAL-02 | `254b6c9` | SQLite migration 9、inventory digest、immutable generation repository與atomic activation |
| PR-LOCAL-03 | `7810564` | authorized flat semantic index、`AllowedItemIDs`、heap Top-K與concurrent refresh tests |
| PR-LOCAL-04 | `1952412` | off/shadow/active hybrid seam、typed fallback、cancellation passthrough與content-free trace |
| PR-LOCAL-05 | `1900829` | worker options seam、optional semantic identity及checkpoint/event/receipt/JSON/report propagation |

最終驗證在上述五個commit完成後執行並成功：

```text
go build ./cmd/hufu
CGO_ENABLED=0 go build ./cmd/hufu
go test ./...
CGO_ENABLED=0 go test ./...
go test -race -timeout=30m ./internal/embedding/... ./internal/context/... ./internal/team/...
go vet ./...
golangci-lint run
```

完整測試過程曾遇到既有 `internal/evalharness` scripted-step時序案例的非決定性失敗；精確案例連續重跑五次通過，之後 normal與no-CGO完整repository gate也各自取得成功結果。沒有因此略過或縮小最終驗證範圍。

此完成紀錄只證明本文件界定的infrastructure seam；production model、quality claim、operator enablement與rollout/release核准仍明確不在範圍內。

---

# 1. 交付範圍

本計畫只保留可以獨立完成的工程基礎：

1. pure-Go static embedding runtime contract；
2. 由 Go test在 `t.TempDir()` 產生的 tiny deterministic model fixture；
3. SQLite migration 9 與 immutable semantic projection generations；
4. canonical authorization-before-scoring 的 in-memory flat index；
5. `HybridRetrieveWithOptions` 的 off/shadow/active內部語意；
6. shadow byte-parity、cancellation與fallback contract；
7. optional semantic retrieval identity在manifest、checkpoint、event與receipt的傳播；
8. 完整local build、test、vet與lint驗收。

本計畫的完成狀態是：

```text
production retrieval behavior unchanged
pure-Go semantic infrastructure compiled and tested
no production semantic model
no network path
no operator-selectable semantic mode
no active rollout
```

不得以本計畫完成為由宣稱已提供production繁中語意檢索。

---

# 2. 明確邊界

本計畫不建立下列deliverable或command：

```text
production Chinese model artifact
teacher-model distillation pipeline
model download/install/remove CLI
compiled production model registry
human-labelled retrieval corpus
quality-gate approval
operator semantic configuration
production shadow/active rollout
default-active decision
release publication
chromem deprecation/removal
```

這些項目不是本計畫的前置條件；本計畫所有測試只能使用repository code與測試期間產生的synthetic fixtures。

本計畫不得新增third-party Go module；只能使用stdlib與repository既有dependencies。既有Go toolchain/module cache視為repository開發基線，不引入feature-specific下載步驟。

---

# 3. Baseline

目前canonical retrieval flow：

```text
Query
  ├── DecomposeQuery
  ├── SearchExact
  ├── SearchLexical / FTS5
  ├── optional VectorSearcher
  ├── RRF
  └── MMR
```

`workspace/context.sqlite` / `ContextItem` 是唯一canonical truth。FTS、legacy chromem及本計畫新增的semantic vectors都只能是rebuildable projection。

現有production worker以：

```go
NewWorkerMemoryService(repo, nil)
```

運作。本計畫不得改變此production wiring。

---

# 4. Normative invariants

## INV-LOCAL-01 — Canonical truth remains SQLite ContextItem

不得：

- 從vector row reconstruct canonical `ContextItem`；
- vector存在但canonical row不存在時回傳結果；
- semantic score修改lifecycle、authority、confidence或evidence；
- projection write新增 `context_events`或推進canonical revision。

## INV-LOCAL-02 — Production behavior remains unchanged

完成所有PR後：

- 不新增production semantic config；
- 不新增implicit model load；
- 不新增network request；
- `NewWorkerMemoryService(repo, nil)` 仍是production wiring；
- existing `HybridRetrieve` callers保持既有語意。

## INV-LOCAL-03 — CGO remains disabled

production dependency不得要求：

```text
import "C"
ONNX Runtime
libtorch
native tokenizer
native vector database
```

## INV-LOCAL-04 — Cancellation is not fallback

只有embedding、projection或semantic scoring的component-local error可以降級。

```go
if errors.Is(err, context.Canceled) ||
   errors.Is(err, context.DeadlineExceeded) {
    return nil, trace, err
}
```

canonical repository error維持既有fatal semantics。

## INV-LOCAL-05 — Authorization precedes semantic Top-K

執行順序固定為：

```text
canonical scope/lifecycle authorization
→ optional lineage allowed-ID restriction
→ vector lookup and score
→ Top-K
```

unauthorized item不得占用Top-K、影響count或出現在trace。

## INV-LOCAL-06 — Shadow cannot affect runtime decisions

shadow的正式：

```text
selected IDs
ordering
scores
RetrievalInsufficient
prompt bytes
manifest bytes
```

必須與off完全相同。shadow semantic results只能進獨立content-free trace。

## INV-LOCAL-07 — Runtime identity is propagated atomically

若測試或未來caller使用active semantic identity，該identity必須一起穿過：

```text
MemoryInjectionManifest
ContextInjectionManifest
session checkpoint
event reducer
execution receipt
JSON/report projection
crash-resume clone/restore
```

不得只更新coordinator或其中一種輸出格式。

## INV-LOCAL-08 — Model identity is immutable

同一 `(model_id, revision)` 不得對應不同 `ManifestSHA256`、dimensions、table或tokenizer bytes。`BeginGeneration` 發現既有不同identity時必須拒絕，不得共用或覆寫projection rows。

---

# 5. Pure-Go embedding package

新增：

```text
internal/embedding/
├── embedder.go
├── model.go
├── loader.go
├── wordpiece.go
├── normalize.go
├── pooling.go
└── *_test.go
```

canonical owner是 `internal/embedding`；此package不得import `internal/context`或 `internal/team`。projection layer可以依賴embedding identity，反向依賴禁止。

## 5.1 Interface

```go
type ModelIdentity struct {
    ID              string
    Revision        string
    Dimensions      int
    ManifestSHA256  string
    TableSHA256     string
    TokenizerSHA256 string
}

type Embedder interface {
    Identity() ModelIdentity
    EmbedQuery(ctx context.Context, text string) ([]float32, error)
    EmbedDocument(ctx context.Context, text string) ([]float32, error)
    EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)
}
```

`model_hash` 一律指 `ManifestSHA256`；`TokenizerSHA256` 是 `vocab.txt` bytes的SHA-256，`TableSHA256` 是 `embeddings.f32` bytes的SHA-256。不得讓不同package另創identity算法。

## 5.2 Local verified-file seam

不實作production asset manager。loader只提供internal seam：

```go
type VerifiedModelFiles struct {
    ManifestPath string
    VocabPath    string
    TablePath    string
    Expected     ModelIdentity
}

func OpenVerifiedModel(files VerifiedModelFiles) (*Model, error)
```

`OpenVerifiedModel` 必須重新驗證manifest、vocab及table hashes；`Expected`不可省略。production code在本計畫內沒有建立 `VerifiedModelFiles` 的caller。

## 5.3 Minimal manifest

測試用schema v1：

```json
{
  "schema_version": 1,
  "id": "fixture-static-wordpiece-v1",
  "revision": "1",
  "dimensions": 4,
  "vocab_size": 8,
  "dtype": "float32-le",
  "pooling": "mean",
  "normalize": true,
  "query_prefix": "",
  "document_prefix": "",
  "tokenizer": {
    "type": "bert-wordpiece",
    "do_basic_tokenize": true,
    "do_lower_case": false,
    "tokenize_chinese_chars": true,
    "strip_accents": false,
    "unk_token": "[UNK]",
    "add_special_tokens": false,
    "skip_token_ids": [],
    "max_input_chars_per_word": 100,
    "max_input_tokens": 32
  },
  "files": {
    "vocab.txt": "<test-generated-sha256>",
    "embeddings.f32": "<test-generated-sha256>"
  }
}
```

parser使用strict JSON decoding並拒絕unknown fields/version、duplicate token、invalid UTF-8、invalid hash、dimension/value overflow及file-length mismatch。

## 5.4 Test fixture construction

不得提交production weights或執行Python。Go test helper在 `t.TempDir()` 中deterministically產生：

```text
8-token vocab.txt
8 × 4 float32-le embeddings.f32
model.json
expected vectors
```

fixture至少包含：

```text
[UNK]
[PAD]
協
調
器
resume
失
敗
```

tests不得下載檔案、讀user cache或依賴environment-specific model。

## 5.5 Tokenizer and pooling contract

V1 fixture runtime固定：

- BasicTokenizer cleanup；
- Chinese character splitting；
- punctuation splitting；
- WordPiece longest-match-first；
- case-sensitive；
- 不加入special tokens；
- `[UNK]` 作為正常pooling token；
- 不產生padding；
- tokenization後截斷到manifest limit；
- zero token回傳 `ErrNoEmbeddableTokens`；
- arithmetic mean；
- final L2 normalization。

pooling前跳過manifest `skip_token_ids`；全部被跳過時回傳 `ErrNoEmbeddableTokens`。norm為0、NaN或Inf時回傳 `ErrInvalidEmbedding`。tokenization、batch embedding及flat scan必須定期檢查caller context。

不得依賴外部tokenizer library或其defaults。

BasicTokenizer/WordPiece的exact contract：

- invalid UTF-8直接error；不做NFC/NFKC；
- 移除U+0000、U+FFFD及Unicode category `Cc`/`Cf` control，唯 `\t`、`\n`、`\r` 視為whitespace；
- ASCII space、`\t`、`\n`、`\r`及Unicode `Zs` 統一成單一token boundary；
- CJK ranges `3400–4DBF`、`4E00–9FFF`、`F900–FAFF`、`20000–2A6DF`、`2A700–2B73F`、`2B740–2B81F`、`2B820–2CEAF`、`2F800–2FA1F` 的每個rune獨立成token；
- ASCII punctuation ranges `21–2F`、`3A–40`、`5B–60`、`7B–7E` 及Unicode category `P*` 各自切開；
- `do_lower_case=false`，`strip_accents=false`，因此不改case或combining marks；
- WordPiece以Go rune sequence做longest-match-first，非首piece加 `##`；
- word超過100 runes或無法完整分解時只產生一個 `[UNK]`；
- token ID等於 `vocab.txt` zero-based line number，duplicate/empty vocab line一律拒絕。

## 5.6 Loader and input limits

loader在model allocation前驗證model limits；Embed methods在token/input allocation前驗證input limit：

```text
dimensions <= 2048
vocab_size <= 1,000,000
table bytes <= 512 MiB
token bytes <= 1 MiB per input
all float32 values finite
exact table length = vocab_size * dimensions * 4
```

`OpenVerifiedModel` 是同步constructor；成功後回傳immutable、concurrent-safe `Model`，由caller持有。constructor失敗不建立global cache或永久latch，下一次明確呼叫可重新驗證；本計畫不新增lazy global loader。

---

# 6. SQLite migration 9

目前repository已有immutable migrations 1～8。新增：

```text
Version 9
semantic_embedding_generations
```

不得修改既有migration。

## 6.1 Schema

```sql
CREATE TABLE context_embedding_generations (
    generation_id   TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL,
    model_id        TEXT NOT NULL,
    model_revision  TEXT NOT NULL,
    model_hash      TEXT NOT NULL,
    source_revision INTEGER NOT NULL,
    state           TEXT NOT NULL
                    CHECK (state IN ('building','active','superseded','failed')),
    expected_count  INTEGER NOT NULL,
    row_count       INTEGER NOT NULL DEFAULT 0,
    content_digest  TEXT NOT NULL,
    created_at      INTEGER NOT NULL,
    activated_at    INTEGER
);

CREATE UNIQUE INDEX idx_context_embedding_generations_active
ON context_embedding_generations(project_id, model_id, model_revision)
WHERE state = 'active';

CREATE TABLE context_embeddings (
    generation_id TEXT NOT NULL,
    item_id        TEXT NOT NULL,
    content_hash   TEXT NOT NULL,
    dimensions     INTEGER NOT NULL,
    vector         BLOB NOT NULL,
    updated_at     INTEGER NOT NULL,

    PRIMARY KEY (generation_id, item_id),
    FOREIGN KEY (generation_id)
        REFERENCES context_embedding_generations(generation_id)
        ON DELETE CASCADE,
    FOREIGN KEY (item_id)
        REFERENCES context_items(id)
        ON DELETE CASCADE
);

CREATE INDEX idx_context_embeddings_item
ON context_embeddings(item_id);
```

所有 `*_at` 使用UTC Unix milliseconds。

## 6.2 Canonical inventory

generation scope是一個workspace SQLite中的 `project_id`。inventory包含該project所有canonical `context_items`，不因lifecycle、visibility或query scope排除。

排序與digest算法固定為：

1. `item_id` bytewise ascending；
2. 每列寫入unsigned-varint length + raw `item_id`；
3. 再寫unsigned-varint length + lowercase `content_hash`；
4. 對完整byte stream取SHA-256。

document embedding input只使用 `ContextItem.Content`，不拼入ID、scope、metadata、evidence或tags。

## 6.3 Generation lifecycle

```text
capture canonical revision + inventory in one read transaction
→ create building generation
→ append rows in bounded batches
→ BEGIN IMMEDIATE
→ recompute row count/digest and validate vectors
→ re-read canonical revision/inventory digest
→ reuse equivalent active or atomically activate new generation
→ COMMIT
```

規則：

- building/failed generation不可由search/load API讀取；
- active generation不可in-place update；
- projection write不得新增 `context_events`；
- activation drift最多重建一次，第二次仍drift就fallback；
- 等價active已存在時，candidate標成superseded並回傳既有winner；
- 保留active及最新兩個superseded generations；
- 清除超過24小時的building/failed generations；
- cleanup永遠拒絕刪除active generation。

---

# 7. Projection repository API

```go
type EmbeddingInventoryItem struct {
    ItemID      string
    Content     string
    ContentHash string
}

type EmbeddingInventory struct {
    ProjectID string
    Revision  int64
    Digest    string
    Items     []EmbeddingInventoryItem
}

type GenerationSpec struct {
    GenerationID   string
    ProjectID      string
    Model          embedding.ModelIdentity
    SourceRevision int64
    ExpectedCount  int
    ContentDigest  string
}

type EmbeddingRow struct {
    ItemID      string
    ContentHash string
    Dimensions  int
    Vector      []float32
}

type CanonicalEmbeddingInventory interface {
    LoadEmbeddingInventory(ctx context.Context, projectID string) (EmbeddingInventory, error)
}

type SemanticProjectionRepository interface {
    BeginGeneration(ctx context.Context, spec GenerationSpec) (Generation, error)
    AppendGenerationRows(ctx context.Context, generationID string, rows []EmbeddingRow) error
    ActiveGeneration(ctx context.Context, projectID string, model ModelIdentity) (Generation, error)
    LoadActiveGeneration(ctx context.Context, generationID string) ([]EmbeddingRow, error)
    ActivateGeneration(
        ctx context.Context,
        generationID string,
        expectedCanonicalRevision int64,
        expectedContentDigest string,
    ) (Generation, error)
    MarkGenerationFailed(ctx context.Context, generationID string) error
    CleanupInactiveGenerations(
        ctx context.Context,
        policy GenerationCleanupPolicy,
    ) (int64, error)
}
```

`LoadEmbeddingInventory` 必須在同一個SQLite read transaction取得revision與rows。`GenerationID` 由 `crypto/rand` 產生，不接受user/path input。

`SQLiteRepository` 可以實作interface，但embedding computation不得進入repository。

---

# 8. SemanticIndex

新增：

```text
internal/context/semantic_index.go
internal/context/semantic_index_test.go
```

## 8.1 Snapshot

```go
type SemanticIndex struct {
    repo       Repository
    projection SemanticProjectionRepository
    embedder   embedding.Embedder

    mu             sync.RWMutex
    refreshMu      sync.Mutex
    model          embedding.ModelIdentity
    generationID   string
    sourceRevision int64
    snapshot       flatVectors
}

type flatVectors struct {
    IDs        []string
    Data       []float32
    Dimensions int
}
```

query前比較canonical revision與active generation ID。任一不同時build/reload新snapshot；repository、tokenization與embedding不得在持有index write lock時執行。完成後以短write lock一次swap所有snapshot identity與data。

另一process即使在相同canonical revision啟用新generation，generation ID改變也必須觸發reload。

## 8.2 Authorization iterator

```go
type RetrievalCandidateIterator interface {
    IterateRetrievable(
        ctx context.Context,
        req SearchRequest,
        fn func(ContextItem) error,
    ) error
}
```

iterator直接重用canonical repository的 `compileRepositoryPredicates` / `scopeAuthorize`或其當下single owner，涵蓋：

```text
project/team/session/branch/agent/task/attempt scope
visibility
lifecycle
superseded
valid_from/valid_until/expires_at
kind
minimum confidence
```

不得複製另一套authorization semantics。

`SearchRequest` 新增optional `AllowedItemIDs []string`。constructor copy、dedupe並bytewise sort；iterator在canonical authorization後、vector scoring前以set限制。此filter只能縮窄，不能授權canonical predicate拒絕的item。

Top-K後不得呼叫unscoped `GetMany`。結果直接使用iterator已授權item，或再次使用相同canonical predicate的authorized batch getter。

## 8.3 Scoring

所有vectors均L2 normalized，因此similarity使用pure-Go dot product。V1使用fixed-size min-heap：

```text
O(N log K)
default K = 20
deterministic tie-break = score desc, item ID asc
```

不得引入assembly、unsafe SIMD、mmap、HNSW或vector DB。

若current active generation缺row、content hash不符、dimension錯誤或含NaN/Inf，整個semantic snapshot視為corrupt；本query不得回傳partial semantic ranking，必須fallback Exact + FTS。

---

# 9. Hybrid retrieval seam

保留既有 `VectorSearcher` 與 `HybridRetrieve`，避免修改legacy caller語意。

`RetrievalMode`、`SemanticFallbackReason`、`HybridRetrievalOptions`與 `SemanticRetrievalTrace` 的canonical owner都是 `internal/context`。

新增：

```go
type RetrievalMode string

const (
    RetrievalOff    RetrievalMode = "off"
    RetrievalShadow RetrievalMode = "shadow"
    RetrievalActive RetrievalMode = "active"
)

type TraceHasher interface {
    HashString(string) string
}

type HybridRetrievalOptions struct {
    Vector            VectorSearcher
    Mode              RetrievalMode
    UnavailableReason SemanticFallbackReason
    TraceHasher       TraceHasher
    TraceSink          func(SemanticRetrievalTrace)
}

func HybridRetrieveWithOptions(
    ctx context.Context,
    repo Repository,
    req SearchRequest,
    opts HybridRetrievalOptions,
) ([]SearchResult, RetrievalTrace, error)
```

既有 `HybridRetrieve(..., vector)` 包裝成 `Mode=active`，維持legacy compatibility。新API規則：

```text
off    → 不呼叫VectorSearcher；正式結果沿用Exact + FTS
shadow → 執行semantic並寫獨立trace；正式結果只使用Exact + FTS
active → semantic加入現有RRF/MMR；component-local failure回退Exact + FTS
```

本計畫不建立任何production caller選擇shadow或active。active只由unit/integration tests與existing legacy wrapper覆蓋。

options必須在任何repository/model call前驗證：unknown mode拒絕；`off` 不可帶Vector、UnavailableReason、TraceHasher或TraceSink；TraceHasher與TraceSink必須同時為nil或同時非nil；`shadow|active` 且Vector為nil時必須提供非 `none` 的UnavailableReason。validation failure不觸發fallback，直接回傳configuration error。

提供 `NewHMACTraceHasher(key []byte) (TraceHasher, error)`：key至少32 bytes、constructor defensive copy、輸出lowercase hex HMAC-SHA256。`TraceSink != nil` 時 `TraceHasher` 必須非nil，否則在任何repository/model call前回傳configuration error。helper只把query與item IDs交給hasher，sink永遠收不到raw values；tests使用固定32-byte key。

semantic query固定使用：

```go
strings.TrimSpace(DecomposeQuery(req.Query).Remainder)
```

remainder空時不embed，fallback reason為 `empty_remainder`。

shadow不得填入既有 `RetrievalTrace.VectorResults`，也不得影響 `RetrievalInsufficient`。

---

# 10. Content-free semantic trace

既有 `RetrievalTrace` 含query與完整 `SearchResult`，只能是ephemeral object，不得持久化。

新增獨立型別：

```go
type SemanticRetrievalTrace struct {
    TraceID                string
    QueryHMAC              string
    Mode                   RetrievalMode
    ModelID                string
    ModelRevision          string
    ModelHash              string
    GenerationID           string
    SourceRevision         int64
    LexicalResultHashes    []string
    SemanticResultHashes   []string
    SelectedResultHashes   []string
    HashesTruncated        bool
    IntersectionCount      int
    SemanticCandidateCount int
    LexicalResultCount     int
    SemanticResultCount    int
    SelectedResultCount    int
    LatencyMillis          int64
    FallbackReason         SemanticFallbackReason
}
```

本計畫只實作injectable `TraceSink` 與in-memory test sink，不新增production JSONL writer或CLI。

`TraceID` 使用 `crypto/rand` 產生128-bit hex ID。hash list最多保留前32個deterministic ranked IDs。tests使用固定HMAC key；production key management不在本計畫內。不得把raw query、content、ContextItem ID、tokens、vectors或raw error放進semantic trace。

V1 fallback enum：

```text
none
empty_remainder
asset_missing
asset_invalid
projection_missing
projection_stale
projection_refresh_failed
projection_corrupt
embedding_failed
invalid_vector
```

`off` 不產生semantic trace。cancellation/deadline直接回傳error，不是fallback。

---

# 11. Worker seam and manifest propagation

新增internal options constructor，但production caller不切換：

```go
type WorkerMemoryOptions struct {
    Semantic          context.VectorSearcher
    Mode              context.RetrievalMode
    UnavailableReason context.SemanticFallbackReason
    TraceHasher       context.TraceHasher
    TraceSink          func(context.SemanticRetrievalTrace)
}

func NewWorkerMemoryServiceWithOptions(
    repo context.Repository,
    opts WorkerMemoryOptions,
) *WorkerMemoryService
```

既有constructor保留：`nil`維持off，non-nil legacy vector維持原語意。新pure-Go semantic code不接入production coordinator。

## 11.1 Semantic retrieval identity

```go
type SemanticRetrievalIdentity struct {
    Mode                   context.RetrievalMode         `json:"mode"`
    ModelID                string                        `json:"model_id"`
    ModelRevision          string                        `json:"model_revision"`
    ModelHash              string                        `json:"model_hash"`
    GenerationID           string                        `json:"generation_id"`
    SourceRevision         int64                         `json:"source_revision"`
    RetrievalPolicyVersion string                        `json:"retrieval_policy_version"`
    FallbackReason         context.SemanticFallbackReason `json:"fallback_reason"`
}
```

V1 policy version固定為：

```text
hybrid-exact-fts-semantic-rrf60-mmr075-v1
```

canonical owner是 `internal/team`。`MemoryInjectionManifest` 與 `ContextInjectionManifest` 都新增：

```go
Semantic *SemanticRetrievalIdentity `json:"semantic_retrieval,omitempty"`
```

欄位非nil時納入canonical fingerprint，並傳播到clone、checkpoint、event reducer、receipt、JSON及report；nil時既有JSON bytes與fingerprint input不得改變。

backward compatibility：舊JSON缺欄位時視為legacy/off，不回填、不重算舊fingerprint、不改寫歷史event。

shadow不得設定正式manifest identity；active contract只由synthetic integration test驗證。

resume已保存manifest時不得重新retrieval，也不得因generation變動重播已完成side effect。

---

# 12. Failure semantics

| Failure | Required behavior |
|---|---|
| fixture/model missing | Exact + FTS |
| hash/schema mismatch | reject model；Exact + FTS |
| tokenizer/embedding error | Exact + FTS |
| projection missing/stale | bounded refresh；失敗則Exact + FTS |
| building generation | 不可見 |
| activation revision drift | 最多重試一次，之後Exact + FTS |
| corrupt/missing active row | invalidate entire semantic snapshot；Exact + FTS |
| canonical DB error | preserve existing fatal semantics |
| `context.Canceled` | propagate immediately |
| `context.DeadlineExceeded` | propagate immediately |

任何fallback只能記固定enum，不能持久化raw error。

---

# 13. Files expected to change

```text
internal/embedding/*

internal/context/model.go
internal/context/retrieval.go
internal/context/retrieval_test.go
internal/context/repository.go
internal/context/sqlite_repository.go
internal/context/sqlite_repository_test.go
internal/context/semantic_index.go
internal/context/semantic_index_test.go
internal/context/scope_matrix_test.go
internal/context/scope_visibility_test.go

internal/team/worker_memory.go
internal/team/worker_memory_recall_test.go
internal/team/worker_memory_lineage_test.go
internal/team/memory_learning.go
internal/team/context_manifest.go
internal/team/execution_receipt.go
internal/team/structured_execution_receipt_validation.go
internal/team/event_reducers.go
internal/team/projection_shadow.go
internal/team/status.go
internal/team/coordinator_session.go
internal/team/session.go
internal/team/session_data_sync.go

cmd/hufu/json_output.go
cmd/hufu/report.go

docs/reference/context-sqlite-schema.md
docs/architecture/memory-learning.md
```

不得修改：

```text
production model release workflow
release configuration
semantic install/download CLI
normal coordinator semantic wiring
legacy chromem migration/removal
```

---

# 14. Independently completable PR plan

每個PR都必須可單獨merge、rollback及驗收，不依賴後續PR才恢復correctness。

## PR-LOCAL-01 — Pure-Go embedding runtime

工作：

- `internal/embedding` interfaces與strict loader；
- WordPiece subset；
- mean pooling與L2 normalization；
- Go-generated tiny fixture；
- malformed manifest/table/tokenizer tests；
- fuzz parser/loader/tokenizer。

驗收：

```text
no network
no Python
no production asset
no CGO
deterministic expected vectors
all loader limits enforced before allocation
```

## PR-LOCAL-02 — Migration 9 and generation repository

工作：

- migration 9；
- inventory snapshot/digest；
- generation repository API；
- atomic activation；
- equivalent-winner handling；
- cleanup policy。

驗收：

```text
migration 8 → 9
fresh DB → 9
pre-migration backup/checksum coverage
building generation never visible
same-revision rebuild never exposes partial rows
two-process activation has one valid winner
projection writes do not advance canonical revision
ContextItem delete cascades projection rows
```

## PR-LOCAL-03 — Flat SemanticIndex

工作：

- immutable contiguous snapshot；
- generation/source revision reload；
- authorization iterator；
- `AllowedItemIDs` pre-score restriction；
- Top-K heap與deterministic tie-break；
- concurrent query/refresh tests。

驗收：

```text
scope leakage = 0
lifecycle leakage = 0
lineage leakage = 0
high-scoring unauthorized item cannot occupy Top-K
corrupt snapshot never returns partial semantic results
go test -race coverage for concurrent query/refresh
```

## PR-LOCAL-04 — Hybrid off/shadow seam

工作：

- `HybridRetrieveWithOptions`；
- exact/lexical/semantic separation；
- `QueryParts.Remainder`；
- cancellation passthrough；
- in-memory content-free trace sink；
- golden regression tests。

驗收：

```text
shadow selected IDs/order/scores == off
shadow RetrievalInsufficient == off
shadow prompt/manifest bytes == off
shadow semantic results do not enter RetrievalTrace.VectorResults
semantic component failure preserves Exact + FTS
cancellation/deadline propagate
existing HybridRetrieve callers unchanged
```

## PR-LOCAL-05 — Worker seam and receipt propagation

工作：

- internal options constructor；
- optional semantic identity；
- conditional fingerprint backward compatibility；
- clone/checkpoint/event/receipt/JSON/report propagation；
- resume tests；
- direct worker/DAG path parity tests。

驗收：

```text
production coordinator still passes nil semantic searcher
old session/event JSON remains readable
old fingerprints are not recomputed
new identity survives checkpoint and resume
receipt identity equals dispatched manifest identity
completed side effects are not replayed
shadow manifest remains byte-identical to off
```

---

# 15. Test matrix

## Embedding

```text
manifest strict decoding
hash mismatch
invalid dimensions/vocab/file length
NaN/Inf
invalid UTF-8
WordPiece longest match
Chinese splitting
punctuation
UNK
empty input
max-token truncation
mean pooling
L2 normalization
concurrent Embed calls on one immutable model
explicit constructor retry after corrected files
```

## Repository and generation

```text
migration 8 → 9
fresh DB → 9
backup
migration checksum mismatch
inventory deterministic ordering/digest
begin/append/activate
revision drift abort
equivalent active winner
building/failed invisibility
cleanup retention
cascade delete
projection write leaves canonical revision unchanged
```

## Retrieval and authorization

```text
exact-only
lexical-only
semantic-only synthetic test
mixed retrieval synthetic test
empty natural-language remainder
component fallback
cancellation/deadline
off/shadow byte parity
all scope dimensions
ancestors/exact/subtree visibility
candidate/rejected/superseded/expired/not-yet-valid
AllowedItemIDs lineage restriction before Top-K
```

## Persistence

```text
optional identity fingerprint
legacy manifest without identity
clone
checkpoint/save/load
event reduce/replay
execution receipt
JSON/report projection
crash-resume without re-retrieval
```

---

# 16. Required validation

每個修改code/test的PR都執行：

```bash
go build ./cmd/hufu
CGO_ENABLED=0 go build ./cmd/hufu
go test ./...
CGO_ENABLED=0 go test ./...
go test -race ./internal/embedding/... ./internal/context/... ./internal/team/...
go vet ./...
golangci-lint run
```

package-specific command只能作為額外診斷，不能取代完整repository gates。

tests不得：

- 發出network request；
- 讀取user model cache；
- 執行Python；
- 依賴Ollama、OpenAI或其他provider；
- 寫入workspace以外路徑；
- 要求人工判斷pass/fail。

---

# 17. Completion criteria

本計畫只有在以下全部成立時才完成：

```text
all five PR-local contracts implemented
all required validation commands exit 0
migration 9 is atomic and backward compatible
canonical authorization precedes semantic Top-K
shadow output is byte-identical to off
cancellation propagates
manifest/receipt identity is internally consistent
production semantic mode remains unreachable
no network/model/release dependency was introduced
```

完成後可交付的能力是「已驗證的semantic retrieval infrastructure seam」，不是production model、quality claim或rollout approval。
