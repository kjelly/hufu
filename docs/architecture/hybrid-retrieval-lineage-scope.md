# Hybrid retrieval lineage scope

> Status: draft
> Authority: normative
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

## Decision status

This is the WP-8C discovery deliverable. It is not accepted and does not authorize a runtime change. `internal/team/worker_memory.go` therefore retains its explicit `Limit: 100000` workaround and post-retrieval allowed-ID check. That ceiling remains a known correctness risk for restricted ancestor branches with more than 100,000 otherwise eligible newer results.

## Existing constraint flow

For each session-lineage branch, worker memory constructs the branch scope and calls `HybridRetrieve`. Active branches are scope-authorized without an additional item set. Restricted ancestor branches also carry an exact `AllowedItemIDs` set derived from the validated event lineage, but that set is currently applied only after retrieval.

The retrieval paths behave as follows:

- `SearchExact` applies content matching, canonical scope visibility, supersession, expiry, validity, lifecycle, kind, and confidence in SQLite, orders by priority/creation/ID, then applies `Limit`.
- `SearchLexical` ranks FTS5 matches, joins canonical rows, applies the same authorization and lifecycle constraints, then applies `Limit`.
- `VectorStore.SearchVector` asks chromem for `Limit` neighbors, hydrates each ID from canonical SQLite, and applies `isRetrievable`. The canonical row—not vector metadata—is the authorization source.
- `HybridRetrieve` runs all exact terms, lexical search, and optional vector search; filters kinds/confidence; fuses lexical/vector ranks with RRF; applies scope ranking, content deduplication/MMR, exact-result prefixing, file-path boosts, deterministic tie breakers, and only then the final limit.
- Worker memory finally rechecks canonical scope, applies the restricted ancestor allowed-ID set, ranks memory tiers, deduplicates content, and applies item/token limits.

Because every source limits before the allowed-ID filter, unrelated post-fork matches can crowd out an authorized ancestor. Lowering the current ceiling makes that failure more likely; `RepositoryQuery` and `Iterate` cannot fix ranked retrieval.

## Proposed representation

Add one typed, immutable lineage constraint to `SearchRequest`:

```go
type AllowedContextIDs struct {
    Restricted bool
    IDs        []string
}
```

`Restricted=false` means no additional lineage predicate. `Restricted=true` with an empty set means no item is eligible. Construction must trim nothing—the context ID is an opaque canonical key—reject empty IDs, deduplicate exact values, and freeze a lookup map before any source runs.

For SQLite exact and lexical searches, bind the normalized IDs as one JSON array parameter and join `json_each(?)` by value. This avoids caller-generated SQL, identifiers, and variable-count expansion, so sets larger than SQLite's bound-variable limit retain identical behavior. The JSON payload must be produced by `encoding/json`, never string concatenation.

For vector retrieval, the same normalized set must be applied before final neighbor truncation. The current chromem API has no arbitrary allowed-ID filter. A correctness-first implementation would request the complete collection for restricted searches, discard disallowed IDs before canonical hydration, then rank the remaining authorized results. That has unbounded query cost and must be benchmarked before acceptance. Staged over-fetch is not acceptable because it reintroduces a silent ceiling. A future vector backend with native ID-set filtering would be preferable.

`HybridRetrieve` must pass the identical immutable constraint to every exact, lexical, and vector request and retain a final common allowed-ID assertion before fusion. Worker memory may then remove its 100,000 workaround only after parity tests pass.

## Required acceptance evidence

Before implementation is accepted, tests must cover:

- active branch with no lineage restriction;
- restricted parent at and before its fork point, excluding post-fork parent and sibling items;
- restricted empty set;
- duplicate IDs and an invalid empty ID;
- an allowed set larger than SQLite's `MAX_VARIABLE_NUMBER`;
- equal ordered `ContextItem` results for exact, lexical, and vector-only fixtures;
- equal fused order after RRF, MMR/content deduplication, file boosts, and final limit;
- stale/deleted vector IDs and canonical hydration authorization;
- cancellation and bounded diagnostic output.

Benchmarks must report exact/FTS/vector latency, candidates considered, canonical rows hydrated, allocation volume, and end-to-end worker-memory latency at representative lineage sizes. Until the vector path has an accepted bounded-cost implementation, this design remains draft and the existing risk stays explicit.
