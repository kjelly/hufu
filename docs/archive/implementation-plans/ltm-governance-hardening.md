# Hufu LTM 治理補強：實作計畫

> Status: in implementation — archived 2026-09-23 from local scratch space before implementation began
> Authority: reference
> Priority: P1
> Baseline: `ee72f72396ec4de8f842cdd4d0e050376a7656e4`
> Risk: medium — 兩個 additive SQLite migration、一個離線模型呼叫命令，以及
> promotion/consolidation 的 fail-closed gate；不改變 completion、retry、ranking 與 prompt 內容
> Normative inputs: [memory-learning](../../architecture/memory-learning.md)、
> [memory-promotion](../../architecture/memory-promotion.md)、
> [knowledge-coverage](../../architecture/knowledge-coverage.md)
> Decisions: 2026-09-23 已由使用者確認（§1）；本文件不留待決事項
> Revision: v3 — v2 納入對照 baseline 的獨立查核結果（FTS 預篩不可行、apply 衝突語意、
> read-only 相容性、遺漏的呼叫點與測試）；v3 加入使用者確認的選用向量預篩（`--vector`）與
> 其前置修正（Ollama embedding model 名稱）

本文件取代原本位於本機 scratch space 的「LTM Promotion Governance Roadmap」。原 roadmap 大部分內容已由既有系統
實作，或與 normative 文件衝突；§0 記錄逐節處置，避免之後重新提案。其餘章節都是 coding agent
可以直接實作、可以用測試驗收的工作項目。

---

## 0. 原 roadmap 逐節處置

| 原章節 | baseline 現況 | 處置 |
| --- | --- | --- |
| §3 `memories` 表、§4 `claims`/`evidence` 表 | `internal/context.ContextItem`（`internal/context/model.go`）已有 `Confidence`、`ValidFrom`/`ValidUntil`/`ExpiresAt`、`SupersededBy`、`Lifecycle`、`Evidence []EvidenceRef`；另有 `ContextEdge` 與 `ExperienceAggregate` | **不做**。memory-learning.md §2 規定不新增第二套 LTM 或 experience DB |
| §5 硬門檻（reuse ≥ 2、不同 task ≥ 2、低 harm） | `internal/promotion/eligibility.go:81-85` | **已完成** |
| §5「no unresolved conflict」門檻 | 不存在 | **WP-5** |
| §5 PromotionScore 加權公式 | memory-promotion.md §5.3 已拒絕 | **不做** |
| §6 before/after success rate | 違反 memory-learning.md §3 invariant 4「Used 不等於 causal」；現行是 `MemoryUses` 歸因 + weighted Beta 下界，policy 層級 A/B 在 `internal/improve` | **不做** |
| §7 Contradiction resolver | `KnowledgeConflicting` 保留未產生（`internal/team/knowledge_state.go:11`）；`contradicts_ids` 只被 `cmd/hufu/context_consolidation_cmd.go:203` 讀取，沒有寫入者 | **WP-3、WP-4、WP-5** |
| §8 Promotion Compiler | `hufu context promotion analyze/list/show/review/edit/approve/reject/apply` 已完整 | **已完成**；steps 驗證不一致 → **WP-2** |
| §9 Dream-lite consolidation | 有 deterministic 分群 + 人工 `--proposal-text`；HF-MEM4-006 的 LLM 起草未接 | **WP-6** |
| §9 Cross-project learning | memory-promotion.md §1.1 列為非目標 | **不做** |
| §10 cache-friendly 排序、`stable_hash` 排序 | `RankContextItems`（`internal/team/context_compiler.go:387`）已 deterministic；hash 排序會破壞 reinforced ranking | **不做** |
| §10–12 cache 命中率 | `fantasy.Usage` 有 cache tokens，hufu 全部丟棄 | **WP-1**（只觀測） |
| §11 Prompt fingerprint（system/memory/task hash） | `ContextInjectionManifest` 已有 `Fingerprint`、`RequestHash`（invariant item 另有 `ContentHash`） | **不做**：命中率可觀測前，拆分 hash 沒有消費者 |
| §12 metrics | `internal/improve` 已有 `memory_*` 指標 | 缺的部分 → **WP-1、WP-2、WP-5** |
| §13 `hufu memory ...` CLI | memory-learning.md §2 規定不新增重疊的 `hufu memory` namespace | **不做**；衝突命令放在 `hufu context conflicts` |
| §14「False promotion rate < 5%」 | 沒有 ground truth，也沒有撤回流程 | **不做**；改用替代指標（WP-2、WP-5） |
| §14「Generated skills require minimal manual editing」 | 沒有記錄 draft 是否被修改 | **WP-2** |
| Auto-skill 與 promotion 管線 | 兩條管線不共用程式碼；`hufu skill promote` 不改寫 frontmatter name | **WP-7**（修 bug + 唯讀使用報告） |

---

## 1. 決策摘要（2026-09-23 使用者確認）

- **D1 衝突偵測**：離線預篩 + LLM 判定。新命令 `hufu context conflicts scan`，只有操作者執行時
  才呼叫模型，不進 run 熱路徑。
  - **預篩（2026-09-23 查核後再次確認）**：候選對來自三個來源的聯集，最後都交給 LLM judge：
    1. 預設：pure-Go 的 deterministic token overlap（含 CJK bigram）；
    2. 預設：共享 file-path evidence；
    3. 選用 `--vector`：重用 `hufu context query` 既有的 Ollama 向量庫
       （`contextstore.OpenOllamaVectorStore`），以每筆記憶已存的 embedding 找語意相近者，
       補上用詞不同的矛盾（例如「Use SQLite」與「migrated to PostgreSQL」）。向量不可用時
       自動退回前兩個來源。
  - 不使用 FTS5：`ftsQuery` 以空白串接 term，FTS5 視為 AND，整段記憶當 query 幾乎配不到東西；
    大寫 `AND`/`OR`/`NOT` 或純中文內容會讓 MATCH 報 syntax error
    （`internal/context/sqlite_repository.go:1342-1347`）。
  - 不使用 pure-Go 語意索引：`NewSemanticIndex` 在正式程式碼中沒有建構點，而且該計畫明確聲明
    沒有 production 模型（`docs/archive/implementation-plans/hufu-pure-go-zh-semantic-retrieval-implementation-plan.md`）。
- **D2 衝突效果**：標記並擋下升級。
  - prompt 仍注入兩筆，prompt 位元組不變；
  - manifest 標 `knowledge_state=conflicting`，coverage 多 `conflicting_count`；
  - promotion analyze 排除、apply 阻擋、consolidation 拒絕；
  - 不改 completion、acceptance、retry、ranking、token budget。
- **D3 衝突解決**：只由人工執行。沿用 `hufu context supersede <old> --with <new>`；誤報用
  `hufu context conflicts dismiss`。人工 dismiss 永遠不被後續掃描推翻。系統永不自動
  supersede、reject 或 expire 記憶。
- **D4 Prompt cache**：只觀測，不改 provider request、不設 cache_control。
- **D5 Skill 管線**：維持兩條管線。修 `hufu skill promote` 的 name bug；`hufu improve` 新增
  唯讀「已套用 skill promotion 使用情形」，只呈現相關性。
- **D6 成功標準**：以 rejected、stale、applied-but-edited promotion 數量與 open conflicts 數量
  取代 false promotion rate。

---

## 2. 範圍與非目標

### 2.1 本文件交付

| WP | 內容 | 依賴 |
| --- | --- | --- |
| WP-1 | Prompt cache token 觀測；`MemoryTokenOverhead` 分母修正；usage 計數不被遮蔽 | 無 |
| WP-2 | Promotion steps 驗證統一、`generated_draft_hash`（migration 10）、治理計數 | 無 |
| WP-3 | 記憶對判定資料模型（migration 11）、repository、read-only 相容 | WP-2（migration 序號） |
| WP-4 | Ollama embedding model 名稱修正；`VectorStore.SearchSimilarTo`；`hufu context conflicts scan/list/show/dismiss`（含選用 `--vector`） | WP-3 |
| WP-5 | 衝突效果：knowledge state、promotion gate、consolidation gate、learning 計數 | WP-3 |
| WP-6 | `hufu context consolidate --draft` | WP-4（共用 sidecar helper）、WP-5 |
| WP-7 | `hufu skill promote` 修正、已套用 skill 使用報告 | 無 |
| WP-8 | Normative 文件、schema 參考、命令參考同步 | 各 WP 同 PR |

### 2.2 非目標

- 不新增平行知識表。判定表 `context_pair_judgments` 只記錄「兩筆既有 `ContextItem` 的關係
  判定」，不存知識內容。不用 `context_edges` 的原因：edge 的主鍵是
  `(from_id, relation, to_id)`，無法記錄內容 hash、判定版本、審查狀態與模型；而且 edge 是知識
  圖譜關係，判定則是模型產生、可被人工推翻的審查紀錄。
- 不自動解決衝突；模型不能建立、supersede、reject 或 expire `ContextItem`。
- 衝突掃描不進 run 熱路徑，也不在 unattended run 中自動執行。
- 不比較不同 scope 的記憶：private agent 對 shared 不比較（§6.4）。
- runtime 衝突標記只套用在 shared persistent memory（與 `ExperienceAggregate` 覆蓋範圍一致）；
  promotion gate 仍涵蓋 agent-private 來源。
- 預篩不使用 FTS5，也不使用 pure-Go 語意索引（D1）；向量只走選用的 Ollama 向量庫。
- 不修 `internal/memory/store.go:167`（legacy memory store）同樣把帶 `ollama/` 前綴的
  model 名稱直接傳給 Ollama 的問題；本文件只修 `OpenOllamaVectorStore`（§7.4）。
- 不設定 cache_control，不改 provider request；hufu 目前只建立 openai/openaicompat provider
  （`internal/agent/agent.go:16-17`）。
- 不修改 `hooks.UsageSummary`（外部 hook payload）；不記錄 sidecar task 的 usage
  （`coordinator_task_run.go:3291-3373` 傳零值）；不改 `subagent_hufu.go:169-173` 自行加總的
  `AttemptResult.Usage`（事件不使用它）。
- 不統一 auto-skill 與 promotion 管線，不 deprecate `hufu skill promote`。
- 不修 `teamDefinitionRevision`（`internal/team/execution_events.go:763`）略過 `skills/`。
- 不實作 promotion 撤回/deprecate 流程（memory-promotion.md §12 item 3）。

---

## 3. 已驗證的程式碼現況（baseline `ee72f72`）

### 3.1 Usage 與 cache tokens

