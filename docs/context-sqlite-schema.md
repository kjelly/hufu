# Canonical context SQLite schema

`workspace/context.sqlite` is the canonical store for context records. It is
opened in WAL mode with a five-second busy timeout; Markdown remains a derived
human-readable projection, never an authoritative input.

## Schema migrations

Migrations are immutable definitions in `internal/context/sqlite_repository.go`.
`schema_migrations` records each version, name, application timestamp, and the
SHA-256 checksum of its SQL. Opening a store rejects a checksum mismatch. When
an existing store requires a new migration, hufu creates a timestamped
`context.sqlite.bak-*` recovery copy first.

| Version | Name | Purpose |
| --- | --- | --- |
| 1 | `initial_context_store` | Creates canonical records, edges, events, and the FTS5 projection. |
| 2 | `context_events_type_index` | Adds an event-type index for revision and audit queries. |
| 3 | `branch_id_lifecycle_schema` | Adds branch scope and explicit candidate/confirmed/rejected lifecycle state. |
| 4 | `outcome_driven_experience` | Adds experience aggregates, idempotent observations, and versioned learning policy snapshots. |
| 5 | `memory_consolidation_proposals` | Adds reviewable canonical-memory consolidation proposals. |
| 6 | `ltm_promotion` | Adds review-gated LTM promotion proposals, immutable source snapshots, and the lifecycle-event outbox. |

## Tables

### `context_items`

One row per canonical `ContextItem`. `id` is the stable canonical identifier;
`content_hash` supports duplicate detection within a compatible scope.

| Column group | Fields | Meaning |
| --- | --- | --- |
| Identity and content | `id`, `kind`, `content`, `content_hash` | Item type and normalized, redacted content. |
| Scope | `project_id`, `team_id`, `session_id`, `agent_id`, `task_id`, `attempt_id` | Project-to-attempt visibility hierarchy; nullable children indicate a wider shared scope. |
| Trust and selection | `authority`, `trust_level`, `priority`, `must_keep`, `pinned`, `confidence` | Rendering authority boundary and deterministic retrieval/selection inputs. |
| Provenance | `source_json`, `evidence_json`, `tags_json`, `metadata_json` | Source reference, evidence links, tags, and extensible metadata. |
| Temporal lifecycle | `created_at`, `updated_at`, `valid_from`, `valid_until`, `expires_at`, `superseded_by` | Recency, validity window, expiry, and replacement relationship. |
| Vector lifecycle | `embedding_state`, `embedding_model` | Rebuildable chromem index status and model used; canonical content is never deleted on model migration. |

Timestamps are UTC Unix milliseconds. Retrieval excludes superseded, expired,
not-yet-valid, and no-longer-valid records unless an explicit maintenance query
asks otherwise.

### `context_edges`

Directed provenance and lifecycle relations: `from_id`, `relation`, `to_id`,
JSON metadata, and creation time. The composite primary key prevents duplicate
edges. Supersession writes a `supersedes` edge in addition to setting
`context_items.superseded_by`.

### `context_events`

Append-only mutation audit log: monotonic `sequence`, event type, optional item
ID, scope JSON, payload JSON, and creation time. `Revision()` returns the
highest sequence for cache invalidation and observability.

### `context_items_fts`

FTS5 lexical projection with unindexed canonical `id` plus searchable `content`,
`kind`, and `tags`. Normal appends and expiry deletion update it transactionally.
`Repository.RebuildLexical` and `hufu context rebuild` recreate it from
`context_items` if repair is required; rebuilding does not modify canonical rows.

### Promotion tables

`promotion_proposals` stores the scoped draft, target-relative path, target base hash, metrics snapshot, and review status. `promotion_sources` preserves each source context ID, content hash, and aggregate revision without modifying or superseding the source. `promotion_event_outbox` transactionally records content-free lifecycle events; promotion commands deliver pending rows to the hash-chained event store and then mark them delivered. Proposed or rejected drafts are not runtime context inputs.

## Indexes

| Index | Columns | Query served |
| --- | --- | --- |
| `idx_context_scope` | project/team/session/agent/task | Scope-filtered collection and retrieval. |
| `idx_context_kind` | project/kind | Kind collection. |
| `idx_context_created` | project/created descending | Recent context selection. |
| `idx_context_hash` | project/content hash | Duplicate detection. |
| `idx_context_validity` | project/valid-until/expires-at | Validity and expiry filtering. |
| `idx_context_events_type` | event type | Event/revision inspection. |

## Operational checks

Run `hufu context rebuild --workspace <workspace>` to rebuild FTS5, or add
`--vector --project <project>` to rebuild the disposable vector index from
confirmed current SQLite items. Use `hufu context query`, `list`, `show`,
`candidates`, `history`, `confirm`, `reject`, and `supersede` for lifecycle
inspection and explicit maintenance. To upgrade an old chromem store, first
run `hufu context migrate-memory --workspace <workspace> --project <project>
--legacy-project <old-project>` for its count/checksum dry run, then repeat
with `--apply`; the destination database is backed up before mutation. These
commands never make Markdown or vector documents authoritative. The migration,
projection, FTS rebuild, and redaction tests in `internal/context` provide the
automated schema contract.