- `fantasy.Usage`（charm.land/fantasy v0.41.1 `model.go:11`）有 `CacheReadTokens`、
  `CacheCreationTokens`。openai/openaicompat 的對應（`providers/openai/language_model_hooks.go:199`、
  `:233`）：`InputTokens = max(prompt_tokens - cached_tokens, 0)`，
  `CacheReadTokens = cached_tokens`，`TotalTokens` 是 provider 的 `total_tokens`（含 cached）。
  因此 **`InputTokens` 是未命中 cache 的 prompt tokens**。
- `team.ExecutionUsage`（`internal/team/execution_events.go:33-49`）沒有 cache 欄位；
  `usageFromSteps`（`:207-229`）丟棄它們。所有 `ExecutionUsage` literal 都使用具名欄位。
- `executionEventSchemaVersion = 5`（`:28`）；沒有 reader 依版本拒絕事件。
- improve loader：`internal/improve/sqlite_analytics_loader.go:30-47`，
  `executionEventColumnCount = 26`（`:45`）；TEMP schema 在 `sqlite_analytics_schema.go:20-47`。
  既有 v3 字面資料列測試在 `sqlite_analytics_loader_test.go:153-193`。
- `MemoryTokenOverhead` 分母 CTE 在 `sqlite_analytics_memory.go:116-122`
  （`CASE WHEN e.input_tokens > 0 ...`，事件集合為 `e.team <> ''`），比值在 `:189-190`。
  有 cache 命中時分母偏小。`TotalTokens` 使用的事件集合不同（`task_id <> '' AND team <> ''`，
  `sqlite_analytics_queries.go:337-342`）。
- `utils.RedactJSONLData` 把 `input_tokens`、`output_tokens`、`total_tokens`、
  `progress_tokens` 的數值遮成 `"[REDACTED]"`（已實測；這些 key 不在
  `numericTelemetryKeys`，`internal/utils/redact.go:65`）。
- `llmLogStreamFinish`（`internal/team/llm_log.go:81-91`）的格式是
  `[ts] === RESPONSE finish_reason=… tokens_in=… tokens_out=…[ est_tokens_in≈…] ===`。
- `internal/improve/reporter.go:16` 以 Markdown 表格輸出 Metrics。

### 3.2 Promotion 管線

- `ValidateDraft(typ, draft, skillName, steps)`（`internal/promotion/validate.go:15`）有
  **五個**呼叫點，各自用不同方式計算 steps：
  - `internal/promotion/analyzer.go:84`：模型 JSON 的 `draft.Steps`；
  - `internal/promotion/service.go:30`（edit）與 `apply.go:89`：`policySteps`（`service.go:82-91`）；
  - `cmd/hufu/improve_handoff.go:215`：`skillDraftStepsForHandoff`（`:1119-1128`）；
  - `internal/improve/experiment.go:218`：`skillDraftSteps`（`:256-265`）。

  後三個 helper 規則相同：只算以 `- `、`1. `、`2. ` 開頭的行，並且把 frontmatter 也算進去。
  通過 analyze 的 skill draft 可能在 apply 時變成 `apply_failed`。
- `PromotionProposal`（`internal/context/promotion.go:52-70`）沒有記錄原始生成 draft。
  `PromotionProposalID`（`:84-96`）由 type、scope、target、base hash、source snapshots 與
  policy version 決定；`CreatePromotion` 命中既有 ID 時回傳原資料列（`:120`、`:144`）。
- `Apply`（`internal/promotion/apply.go:50-`）的順序：status → registry → target →
  `validateEvidence`（`:79`，失敗轉 stale）→ draft hash → `ValidateDraft`（`:89`）→ 讀 target
  → `alreadyWritten` 補寫 applied（`:100-104`）→ target hash 不符轉 stale（`:109-`）→ 寫入。
  `applyFailed`（`:295-301`）只 enqueue `memory_promotion_apply_failed`，不改 status。
- `internal/inspect/learning.go:89-112` 只計 proposed、approved、applied；失敗時把各計數個別設
  為 nil（`:89-98`）。`LearningView` 在 `internal/operator/types.go:139-156`；
  `internal/operator/snapshot.go:26-37`（`NormalizeSnapshot` 複製計數）、`:159-172`（負值檢查）、
  `:250-275`（snapshot hash 輸入）都要處理新欄位。`operator.SchemaVersion = 1`（`types.go:7`）。
- analyze 的 JSON 輸出已含 `diagnostics`；text 輸出只印 eligible 或
  `No suitable LTM entries found for promotion.`（`cmd/hufu/context_promotion_cmd.go:144-164`）。
- 治理事件走 `promotion_event_outbox`：`insertPromotionOutbox(ctx, *sql.Tx, event)`
  （`internal/context/promotion.go:356`，package 內可用）、`PendingPromotionEvents` 不過濾
  type；`flushPromotionEvents`（`cmd/hufu/context_promotion_cmd.go:307-329`）寫入
  `promotionWorkspacePath()` 的 event store，run ID 為 `promotion`。
- Sidecar 建構：`newPromotionGenerator`（`context_promotion_cmd.go:342-380`）讀取全域
  `promotionModel`（`:348`）與 `promotionWorkspacePath()`（`:362`）；測試透過
  `promotionGeneratorFactory`（`:24`）替換。`promotionRegistry()`（`:70-76`）讀全域
  `promotionSearchPath`。purpose `promotion_draft` 登錄在 `internal/team/context_purpose.go:57`；
  `ContextTriggerSidecarTask` 定義在 `internal/team/context_request.go:23`。
- `sidecar.Profile`（`internal/sidecar/sidecar.go:40`）；`ClassifierProfile` 的
  `MaxInputRunes` 為 8000（`:25`、`:71`），`ExecuteProfile` 超過上限直接回傳 error
  （`:487-492`）。auxiliary prompt 會在 24000 runes 截斷（`internal/team/auxiliary_context.go:14`、
  `:114`）。

### 3.3 記憶寫入、衝突與 knowledge state

- 持久記憶候選來源：`memory_save`、`ltm_update`（`coordinator_tools_memory.go`）、reflexion 與
  `AutoExtractLTM`（`coordinator_reflexion.go`、`coordinator_memory.go:300,357`、
  `experience_processor.go:34`），全部是自由文字，沒有主題 key。
- run 被 accept 時 shared candidates 自動 confirm（`internal/team/shared_memory.go:285-321` →
  `SQLiteRepository.ConfirmCandidates`，`internal/context/sqlite_repository.go:1083`）。只有帶
  `supersedes_ids` 時才取代舊記憶，所以互相矛盾的 confirmed 記憶可以並存。
- `hufu context reject` 只接受 candidate（`cmd/hufu/contextcmd.go:455`）；confirmed 記憶唯一
  的人工處置是 `hufu context supersede <old> --with <new>`（`:472-508`）。
- 空的 scope 欄位以 NULL 儲存（`nilIfEmpty`，`sqlite_repository.go:684-690`）。
- compiler aggregate 經 `CanonicalContextBundle.SharedPersistentAggregates`
  （`context_compiler.go:154-157`）帶入，建構點為 `context_shadow.go:138` 與
  `context_router.go:359`。`canonicalCompilerItemsScored`（`context_compiler.go:251`）的呼叫點
  為 `:736`、`:912` 與 `internal/team/knowledge_state_test.go:117`。
- `classifyKnowledgeState`（`knowledge_state.go:33`）的呼叫點為 `context_manifest.go:99` 與
  `knowledge_state_test.go:45,49`。coverage 顯示在 `cmd/hufu/report.go:60`、
  `cmd/hufu/inspectcmd.go:424-427`、`internal/inspect/evidence.go:69-70,202-203`。

### 3.4 Consolidation、skill、improve

- `hufu context consolidate --apply-proposal` 必須提供 `--proposal-text`
  （`cmd/hufu/context_consolidation_cmd.go:46-126`）；`--team` 是選填
  （`contextcmd.go:514`）。`validateConsolidationSources`（`:187`）是純函式，呼叫點在
  `:81` 與 `:291`（approve）。`internal/context/consolidation.go` 只有 Save/Get/Update。
- `skill.PromoteDraft`（`internal/skill/lifecycle.go:109-137`）不改寫 frontmatter `name:`；
  runtime 以 frontmatter name 為 skill 名稱（`internal/skill/skill.go:179`）。
  `ValidateSkillDraft` 回傳 `*SkillDef`，body 在 `SkillDef.Content`（`skill.go:19`、`:43-55`）。
  既有 `TestPromoteDraft`（`lifecycle_test.go:73-`）的 fixture 沒有 `description:`。
- improve 的 `task_summary`（`sqlite_analytics_task_summary.go:21-33`）沒有時間欄位；
  `execution_events` TEMP 表有 `timestamp_unix_ns`。`GroupedMetrics.BySkill` 由
  `groupBySkillQuery`（`sqlite_analytics_grouped.go:50-81`）產生，範圍為 selected runs。

### 3.5 Read-only repository

`OpenSQLiteReadOnly`（`internal/context/sqlite_repository.go:78-81`）從不套用 migration。
promotion list/show/review、`InspectLearning`、TUI promotion 詳情
（`cmd/hufu/display.go:1373`）與 improve 都可能以 read-only 開啟尚未升級的資料庫。
`schema_migrations` 的 `MAX(version)` 可以判斷版本（`sqlite_repository.go:138` 已有此查詢）。
`ReadOnlyRepository` 沒有其他實作或 fake。

### 3.6 生成的文件與超大檔案

- `docs/reference/operator-command-reference.md` 是生成檔，由
  `TestGeneratedOperatorCommandReferenceIsCurrent`（`cmd/hufu/operator_reference_test.go:12-25`）
  檢查；範例來源是 `canonicalExamples`（`cmd/hufu/cli_metadata.go:28-45`），以
  `go run ./cmd/hufu examples --format markdown` 重新產生。
- `docs/reference/context-sqlite-schema.md:21-31` 有 migration 對照表。
- 以下檔案超過 800 行，新邏輯一律放新檔，這些檔案只做最小接線：
  `internal/context/sqlite_repository.go`（1410）、`cmd/hufu/report.go`（1318）、
  `cmd/hufu/improve_handoff.go`（1147）、`cmd/hufu/contextcmd.go`（1003）、
  `internal/team/context_compiler.go`（966）、`internal/improve/experiment.go`（887）、
  `internal/team/coordinator_task_run.go`。

### 3.7 Ollama 向量庫（`--vector` 的基礎）

- `OpenOllamaVectorStore(workspace, model, ollamaURL)`（`internal/context/vector.go:56-57`）在
  `<workspace>/context-vectors` 建立 chromem-go collection，embedding 以
  `chromem.NewEmbeddingFuncOllama(model, url)` 呼叫 `<url>/embeddings`。呼叫點：
  `hufu context rebuild --vector`（`cmd/hufu/contextcmd.go:578`）與 `hufu context query`
  （`:598-604`，失敗時靜默退回 lexical）。
- `Rebuild(ctx, repo, scope)`（`vector.go:60-105`）：以 `VisibilitySubtree` 讀取 scope 內**所有**
  item（不分 lifecycle），先刪除整個 collection，再逐筆 embedding（每筆一次 Ollama 呼叫）。
  單筆失敗時把該 item 的 `embedding_state` 設為 `pending` 並繼續，最後回傳
  `errors.Join`（index 可能不完整）；成功者寫 `embedding_state=embedded`。
- `SearchVector`（`vector.go:107-142`）以 query 文字重新 embedding，查詢後逐筆從 SQLite hydrate，
  並以 `isRetrievable`（`:149-191`）套用 scope/visibility/lifecycle/validity 過濾；
  `Score` 是 chromem 的 cosine similarity。
- chromem-go v0.7.0 的 `Collection` 提供 `GetByID`（`collection.go:298`，回傳含 `Embedding`
  的 `Document`）與 `QueryEmbedding`（`:478`），可以用已存的向量查詢，不需要再次 embedding。
- **既有 bug（已實測）**：預設 `config.DefaultEmbeddingModel = "ollama/nomic-embed-text:latest"`
  （`internal/config/config.go:24`）原樣傳給 Ollama，Ollama 回傳
  `model "ollama/nomic-embed-text:latest" not found`；去掉前綴的 `nomic-embed-text:latest` 可以
  正常取得 embedding。因此在預設設定下，`hufu context query` 的向量路徑永遠退回 lexical，
  `hufu context rebuild --vector` 會失敗。`internal/config` 不依賴 `internal/context`，所以
  `internal/context` 可以匯入 `internal/config`。
- 既有測試以 `NewVectorStore(dir, "test-v1", testEmbedding)` 注入假 embedding
  （`internal/context/vector_test.go:61`）。

---

## 4. WP-1：Prompt cache token 觀測

### 4.1 `ExecutionUsage`

```go
type ExecutionUsage struct {
    // InputTokens are prompt tokens not served from the provider cache
    // (fantasy subtracts cached tokens for openai-compatible providers).
    InputTokens    int `json:"input_tokens"`
    OutputTokens   int `json:"output_tokens"`
    TotalTokens    int `json:"total_tokens"`
    ProgressTokens int `json:"progress_tokens,omitempty"`
    CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
    CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

// PromptTokens is uncached input plus cache reads plus cache writes.
func (u ExecutionUsage) PromptTokens() int
```

- `usageFromSteps` 加總兩個 cache 欄位；`TotalTokens`、`ProgressTokens` 算法不變。
- 不升 `executionEventSchemaVersion`（additive omitempty 欄位，reader 不依版本拒絕）。
- `llmLogStreamFinish`：任一 cache 值非零時，在 `tokens_out=%d` 之後、`est_tokens_in` 與結尾
  ` ===` 之前插入 ` cache_read=%d cache_write=%d`。兩者皆為零時輸出逐字不變。

### 4.2 `hufu improve`

- loader 新增欄位 `cache_read_tokens`、`cache_creation_tokens`（INSERT、
  `executionEventColumnCount` 26 → 28、TEMP schema 預設 0）。
- `improve.Metrics` 新增：

  ```go
  PromptCacheReadTokens     int     `json:"prompt_cache_read_tokens"`
  PromptCacheCreationTokens int     `json:"prompt_cache_creation_tokens"`
  // read / (input + read + creation) over the TotalTokens event set; 0 when
  // the denominator is 0.
  PromptCacheHitRate float64 `json:"prompt_cache_hit_rate"`
  ```

  事件集合必須與 `TotalTokens` 相同（`task_id <> '' AND team <> ''`）。`TrendPoint` 內嵌 `Metrics`，每個 trend
  point 用同一個 helper 計算。
- `MemoryTokenOverhead` 分母改為 `input + cache_read + cache_creation`，三個欄位各自保留
  `> 0` 防護；事件集合維持 `team <> ''`，**不要**與 `TotalTokens` 的集合統一。WP-1 之前的
  事件沒有 cache 欄位（視為 0），所以對舊事件重算的結果不變；跨越 WP-1 邊界比較 overhead
  時，新值會因分母正確而下降，這是修正而不是退步（寫進 reporter 的欄位說明與 WP-8 文件）。
- `reporter.go` 的 Metrics 表格在 `Total tokens` 列之後新增三列：
  `| Prompt cache read tokens | %d |`、`| Prompt cache created tokens | %d |`、
  `| Prompt cache hit rate | %.1f%% |`。
- 更新 `internal/improve/testdata/sqlite_analytics_semantic.golden.json`。

### 4.3 Redaction

`numericTelemetryKeys`（`internal/utils/redact.go:65`）加入 `input_tokens`、`output_tokens`、
`total_tokens`、`progress_tokens`、`cache_read_tokens`、`cache_creation_tokens`。只豁免
`json.Number`；這些 key 下的字串值仍然遮蔽。

### 4.4 測試

- `TestUsageFromStepsSumsCacheTokens`（table-driven：無 cache、只有 read、read+creation、
  TotalTokens 為 0 的估算路徑）。
- `TestExecutionUsageJSONOmitsZeroCacheFields`（零值時與 baseline 逐字相同）。
- `TestLLMLogStreamFinishCachePlacement`（零值不變；非零時位置正確；與 est 並存）。
- `TestPromptCacheHitRate`（table-driven，含分母 0 與空 task_id 事件不計入）。
- `TestMemoryTokenOverheadUsesPromptTokens`。
- `TestTrendPointCacheMetricsMatchReportMetrics`。
- `TestRedactJSONLDataPreservesUsageCounters`（數值保留；字串值仍遮蔽）。
- 既有 `sqlite_analytics_loader_test.go:153-193` 的 v3 資料列照常載入，新欄位為 0。

### 4.5 檔案

`internal/team/execution_events.go`、`internal/team/llm_log.go`、
`internal/improve/{sqlite_analytics_loader,sqlite_analytics_schema,sqlite_analytics_queries,sqlite_analytics_memory,sqlite_analytics_trend,improve,reporter}.go`、
`internal/utils/redact.go`，以及對應測試與 golden。

---

## 5. WP-2：Promotion 管線正確性與治理計數

### 5.1 Steps 驗證統一

```go
// ValidateDraft validates a draft; for skills it requires at least two list
// steps in the body (DraftSteps).
func ValidateDraft(typ Type, draft, skillName string) error
// DraftSteps returns list-item lines of a skill draft body (frontmatter
// excluded, via skill.ValidateSkillDraft → SkillDef.Content). Policies return nil.
func DraftSteps(typ Type, draft string) []string
```

- list item 的規則：`^\s*(?:[-*+]|\d+[.)])\s+\S`。
- 五個呼叫點全部改用新簽名；刪除 `policySteps`、`skillDraftStepsForHandoff`、
  `skillDraftSteps`。`cmd/hufu/improve_handoff.go` 與 `internal/improve/experiment.go` 只刪除
  helper 並改呼叫，不做其他修改。
- 模型 JSON 的 `steps` 欄位仍可接受（strict decode 不報錯），但不再作為依據。analyze prompt
  改成要求「draft body 至少兩個 list item 步驟」。

### 5.2 `generated_draft_hash`

- Migration 10（`migrations` 陣列新增一行，不改既有項目）：

  ```sql
  ALTER TABLE promotion_proposals ADD COLUMN generated_draft_hash TEXT NOT NULL DEFAULT '';
  ```

- `PromotionProposal.GeneratedDraftHash string`：`CreatePromotion` 寫入時等於 `DraftHash`；
  `UpdatePromotionDraft` 不改它。`scanPromotion` 與 `promotion.go:204,216,338` 的 SELECT
  都要處理此欄位；read-only 開啟、schema < 10 時以 `''` 代替（§6.5 規則）。

  ```go
  // DraftEdited reports whether the reviewed draft differs from the generated
  // one; known is false for proposals created before migration 10.
  func (p PromotionProposal) DraftEdited() (edited, known bool)
  ```

- `hufu context promotion list/show --json` 每筆新增 `"draft_edited": true|false|null`。
  text 輸出在每列尾端新增 tab 欄位 `draft_edited=<true|false|unknown>`。

### 5.3 治理計數

`LearningView` 新增：

```go
RejectedPromotions           *int64 `json:"rejected_promotions"`
StalePromotions              *int64 `json:"stale_promotions"`
AppliedEditedPromotions      *int64 `json:"applied_edited_promotions"`
AppliedEditUnknownPromotions *int64 `json:"applied_edit_unknown_promotions"`
```

- `internal/inspect/learning.go` 由同一次 `ListPromotions` 計算；查詢失敗時與既有欄位一樣
  各自設為 nil（`:89-98`）；同步更新 `setZeroLearningCounters`、`clearLearningCounters`。
- `internal/operator/snapshot.go`：`NormalizeSnapshot` 複製新計數（`:26-37`）、負值檢查加入
  新欄位（`:159-172`）。snapshot hash 會因新欄位改變，屬預期；**不升**
  `operator.SchemaVersion`（additive），更新 operator contract fixtures。
- `hufu context learning` text 的 Promotion 行改成
  `Promotion: eligible=unknown proposed=… approved-not-applied=… applied=… (edited=… edit-unknown=…) rejected=… stale=…`。

### 5.4 Analyze diagnostics 的 text 輸出

`hufu context promotion analyze`（含 `--dry-run`）的 text 輸出在既有內容之後，逐行輸出
`diagnostic\t<source-id>\t<reason>`，依 (source-id, reason) 排序。零 eligible 時第一行仍是
`No suitable LTM entries found for promotion.`（memory-promotion.md 不變量 3）。JSON 維持
既有 `diagnostics` 欄位。

### 5.5 測試

- `TestDraftStepsIgnoresFrontmatter`（table-driven：`-`、`*`、`+`、`1.`、`3)`、只有 heading、
  frontmatter YAML list）。
- `TestAnalyzeRejectsDraftThatApplyWouldReject`。
- `TestValidateDraftCallersAgree`：analyze、edit、apply、improve handoff、experiment 對同一份
  draft 結果一致。
- `TestMigration10LeavesEditStateUnknown`、`TestEditMarksProposalEdited`、
  `TestReanalyzeKeepsGeneratedDraftHash`。
- `TestInspectLearningCountsRejectedStaleAndEdited`、`TestNormalizeSnapshotClonesNewCounters`。
- `TestAnalyzeTextPrintsSortedDiagnostics`（含零 eligible 情境）。
- 更新 `internal/context/migration_next_fixture_test.go:40-44` 與
  `semantic_projection_test.go:359` 寫死的 schema version。

---

## 6. WP-3：記憶對判定資料模型

### 6.1 型別（新檔 `internal/context/conflict.go`）

```go
// CurrentConflictJudgePolicyVersion must be bumped whenever the judge prompt
// or decision rules change.
const CurrentConflictJudgePolicyVersion = "conflict-judge-v1"

type PairVerdict string

const (
    PairVerdictContradicts  PairVerdict = "contradicts"
    PairVerdictCompatible   PairVerdict = "compatible"
    PairVerdictDuplicate    PairVerdict = "duplicate"
    PairVerdictRefines      PairVerdict = "refines"
    PairVerdictUndetermined PairVerdict = "undetermined" // judge output was invalid
)

// PairJudgmentStatus is stored review state. Only contradictions are open or
// dismissed; every other verdict is not_applicable.
type PairJudgmentStatus string

const (
    PairJudgmentOpen          PairJudgmentStatus = "open"
    PairJudgmentDismissed     PairJudgmentStatus = "dismissed"
    PairJudgmentNotApplicable PairJudgmentStatus = "not_applicable"
)

// ConflictState is derived at read time and never stored.
type ConflictState string

const (
    ConflictStateOpen                ConflictState = "open"
    ConflictStateDismissed           ConflictState = "dismissed"
    ConflictStateResolvedBySupersede ConflictState = "resolved_by_supersede"
    ConflictStateInactive            ConflictState = "inactive"
)

type PairJudgment struct {
    ID                 string
    ProjectID          string
    TeamID             string // "" when the items have no team
    AgentID            string // "" for shared items
    ItemAID            string // bytewise smaller ID
    ItemBID            string
    ItemAContentHash   string
    ItemBContentHash   string
    Verdict            PairVerdict
    Status             PairJudgmentStatus
    JudgePolicyVersion string
    JudgeModel         string
    Rationale          string // redacted, truncated to 512 runes
    DismissReason      string
    CreatedAt          time.Time
    UpdatedAt          time.Time
}

type ConflictView struct {
    PairJudgment
    State  ConflictState
    ItemAKind, ItemBKind ContextKind // "" when the item no longer exists
}
```

- ID：`"conflict-" + hex(sha256(project \x00 team \x00 agent \x00 itemA \x00 hashA \x00 itemB \x00 hashB \x00 judgePolicyVersion))[:12 bytes]`。
- `State` 推導（只對 `Verdict=contradicts`）：
  1. `Status=dismissed` → `dismissed`。
  2. `JudgePolicyVersion != CurrentConflictJudgePolicyVersion` → `inactive`。
  3. 兩筆都存在、`Lifecycle=confirmed`、`SupersededBy` 為空、`ExpiresAt` 未到、`ContentHash`
     等於判定時的值 → `open`。
  4. 任一筆 `SupersededBy` 非空 → `resolved_by_supersede`。
  5. 其他（刪除、過期、hash 改變）→ `inactive`。
- 表中空 team/agent 存 `''`，`context_items` 存 NULL；join 時以 `COALESCE(team_id,'')` 比較。

### 6.2 Migration 11

```sql
CREATE TABLE context_pair_judgments (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  team_id TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '',
  item_a_id TEXT NOT NULL,
  item_b_id TEXT NOT NULL,
  item_a_content_hash TEXT NOT NULL,
  item_b_content_hash TEXT NOT NULL,
  verdict TEXT NOT NULL CHECK (verdict IN ('contradicts','compatible','duplicate','refines','undetermined')),
  status TEXT NOT NULL CHECK (status IN ('open','dismissed','not_applicable')),
  judge_policy_version TEXT NOT NULL,
  judge_model TEXT NOT NULL,
  rationale TEXT NOT NULL DEFAULT '',
  dismiss_reason TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK (item_a_id < item_b_id),
  CHECK ((verdict = 'contradicts') = (status IN ('open','dismissed')))
);
CREATE INDEX idx_pair_judgments_scope ON context_pair_judgments(project_id, team_id, status);
CREATE INDEX idx_pair_judgments_item_a ON context_pair_judgments(item_a_id);
CREATE INDEX idx_pair_judgments_item_b ON context_pair_judgments(item_b_id);
```

不加 foreign key（`DeleteExpired` 會刪除過期 item，由推導規則 5 處理）。若 WP-2 與 WP-3 實作
順序對調，使用下一個可用版本號；已發布 migration 永不修改。

### 6.3 Repository 方法（`*SQLiteRepository`，`conflict.go`）

```go
// LookupPairJudgment returns the row for id, if any.
LookupPairJudgment(ctx context.Context, id string) (PairJudgment, bool, error)

// SavePairJudgment inserts j when its ID is new and returns (row, created).
//   - If a row exists with verdict undetermined and replaceUndetermined is
//     true, it is updated in place; otherwise the existing row is returned
//     unchanged (a dismissed conflict stays dismissed).
//   - A new contradicts row inherits status dismissed and dismiss_reason when
//     any dismissed row exists for the same item IDs and content hashes under
//     any judge version; no detected event is written in that case.
//   - Otherwise a new or replaced contradicts row gets status open and a
//     memory_conflict_detected event in the same transaction.
//   - Rationale is redacted and truncated to 512 runes; it never errors on length.
SavePairJudgment(ctx context.Context, j PairJudgment, actor string, replaceUndetermined bool) (PairJudgment, bool, error)

ListConflicts(ctx context.Context, q ConflictQuery) ([]ConflictView, error)
GetConflict(ctx context.Context, id, projectID, teamID string) (ConflictView, error)

// DismissConflict: derived state open → dismissed with a memory_conflict_dismissed
// event; already dismissed → returns the view unchanged, no event (idempotent);
// any other state → error. reason must be non-empty and satisfy
// utils.RedactSecrets(reason) == reason, otherwise error.
DismissConflict(ctx context.Context, id, projectID, teamID, reason, actor string) (ConflictView, bool, error)

// OpenConflictsForItems maps item ID → sorted IDs of conflicts in derived
// state open. itemIDs are queried in chunks of at most 500.
OpenConflictsForItems(ctx context.Context, projectID, teamID string, itemIDs []string) (map[string][]string, error)

type ConflictQuery struct {
    ProjectID, TeamID string
    IncludeInactive   bool // false: derived state open only; true: every contradicts row
}
```

- 新增 interface，供 `internal/team` 以 type assertion 使用（比照 `ExperienceRepository`）：

  ```go
  type ConflictLookup interface {
      OpenConflictsForItems(ctx context.Context, projectID, teamID string, itemIDs []string) (map[string][]string, error)
  }
  ```

- 事件由 repository 在同一 transaction 內以 `insertPromotionOutbox` 寫入，content-free：
  - `memory_conflict_detected`：`{schema_version:1, conflict_id, item_ids:[a,b], content_hashes:[ha,hb], judge_policy_version, actor}`，idempotency key 為 `<id>:detected`。
  - `memory_conflict_dismissed`：`{schema_version:1, conflict_id, item_ids:[a,b], actor}`，
    idempotency key 為 `<id>:dismissed`。
  - payload 不含 judge model、rationale、dismiss reason（memory-learning.md 不變量 10）。

### 6.4 比較範圍

只比較 `ProjectID`、`TeamID`、`AgentID` 完全相同的兩筆；persistent 記憶的
Session/Branch/Task/Attempt 必須為空。

### 6.5 Read-only 相容

新增 `appliedSchemaVersion(ctx) (int, error)`（讀 `schema_migrations`，每個 repository 實例
快取一次）。read-only 開啟時：

- schema < 10：promotion SELECT 以 `'' AS generated_draft_hash` 代替。
- schema < 11：`ListConflicts`/`GetConflict` 回傳 sentinel `ErrConflictsUnavailable`；
  `OpenConflictsForItems` 回傳空 map、nil error。
- 可寫入開啟一律先 migrate，不需要這些分支。

`ReadOnlyRepository` 加入 `ListConflicts`、`GetConflict`、`OpenConflictsForItems`、
`ListAppliedSkillPromotions`（WP-7）。

### 6.6 測試

- `TestMigration11CreatesPairJudgments`（從 baseline 資料庫升級、重開仍存在）。
- `TestSchemaCheckRejectsInvalidVerdictStatus`（table-driven，含 a/b 順序）。
- `TestPairJudgmentIDIsDeterministic`（交換 a/b、hash 改變、版本改變）。
- `TestSavePairJudgmentKeepsExistingRow`、`TestSavePairJudgmentReplacesUndeterminedOnlyWhenAsked`。
- `TestNewContradictionInheritsDismissalAcrossJudgeVersions`（不寫 detected event）。
- `TestSavePairJudgmentTruncatesRationale`。
- `TestConflictStateDerivation`（table-driven 覆蓋 §6.1 五條規則）。
- `TestDismissConflictStateMachine`（空 reason、含 secret、已 dismissed 冪等、superseded、inactive）。
- `TestOpenConflictsForItemsChunksLargeInput`（> 500 IDs）。
- `TestReadOnlyRepositoryBeforeMigration10And11`。

---

## 7. WP-4：`hufu context conflicts` 命令

### 7.1 純邏輯（新套件 `internal/memoryconflict`）

**`eligible.go`**

```go
var KnowledgeKinds = []contextstore.ContextKind{
    contextstore.ContextDecision, contextstore.ContextConvention,
    contextstore.ContextArchitecture, contextstore.ContextPattern,
    contextstore.ContextInstruction, contextstore.ContextRequirement,
}
func Eligible(item contextstore.ContextItem, projectID, teamID string, now time.Time) (ok bool, reason string)
```

- 把 `internal/promotion/eligibility.go` 中與 scope 值和 secret 無關的條件（`:35` 的 lifecycle
  與 superseded、`:38-46` 的窄 scope、`ExpiresAt`、persistent metadata）抽成
  `contextstore.IsCurrentPersistentKnowledge(item ContextItem, now time.Time) bool`，promotion
  與本套件共用。promotion 行為必須完全不變，**不**加入 `ValidUntil` 檢查。
- `Eligible` 另外檢查 project/team 相符、kind 在 `KnowledgeKinds`，以及 secret 檢查（與 promotion
  相同條件，不通過時 reason 為 `secret_like_content`）。

**`pairs.go`**

```go
// SimilarSearcher is satisfied by *contextstore.VectorStore (§7.4).
type SimilarSearcher interface {
    SearchSimilarTo(ctx context.Context, itemID string, req contextstore.SearchRequest) ([]contextstore.SearchResult, error)
}
type PairOptions struct {
    TopK                int     // default 5 (lexical)
    MinSharedTokens     int     // default 2
    MinOverlap          float64 // default 0.2; |A∩B| / min(|A|,|B|)
    MaxItemsPerScope    int     // default 2000; exceeding returns an error
    MaxFilePathFanout   int     // default 20
    Vector              SimilarSearcher // nil: vector source disabled
    VectorTopK          int     // default 5
    MinVectorSimilarity float64 // default 0.45 (chromem cosine similarity; see §7.2 calibration)
}
type Pair struct{ A, B contextstore.ContextItem } // A.ID < B.ID
type CandidateSet struct {
    Pairs         []Pair
    Diagnostics   []Diagnostic
    VectorUsed    bool
    LexicalPairs  int // pairs contributed by each source before the union
    FilePathPairs int
    VectorPairs   int
}
func Tokens(content string) map[string]struct{}
func CandidatePairs(ctx context.Context, items []contextstore.ContextItem, opts PairOptions) (CandidateSet, error)
```

- `Tokens`：
  - 轉小寫；
  - ASCII `[a-z0-9_]+`，長度 ≥ 3，排除固定 stopword 清單（the、and、for、with、this、that、
    from、into、when、then、than、use、used、using、must、should、not、are、was、were、has、
    have、all、any、can、will）；
  - 漢字、平假名、片假名、韓文的連續字元以相鄰 rune bigram 產生 token（長度 1 時取單字）；
  - 其他非 ASCII 字母連續字元，長度 ≥ 3 時整段當一個 token。
- `CandidatePairs`：
  1. 依 (ProjectID, TeamID, AgentID) 分組；任一組超過 `MaxItemsPerScope` 時回傳 error，
     訊息指示使用 `--max-items`。
  2. 組內對每筆 X（依 ID），其他記憶的分數為 overlap coefficient；共享 token 至少
     `MinSharedTokens` 且分數至少 `MinOverlap` 者，依 (分數 desc, ID asc) 取前 `TopK`。
  3. 同組內共享至少一個 `EvidenceRef{Type:"file_path"}` 的記憶成對；某個 path 被超過
     `MaxFilePathFanout` 筆共享時忽略該 path，並產生 diagnostic `file_path_too_common`。
  4. `opts.Vector` 非 nil 時，對組內每筆 X（依 ID）呼叫
     `SearchSimilarTo(ctx, X.ID, SearchRequest{Scope: {ProjectID, TeamID, AgentID: X.Scope.AgentID}, Visibility: VisibilityExact, Limit: 4 * VectorTopK})`。
     - 只保留 eligible 集合內、同組、`Score >= MinVectorSimilarity` 的結果，依
       (Score desc, ID asc) 排序後取前 `VectorTopK`。
     - 任何一次呼叫回傳 error：丟棄本次掃描**所有**向量候選，`VectorUsed=false`，加一筆
       diagnostic `vector_unavailable`，繼續用前兩個來源。不使用部分向量結果，避免候選集合
       因失敗位置而改變。
     - 全部成功時 `VectorUsed=true`。
  5. 三個來源取聯集，正規化為 A.ID < B.ID、去重、排除 `ContentHash` 相同者，依
     (A.ID, B.ID) 排序。`LexicalPairs`、`FilePathPairs`、`VectorPairs` 是各來源在聯集前的
     正規化對數。
- 決定性：來源 1、2 完全 deterministic；來源 3 取決於 embedding 模型輸出。已判定的對以
  pair ID 快取、不會重新呼叫 judge，所以向量結果的差異只影響「哪些新的對被提出」。

**`judge.go`**

```go
var ErrInvalidJudgment = errors.New("invalid conflict judgment")
type TextGenerator interface{ GenerateText(ctx context.Context, prompt string) (string, error) }
type Judgment struct {
    Verdict   contextstore.PairVerdict `json:"verdict"`
    Rationale string                   `json:"rationale"`
}
type Judge interface {
    Prompt(a, b contextstore.ContextItem) string
    Judge(ctx context.Context, a, b contextstore.ContextItem) (Judgment, error)
}
type JSONJudge struct{ Generator TextGenerator }
```

- prompt 提供兩筆的 ID、kind、content，問「若兩筆都被遵守，是否對同一件事給出不相容的指示」；
  只能回傳一個 JSON object，欄位只有 `verdict`（contradicts/compatible/duplicate/refines）與
  `rationale`（要求 ≤ 300 字元；超過時由 repository 截斷，不視為錯誤）。
- 解碼規則同 `promotion.JSONDraftGenerator`（`internal/promotion/generator.go:33-58`）：拒絕
  fence、unknown field、trailing JSON。這些錯誤與未知 verdict 都 wrap `ErrInvalidJudgment`；
  `GenerateText` 本身的錯誤原樣回傳。
- 修改 prompt 或規則時必須遞增 `contextstore.CurrentConflictJudgePolicyVersion`。

**`scan.go`**

```go
type ScanRepository interface {
    Iterate(context.Context, contextstore.RepositoryQuery, func(contextstore.ContextItem) error) error
    LookupPairJudgment(context.Context, string) (contextstore.PairJudgment, bool, error)
    SavePairJudgment(context.Context, contextstore.PairJudgment, string, bool) (contextstore.PairJudgment, bool, error)
}
type ScanOptions struct {
    ProjectID, TeamID  string
    MaxPairs           int  // default 20: judge calls per scan
    MaxPromptRunes     int  // CLI passes sidecar.ClassifierProfile.InputRuneLimit()
    DryRun             bool
    RetryUndetermined  bool
    JudgeModel, Actor  string
}
type Diagnostic struct {
    ItemID string `json:"item_id,omitempty"`
    PairID string `json:"pair_id,omitempty"`
    Reason string `json:"reason"` // secret_like_content | file_path_too_common | vector_unavailable | input_too_large | invalid_judgment
}
type ScanReport struct {
    EligibleItems  int          `json:"eligible_items"`
    CandidatePairs int          `json:"candidate_pairs"`
    LexicalPairs   int          `json:"lexical_pairs"`
    FilePathPairs  int          `json:"file_path_pairs"`
    VectorPairs    int          `json:"vector_pairs"`
    VectorUsed     bool         `json:"vector_used"`
    AlreadyJudged  int          `json:"already_judged"`
    Judged         int          `json:"judged"`
    Contradictions int          `json:"contradictions"`
    Undetermined   int          `json:"undetermined"`
    Skipped        int          `json:"skipped"`
    Deferred       int          `json:"deferred"`
    DryRun         bool         `json:"dry_run"`
    Diagnostics    []Diagnostic `json:"diagnostics"`
}
func Scan(ctx context.Context, repo ScanRepository, judge Judge, opts ScanOptions, pairOpts PairOptions) (ScanReport, error)
```

1. 用 `Iterate`（`Scope{ProjectID, TeamID}`，`VisibilitySubtree`）把 `Eligible` 的記憶收集到
   記憶體。visitor 內不得呼叫 repository。
2. `CandidatePairs(ctx, items, pairOpts)` 產生候選對，並把來源計數、`VectorUsed` 與
   diagnostics 複製到 `ScanReport`。
3. 依序處理每一對：
   - 計算 ID。已存在且不是（undetermined 且 `RetryUndetermined`）→ `AlreadyJudged`，不呼叫模型。
   - `utf8.RuneCountInString(judge.Prompt(a,b)) > MaxPromptRunes` → `Skipped` +
     `input_too_large`，不落庫。
   - 已達 `MaxPairs` → `Deferred`。
   - `DryRun` → 計入 `Judged`（表示「會判定」），不呼叫模型、不寫入。
   - 呼叫 judge。成功 → `SavePairJudgment`。`ErrInvalidJudgment` → 以 verdict
     `undetermined` 落庫 + `invalid_judgment` diagnostic；連續 3 對 undetermined 時停止並回傳
     error。其他錯誤（transport、timeout）→ 立即停止並回傳 error，該對不落庫；已 commit 的
     保留。
4. 每筆判定各自 commit。

### 7.2 CLI（新檔 `cmd/hufu/context_conflicts_cmd.go`）

```text
hufu context conflicts scan    --workspace <ws> --project <id> --team <name>
                               [--team-search-path <csv>] [--model <model>]
                               [--top-k 5] [--max-pairs 20] [--max-items 2000]
                               [--vector] [--vector-top-k 5] [--min-vector-similarity 0.45]
                               [--retry-undetermined] [--dry-run] [--json]
hufu context conflicts list    --workspace <ws> --project <id> --team <name> [--all] [--json]
hufu context conflicts show    <conflict-id> --workspace <ws> --project <id> --team <name>
                               [--show-content] [--json]
hufu context conflicts dismiss <conflict-id> --workspace <ws> --project <id> --team <name>
                               --reason <text> [--json]
```

- 本命令使用自己的 flag 變數；`--project` 與 `--team` 必填。
- 共用 helper（從 `context_promotion_cmd.go` 抽到新檔 `cmd/hufu/maintenance_sidecar.go`）：

  ```go
  type maintenanceTextGenerator interface {
      GenerateText(ctx context.Context, prompt string) (string, error)
  }
  type maintenanceGeneratorOptions struct {
      TeamDir, Workspace, ModelOverride, Purpose string
      Profile                                   sidecar.Profile
  }
  // Returns the generator, the resolved model name, and a release func.
  func newMaintenanceTextGenerator(ctx context.Context, opts maintenanceGeneratorOptions) (maintenanceTextGenerator, string, func(), error)
  func teamRegistryFromSearchPath(csv string) *team.TeamRegistry
  func flushGovernanceEvents(ctx context.Context, repo *contextstore.SQLiteRepository, workspace string) error
  ```

  - `newPromotionGenerator(ctx, teamDir)` 保留原簽名，內部呼叫 helper（`promotionWorkspacePath()`、
    `promotionModel`、`"promotion_draft"`、`CompactorProfile`），`promotionGeneratorFactory`
    與既有測試不變。
  - `promotionRegistry()` 改成呼叫 `teamRegistryFromSearchPath(promotionSearchPath)`；
    `flushPromotionEvents(ctx, repo)` 改成呼叫 `flushGovernanceEvents(ctx, repo, promotionWorkspacePath())`。
  - 模型解析順序不變：`ModelOverride`、team sidecar、team generation、config sidecar、config model。
- scan 以 `newMaintenanceTextGenerator(..., Purpose: "memory_conflict_judge", Profile: sidecar.ClassifierProfile)`
  建立 judge，解析出的模型寫入 `JudgeModel`。在 `internal/team/context_purpose.go` 登錄
  `"memory_conflict_judge": {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "judgment_unavailable"}`。
  `--dry-run` 不建立 sidecar，也不需要 judge 模型設定。
- `--vector`（選用，預設關閉）：
  - 以 `contextstore.OpenOllamaVectorStore(workspace, config.ResolveEmbeddingModel(""), config.DefaultOllamaAPIURL)`
    開啟（與 `hufu context query` 相同的 model 與 URL 解析），再
    `Rebuild(ctx, repo, Scope{ProjectID, TeamID})`。
  - Open 或 Rebuild 回傳任何 error（包括 `errors.Join` 的部分失敗）：本次不使用向量，
    `ScanReport.VectorUsed=false` 並加 diagnostic `vector_unavailable`，text 輸出註明
    `vector=unavailable`，掃描照常以其他兩個來源繼續，exit code 不受影響。
  - 副作用（與 `hufu context query` 相同，屬預期）：重建可丟棄的 `<workspace>/context-vectors`，
    並更新 `context_items.embedding_state`。
  - 成本：每次掃描對 `Scope{ProjectID, TeamID}` 內每筆 item 做一次 embedding；
    `SearchSimilarTo` 使用已存向量，不再額外呼叫。
  - `--dry-run --vector` 會呼叫 embedding（本機 Ollama），但不呼叫 judge、不寫判定。
  - 測試注入點：
    `var conflictVectorFactory = func(ctx context.Context, workspace string, repo contextstore.Repository, scope contextstore.Scope) (memoryconflict.SimilarSearcher, error)`，
    預設實作即上述 Open + Rebuild。
  - 沒有 `--vector` 時，`--vector-top-k` 與 `--min-vector-similarity` 不生效。
  - 門檻校準（2026-09-23，本機 `nomic-embed-text`）：無關記憶約 0.37–0.38；「Use SQLite for
    storage」對「Architecture migrated to PostgreSQL」為 0.487；真正矛盾的「tabs」對「four
    spaces」為 0.767、中文正反陳述為 0.978。原訂 0.5 會漏掉代表案例，因此預設改為 0.45。
- 寫入類子命令（scan、dismiss）開頭先 `flushGovernanceEvents`，結束前再 flush 一次；
  list/show 以 read-only 開啟，不 flush（與 promotion 一致）。
- `list` 預設只列 derived state open；`--all` 列出所有 contradicts 判定。沒有 open 衝突時 text
  輸出 `No open memory conflicts.`，exit 0。read-only 且 schema < 11 時輸出
  `Memory conflicts are unavailable until the context store is upgraded.`，exit 0。
- `show` 預設 content-free。`--show-content` 另外顯示兩筆內容（經 `utils.RedactSecrets`）、
  rationale、dismiss reason。state 為 open 時印出兩種解決方式：
  `hufu context supersede <id> --with <other-id> --project … --team …`、
  `hufu context conflicts dismiss <conflict-id> --reason …`。
- `dismiss` 已 dismissed 時輸出 `already dismissed`，exit 0。
- JSON：
  - scan：`{"schema_version":1,"scan":ScanReport}`
  - list：`{"schema_version":1,"conflicts":[conflictJSON]}`
  - show、dismiss：`{"schema_version":1,"conflict":conflictJSON}`
  - `conflictJSON`：`id`、`state`、`item_ids`、`kinds`、`content_hashes`、
    `judge_policy_version`、`judge_model`、`created_at`、`updated_at`；加 `--show-content`
    時再多 `contents`、`rationale`、`dismiss_reason`。
- text：scan 輸出一行計數摘要（含 `lexical=… file_path=… vector=<n|off|unavailable>`），之後逐行
  `diagnostic\t<item-or-pair-id>\t<reason>`（排序；scan 層級的 `vector_unavailable` 以 `-` 作為 ID）。
  list 每列為 `<id>\t<state>\t<item-a>\t<item-b>`。
- `cmd/hufu/completion.go` 依 promotion 寫法新增四個 extern；`cmd/hufu/cli_metadata.go` 的
  `canonicalExamples` 加一個 `hufu context conflicts list` 範例，並重新產生命令參考（§11）。

### 7.3 測試

- `TestIsCurrentPersistentKnowledgeKeepsPromotionBehavior`：同一組 fixture 抽取前後 promotion
  結果相同。
- `TestTokens`（table-driven：英文 stopword、底線、數字、純中文 bigram、中英混合、日文、單一
  漢字）。
- `TestCandidatePairs`（table-driven：deterministic、scope 不同不配對、相同 hash 排除、
  file_path 成對、fanout 超過上限、超過 MaxItemsPerScope 回傳 error、TopK 截斷與 tie-break）。
- `TestJSONJudgeStrictDecode`（fence、unknown field、trailing JSON、未知 verdict → `ErrInvalidJudgment`）。
- `TestScanSkipsAlreadyJudgedWithoutModelCall`（fake generator 計數）。
- `TestScanSkipsOversizedPrompt`、`TestScanPersistsUndeterminedAndStopsAfterThree`、
  `TestScanRetryUndetermined`、`TestScanStopsOnTransportErrorKeepsCommitted`、
  `TestScanRespectsMaxPairs`、`TestScanDryRunWritesNothing`。
- `TestConflictsCLIListShowDismiss`（`t.TempDir()` 的真實 SQLite；dismiss 後 list 空、`--all`
  仍顯示、outbox 有 dismissed 事件、重複 dismiss 冪等）。
- `TestConflictsShowDefaultIsContentFree`、`TestConflictsListOnUnmigratedReadOnlyStore`。
- `TestNewPromotionGeneratorUsesMaintenanceHelper`（既有 `context_promotion_cmd_test.go:163-198`
  的 factory 測試維持通過）。
- 向量來源（fake `SimilarSearcher`，不需要 Ollama）：
  - `TestCandidatePairsVectorFindsVocabularyMismatch`：「Use SQLite for storage」與
    「Architecture migrated to PostgreSQL」在 lexical 下不成對，加上 fake 向量後成對；
  - `TestCandidatePairsVectorFiltersEligibilityScopeAndThreshold`（非 eligible、不同 scope、
    低於門檻、自己本身都被排除）；
  - `TestCandidatePairsVectorTieBreak`（同分依 ID）；
  - `TestCandidatePairsDropsAllVectorResultsOnError`（第 N 次呼叫失敗 → 向量候選全部丟棄、
    `vector_unavailable`、lexical 結果不變）；
  - `TestCandidatePairsUnionCountsPerSource`。
- `TestConflictsScanVectorUnavailableFallsBack`（`conflictVectorFactory` 回傳 error → exit 0、
  `vector_used=false`、diagnostic 存在）。
- 依本 repo 慣例不 mock LLM：純函式餵 canned string，CLI 測試注入 fake `TextGenerator`。

### 7.4 向量庫前置修正（WP-4 的第一個 PR）

**Embedding model 名稱**

```go
// OllamaEmbeddingModelName strips a leading "ollama/" provider prefix so the
// name is valid for the Ollama embeddings API. Other values are unchanged.
func OllamaEmbeddingModelName(model string) string // internal/config
```

- `OpenOllamaVectorStore` 改成
  `NewVectorStore(dir, model, chromem.NewEmbeddingFuncOllama(config.OllamaEmbeddingModelName(model), url))`。
  `NewVectorStore` 收到的 `model`（寫入 `<dir>/model` 與 collection metadata 的識別）維持原字串，
  既有 index 不會因此被重建。
- 影響：修正後，預設設定下 `hufu context query` 的向量路徑與 `hufu context rebuild --vector`
  開始真正使用 embedding（之前前者靜默退回 lexical、後者失敗）。這是修正既有 bug，寫入
  WP-8 的文件與 PR 說明。
- 測試：
  - `TestOllamaEmbeddingModelName`（table-driven：`ollama/nomic-embed-text:latest`、
    `nomic-embed-text:latest`、`ollama/`、空字串、其他前綴 `openai/x` 不變）；
  - `TestOpenOllamaVectorStoreSendsUnprefixedModel`：`httptest.Server` 當 Ollama，斷言
    `/embeddings` 請求 body 的 `model` 為 `nomic-embed-text:latest`，並且 `<dir>/model` 仍記錄
    原字串。

**`VectorStore.SearchSimilarTo`**（`internal/context/vector.go`，191 行，可直接擴充）

```go
// ErrVectorItemNotIndexed reports that itemID has no stored embedding.
var ErrVectorItemNotIndexed = errors.New("context item is not in the vector index")

// SearchSimilarTo returns items similar to an already indexed item using its
// stored embedding (no embedding call). The source item is excluded. Results
// are hydrated and authorized exactly like SearchVector.
func (s *VectorStore) SearchSimilarTo(ctx context.Context, itemID string, req SearchRequest) ([]SearchResult, error)
```

- 以 `collection.GetByID(ctx, itemID)` 取得 `Document.Embedding`，找不到時回傳
  `ErrVectorItemNotIndexed`；再以 `collection.QueryEmbedding(ctx, embedding, n, nil, nil)` 查詢，
  `n = min(req.Limit + 1, collection.Count())`（`req.Limit <= 0` 時視為 20）。
- 從 `SearchVector` 抽出共用的 `hydrateVectorResults(ctx, results, req)`（`repo.Get` +
  `isRetrievable`，被刪除的 item 略過）。`SearchVector` 的行為與測試不變。
- 未 `Rebuild` 時回傳與 `SearchVector` 相同的 error；collection 為空時回傳 nil, nil。
- 測試（沿用 `vector_test.go` 的 `testEmbedding`）：
  - `TestSearchSimilarToUsesStoredEmbeddingWithoutEmbedCall`（計數 embed 呼叫：Rebuild 之後為 0）；
  - `TestSearchSimilarToExcludesSourceAndAppliesScope`；
  - `TestSearchSimilarToUnknownItem`；
  - `TestSearchVectorUnchangedAfterRefactor`。

---

## 8. WP-5：衝突效果

### 8.1 Runtime 標記（只影響 manifest 歸因）

- `CanonicalContextBundle` 新增 `SharedPersistentConflicts map[string][]string`。
- 新檔 `internal/team/memory_conflict_runtime.go`：

  ```go
  // openConflictsForItems is best effort: a repository without ConflictLookup
  // or a query error returns nil. Errors are logged and emitted as
  // observability_degraded {component: "memory_conflict"}; they never fail
  // the model call.
  func (c *Coordinator) openConflictsForItems(ctx context.Context, items []contextstore.ContextItem) map[string][]string
  ```

  以 `c.contextRepo.(contextstore.ConflictLookup)` 取得，使用 `c.contextScope()` 的
  ProjectID/TeamID。在 `context_shadow.go:138` 與 `context_router.go:359` 兩處建構 bundle 時
  呼叫；ranking 降級（aggregates 為 nil）時仍照常載入。
- compiler `ContextItem` 新增 `ConflictIDs []string`；`canonicalCompilerItemsScored` 多一個
  `conflicts map[string][]string` 參數，更新 `:736`、`:912` 與 `knowledge_state_test.go:117`。
- `classifyKnowledgeState(authority, aggregate, now, policy, conflicting bool)`：historical 且
  conflicting 時回傳 `KnowledgeConflicting`，優先於其他狀態；normative 與 example 不受影響。
  更新 `context_manifest.go:99`（傳 `len(item.ConflictIDs) > 0`）與
  `knowledge_state_test.go:45,49`。
- 即使衝突的另一方沒有被注入，被注入的一方仍然標記 conflicting。
- `OutcomeCoverageSignal.ConflictingCount int \`json:"conflicting_count,omitempty"\``
  （omitempty：沒有衝突時 JSON 與 baseline 相同）。`IncludedItemCount` 包含 conflicting。
- 顯示：`cmd/hufu/report.go:60`、`cmd/hufu/inspectcmd.go:424-427` 在 stale 之後加
  `, %d conflicting`；`internal/inspect/evidence.go` 的結構與轉換加入
  `ConflictingCount`（同樣 omitempty）。
- **不變量**：有無衝突時，compiler 輸出的 prompt 位元組、included/omitted 集合、排序、token
  計數完全相同；差異只在 manifest 的 `knowledge_state` 與 coverage 計數。

### 8.2 Promotion gate（fail-closed）

- `promotion.EligibilityRepository` 加入 `OpenConflictsForItems`，並更新
  `internal/promotion/eligibility_iteration_test.go:13-35` 與其他 fake。`EligibleSources` 在收集
  candidates 後一次查詢，有 open 衝突者排除並加
  `Diagnostic{SourceID, Reason: "unresolved_conflict"}`；查詢失敗回傳 error。
- 新增 `func (s Service) validateNoOpenConflicts(ctx context.Context, p Proposal) error`。
- `Apply`：放在 `alreadyWritten` 分支（`apply.go:100-104`）**之後**、target hash 檢查之前。有
  open 衝突時回傳 `s.applyFailed(ctx, p, err)`，**status 維持 `approved`**。錯誤訊息為
  `promotion source <id> has an unresolved memory conflict <conflict-id>; supersede or dismiss it, then apply again`。
  查詢失敗同樣走 `applyFailed`。衝突解除後，直接重跑 `apply` 即可成功，不需要重新 analyze。
- `ValidateProposalEvidence`（improve handoff 使用）同樣呼叫，有衝突時回傳 error。
- analyze：已存在的 proposal 不受 gate 影響（`CreatePromotion` 回傳既有資料列）；gate 在
  approve 後的 apply 生效。

### 8.3 Consolidation gate（fail-closed）

- 新增 `validateConsolidationConflicts(ctx, repo, sources) error`，放在新檔
  `cmd/hufu/context_consolidation_conflicts.go`。依每個來源自己的 TeamID 分組查詢
  `OpenConflictsForItems`，有衝突時回傳 `source %q has an unresolved memory conflict (%s)`。
  在 `validateConsolidationSources` 的兩個呼叫點（`context_consolidation_cmd.go:81`、`:291`）
  之後呼叫。`validateConsolidationSources` 維持純函式。
- dry-run：`clusters` 欄位形狀不變，新增同層 `"conflicted_ids": [...]`（排序後、出現在任一
  cluster 且有 open 衝突的 ID；同樣依各 item 的 TeamID 查詢）。text 輸出附加
  `; <n> item(s) with unresolved conflicts`。
- 保留既有 `contradicts_ids` 檢查。

### 8.4 Learning 計數

`LearningView.OpenConflicts *int64 \`json:"open_conflicts"\``，由
`ListConflicts(ConflictQuery{ProjectID, TeamID})` 計數。`ErrConflictsUnavailable` → nil，
不影響 status。其他錯誤 → nil，並在 status 仍為 available 時設 `UnavailableReason` 為
`conflict_query_failed`。同步 `NormalizeSnapshot` 與負值檢查。`hufu context learning` 新增一行
`Conflicts: open=<n|unknown>`。

### 8.5 測試

- `TestClassifyKnowledgeStateConflicting`（table-driven）。
- `TestConflictMarkingDoesNotChangePrompt`（§8.1 不變量）。
- `TestManifestMarksConflictEvenWhenCounterpartOmitted`。
- `TestOpenConflictLookupFailureDegradesWithoutFailing`。
- `TestTaskKnowledgeCoverageCountsConflicting`、`TestCoverageJSONUnchangedWithoutConflicts`。
- `TestEligibleSourcesExcludesUnresolvedConflict`、`TestEligibleSourcesFailsClosedOnConflictQueryError`。
- `TestApplyBlockedWhileConflictOpenKeepsApproved`、`TestApplySucceedsAfterDismiss`、
  `TestApplySucceedsAfterCounterpartSuperseded`、`TestAlreadyWrittenApplyIgnoresNewConflict`。
- `TestConsolidationRejectsConflictedSources`、`TestConsolidationDryRunReportsConflictedIDsPerTeam`。
- `TestInspectLearningCountsOpenConflicts`、`TestInspectLearningConflictsUnavailable`。

---

## 9. WP-6：Consolidation LLM 起草

### 9.1 行為

```text
hufu context consolidate --apply-proposal --source <id,id,...> --draft
                         --project <id> --team <name>
                         [--team-search-path <csv>] [--model <model>] [--workspace <ws>]
```

- `--draft` 必須搭配 `--apply-proposal`、`--source`、`--team`，並與 `--proposal-text` 互斥。
  沒有 `--draft` 時，人工路徑行為不變。
- 處理順序：
  1. 既有來源驗證：`validateConsolidationSources`、`validateConsolidationSupport`；
  2. WP-5 的 `validateConsolidationConflicts`；
  3. 來源內容總長 > 16000 runes 時回傳
     `consolidation sources are too large for drafting; use --proposal-text`（遠低於 24000 runes
     的 auxiliary 截斷）；
  4. 冪等檢查：`FindProposedConsolidation` 命中時印出既有 proposal，exit 0，不呼叫模型；
  5. 呼叫模型並驗證；
  6. 走與人工文字完全相同的持久化路徑。

  任一步失敗都不呼叫後續步驟、不落庫，exit 非零。
- 新增 repository 方法（`internal/context/consolidation.go`）：

  ```go
  // FindProposedConsolidation returns the earliest (created_at, id) proposal
  // with status proposed whose sorted source IDs equal sortedSourceIDs.
  FindProposedConsolidation(ctx context.Context, projectID, teamID string, sortedSourceIDs []string) (ConsolidationProposal, bool, error)
  ```

- candidate metadata：模型路徑加 `"proposal_origin": "model"`、`"draft_model": <resolved model>`；
  人工路徑加 `"proposal_origin": "operator"`。`memory_consolidation_proposed` payload 加入
  `proposal_origin`。
- sidecar：`newMaintenanceTextGenerator(..., Purpose: "consolidation_draft", Profile: sidecar.CompactorProfile)`；
  登錄
  `"consolidation_draft": {Trigger: ContextTriggerSidecarTask, FallbackAllowed: false, FallbackOutcome: "draft_unavailable"}`。
  workspace 使用 consolidate 既有的 `getContextWorkspace()`。
- 新邏輯放在新檔 `cmd/hufu/context_consolidation_draft.go`；`context_consolidation_cmd.go`
  只做旗標與分支接線。
- `cmd/hufu/completion.go` 的 `hufu context consolidate` extern 加入 `--draft`、`--model`、
  `--team-search-path`。

### 9.2 純邏輯（新套件 `internal/consolidation`）

```go
type DraftSource struct {
    ID      string `json:"id"`
    Kind    string `json:"kind"`
    Content string `json:"content"`
}
type DraftResult struct {
    Text             string   `json:"text"`
    CoveredSourceIDs []string `json:"covered_source_ids"`
}
type TextGenerator interface{ GenerateText(ctx context.Context, prompt string) (string, error) }
type JSONDrafter struct{ Generator TextGenerator }
func (d JSONDrafter) Draft(ctx context.Context, sources []DraftSource) (DraftResult, error)
// ValidateDraft requires non-empty text of at most 2000 runes, no secret-like
// material, not byte-identical to any single source, and CoveredSourceIDs
// equal to the source ID set.
func ValidateDraft(result DraftResult, sources []DraftSource) error
```

strict decode 規則同 §7.1。

### 9.3 測試

- `TestConsolidationValidateDraft`（table-driven：空、超長、secret、與來源相同、漏來源、多來源、
  合法）。
- `TestJSONDrafterStrictDecode`。
- `TestConsolidateDraftValidatesBeforeModelCall`（來源無效、衝突、過長：fake generator 呼叫
  次數為 0）。
- `TestConsolidateDraftIsIdempotentForPendingProposal`（多筆 proposed 時取最早的）。
- `TestConsolidateDraftPersistsLikeManualPath`（只差 `proposal_origin`/`draft_model`）。
- `TestConsolidateDraftFlagValidation`。

---

## 10. WP-7：Skill 管線

### 10.1 `hufu skill promote` 修正

`skill.PromoteDraft`（`internal/skill/lifecycle.go:109`）：

1. 讀取 draft，計算 `newName`（規則不變）。
2. frontmatter `name` 與 `newName` 不同時，只改寫 frontmatter 內第一個 `name:` 行，其餘位元組
   不動；frontmatter 沒有 `name:` 時回傳 error。
3. 以 `ValidateSkillDraft` 驗證改寫後內容，並要求 `def.Name == newName`。這比原本的
   `parseSkillBytes` 嚴格，缺 description 或 body 的 draft 會被拒絕。
4. 以 temp file + rename 原子改寫 `drafts/<draft>/SKILL.md`，再 rename 目錄。目錄 rename 失敗
   時 draft 留在原位；因為 name 已一致，重試仍正確。

測試：更新既有 `TestPromoteDraft`（`lifecycle_test.go:73-`）的 fixture，補上 `description:`
與 body，並斷言 frontmatter name 為 `foo`；新增 `TestPromoteDraftRewritesFrontmatterName`
（`DiscoverSkills` 找得到 `foo`、找不到 `draft-foo`）、`TestPromoteDraftKeepsBytesWhenNameMatches`、
`TestPromoteDraftRejectsDraftWithoutDescription`。

### 10.2 已套用 skill promotion 使用報告（唯讀）

- `ReadOnlyRepository.ListAppliedSkillPromotions(ctx, teamID string) ([]PromotionProposal, error)`：
  `type='skill' AND status='applied' AND team_id=?`，不限 project，依 `applied_at, id` 排序。
- `improve.Report` 新增：

  ```go
  // PromotedSkills is an association report, not a causal attribution.
  PromotedSkills            []PromotedSkillUsage `json:"promoted_skills,omitempty"`
  PromotedSkillsUnavailable string               `json:"promoted_skills_unavailable,omitempty"`

  type PromotedSkillUsage struct {
      ProposalID        string `json:"proposal_id"`
      SkillName         string `json:"skill_name"` // directory of skills/<name>/SKILL.md
      AppliedAt         string `json:"applied_at"` // RFC3339Nano UTC
      TasksSinceApplied int    `json:"tasks_since_applied"`
      Done              int    `json:"done"`
      Error             int    `json:"error"`
      RetriedTasks      int    `json:"retried_tasks"`
      UntimedTasks      int    `json:"untimed_tasks"`
  }
  ```

- 計算範圍與 `BySkill` 相同：report 的 selected runs。
  - 新增 CTE：每個 (run_id, task_id) 的 `first_event_unix_ns = MIN(timestamp_unix_ns)`，只取可
    解析（非 NULL）的值。
  - 所有事件 timestamp 都無法解析的 task 為 untimed，只計入 `UntimedTasks`。
  - 其餘 task 在 `first_event_unix_ns >= applied_at`（`AppliedAt` 轉 UnixNano）且 task skills
    含 `SkillName` 時計入。`Done`、`Error`、`RetriedTasks` 語意同 `groupBySkillQuery`。
- 資料來源：`<Report.Workspace>/context.sqlite`，以 `OpenSQLiteReadOnly` 開啟。
  - 檔案不存在：兩個欄位都省略，不產生任何輸出差異（包括 improve monitor 路徑）。
  - 開啟或查詢失敗：省略 `PromotedSkills`，`PromotedSkillsUnavailable = "query_failed"`
    （不含錯誤原文）。
  - 沒有已套用的 skill promotion：兩個欄位都省略。
- 所有產生 `Report` 的路徑都使用同一個 helper，包括 improve monitor
  （`cmd/hufu/improve_automation.go:157`）。
- `reporter.go` 新增 `## Promoted skills (association only)` 表格；`PromotedSkills` 為空時不
  輸出該段。

測試：`TestPromotedSkillUsageCountsOnlyTasksAfterApply`、`TestPromotedSkillUsageCountsUntimed`、
`TestPromotedSkillsOmittedWithoutContextStore`、`TestPromotedSkillsUnavailableOnQueryFailure`、
`TestListAppliedSkillPromotionsFiltersTypeAndStatus`。

---

## 11. WP-8：文件同步（與對應 WP 同 PR）

- `docs/architecture/knowledge-coverage.md`（WP-5）：
  - `conflicting` 改為「持久化的 pair judgment 在 derived state 為 open 的 shared persistent
    記憶」；
  - 說明分類本身對已持久化判定是 deterministic，判定則由操作者離線執行的模型產生；
  - 只影響歸因；加入 `conflicting_count`。
- `docs/architecture/memory-promotion.md`（WP-2、WP-5）：
  - §5.1 加入「無未解決衝突」門檻；§8 preflight 加入衝突檢查，並說明「阻擋但保持 approved」；
  - 依程式碼修正既有不一致：event store 是 `<workspace>/logs/event_store.jsonl`；list/show 以
    read-only 開啟、不 flush outbox；`DraftResult` 只回傳單一 type；
  - 記錄 steps 規則、`generated_draft_hash` 與 `draft_edited`。
- `docs/architecture/memory-learning.md`：
  - HF-MEM4-006 註明 `--draft`（WP-6）；
  - 在 §7 HF-MEM4-006 的 edge 清單旁註明：衝突判定使用 `context_pair_judgments` 而不是
    `contradicts` edge，並附 §2.2 的理由（WP-3）；
  - HF-MEM4-005 的 metrics 註明 `memory_token_overhead` 分母改為 prompt tokens（WP-1）。
- `docs/reference/context-sqlite-schema.md`：migration 表加入 10、11，表格說明加入
  `generated_draft_hash` 與 `context_pair_judgments`（WP-2、WP-3）。
- `AGENTS.md` CLI 表格加入 `hufu context conflicts`（含 `--vector` 需要本機 Ollama embedding
  model）與 `consolidate --draft`（WP-4、WP-6）。
- `README.md` 的 embedding model 說明（`:1154`、`:1670`、`:1695` 附近）註明：`ollama/` 前綴會在
  呼叫 Ollama embeddings API 前去掉；`hufu context query`/`rebuild --vector` 在預設設定下現在會
  實際使用向量（WP-4 §7.4）。
- `docs/reference/operator-command-reference.md`：**不手改**。在 `canonicalExamples` 加範例後，
  以 `go run ./cmd/hufu examples --format markdown` 重新產生（WP-4）。
- 更新 normative 文件時同步 header 的 `Verified-Commit`，並通過 `bin/check-docs`。

---

## 12. 相容性與測試矩陣

### 12.1 有意的輸出變更

以下變更是預期中的 schema 擴充，不算回歸：

| 位置 | 變更 |
| --- | --- |
| `hufu improve` JSON/Markdown | 新增 3 個 prompt cache 指標；有 cache tokens 時 `memory_token_overhead` 變小；可能出現 `promoted_skills` / `promoted_skills_unavailable` |
| `LearningView`（`hufu context learning`、operator snapshot） | 新增 5 個 `*int64` 計數（總是輸出，可能為 null）；snapshot hash 改變 |
| `hufu context promotion list/show` | 新增 `draft_edited` |
| `hufu context promotion analyze` text | 新增 `diagnostic` 行 |
| `hufu context consolidate` dry-run JSON | 新增 `conflicted_ids` |
| llm.log | cache 非零時新增兩個欄位 |
| debug bundle | usage 計數不再遮蔽 |
| `hufu context query`、`hufu context rebuild --vector` | 預設 embedding model 開始可用：query 在 Ollama 可用時會融合向量結果，rebuild 不再失敗（§7.4） |
| `hufu context conflicts scan --vector` | 重建 `<workspace>/context-vectors`、更新 `embedding_state`（與 `hufu context query` 相同） |

### 12.2 必須與 baseline 相同

零 cache tokens 時的 execution events JSON；沒有衝突時的 TaskResult/inspect coverage JSON；
prompt 位元組（§8.1）；promotion 的 eligibility 結果（在沒有衝突時）。

### 12.3 矩陣

| 面向 | 必須成立 |
| --- | --- |
| Migration | 10/11 可從 baseline 資料庫升級並重開；read-only 在 schema 9/10 上不報錯（§6.5） |
| 決定性 | tokens、lexical/file-path 候選對、judgment ID 在相同輸入下相同；向量候選取決於 embedding 輸出，失敗時整批丟棄；replay 後 manifest 的 knowledge state 相同 |
| 選用依賴 | 沒有 Ollama 或 embedding model 時，`--vector` 退回其他來源，exit code 不變；CI 測試不需要 Ollama |
| Fail-closed | promotion analyze/apply、consolidation 在衝突查詢失敗時拒絕；transport 錯誤不落庫 |
| Fail-open | runtime 衝突查詢失敗只降級歸因 |
| 人工控制 | 沒有任何路徑自動 supersede/reject/expire 記憶或自動 dismiss；dismiss 跨判定版本保留 |
| 隱私 | 事件 payload 與預設 CLI 輸出不含記憶內容、rationale、dismiss reason、judge model |
| 成本上限 | 每次 scan 最多 `MaxPairs` 次模型呼叫；已判定的對不重呼叫 |

測試遵守 CLAUDE.md 的 table-driven 規則，以及本 repo 的慣例：不 mock LLM（fake
`TextGenerator` 餵 canned string）、磁碟功能用 `t.TempDir()`。

---

## 13. WP 順序與 PR 拆分

```text
WP-1 (cache 觀測) ───────────────────────────────┐
WP-2 (promotion 正確性, migration 10) ─┐          │
                                       v          │
                         WP-3 (pair judgments, migration 11)
                            │                     │
                  ┌─────────┴─────────┐           │
                  v                   v           │
          WP-4 (conflicts CLI)   WP-5 (衝突效果)   │
                  └─────────┬─────────┘           │
                            v                     │
                 WP-6 (consolidation --draft)     │
WP-7 (skill promote + 使用報告) ──────────────────┤
                                                  v
                                     WP-8 隨各 WP 同 PR 更新
```

- 每個 WP 一個 PR；WP-1、WP-2、WP-7 可平行。WP-4 拆成兩個 PR：§7.4 向量庫前置修正先合
  （不依賴 WP-3，可與 WP-1 平行），`hufu context conflicts` 後合。
- WP-5 可拆成兩個 PR：gates（§8.2–8.4）先合，runtime 標記（§8.1）後合。
- commit message 用 single-quoted heredoc，避免 backtick 被 shell 執行。

---

## 14. 驗證指令與完成定義

Baseline 狀態（2026-09-23，`ee72f72`，本機）：`go test -count=1 ./...`、`go vet ./...`、
`golangci-lint run`（v2.12.2，0 issues）、`bin/check-docs` 全部通過，沒有已知失敗。任何新的
失敗都視為本計畫造成。

每個修改 Go 程式碼的 PR 都必須依序通過：

```bash
go test ./...
go vet ./...
golangci-lint run
bin/check-docs
```

整份計畫完成的條件：

1. 上述指令全部通過。
2. 對含 cache 命中事件的 fixture，`hufu improve` 顯示非零 `prompt_cache_hit_rate`；沒有 cache
   tokens 時為 0，其他指標與 baseline 相同。
3. 對含互相矛盾記憶（含一組純中文記憶）的 fixture workspace，以 fake judge 執行
   `hufu context conflicts scan`，產生 open 衝突；加上 fake 向量來源後，用詞不同的矛盾
   （「Use SQLite」對「migrated to PostgreSQL」）也被提出。之後：
   - promotion analyze 出現 `unresolved_conflict` diagnostic；
   - 已 approve 的 proposal 在 apply 時被阻擋，status 仍為 approved；
   - consolidation 拒絕該來源；
   - run manifest 標示 `conflicting`，prompt 位元組不變。
4. `hufu context supersede` 或 `hufu context conflicts dismiss` 之後：
   - 上述效果全部解除；
   - apply 直接成功，不需要重新 analyze；
   - 再次 scan 不會讓已 dismiss 的衝突重新 open。
5. 通過 analyze 的 skill draft 不會因 steps 規則在 apply 或 improve handoff 失敗；
   `hufu context learning` 顯示 rejected、stale、edited、open conflicts 計數。
6. `hufu skill promote draft-foo` 之後，runtime 可用 `foo` 載入；`hufu improve` 列出已套用
   skill promotion 的使用情形，並標示 association only。
