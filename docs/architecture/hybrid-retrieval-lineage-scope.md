# Hybrid retrieval lineage scope

> Status: active
> Authority: normative
> Verified-Commit: 825ff686
> Supersedes: —
> Superseded-By: —

## Scope and authorization

The [session continuation plan](../implementation-plans/session-continuation-and-derived-forks.md#8-wp6ancestor-memory-檢索完整性獨立工作包)
provides the product requirement for pre-limit ancestor authorization. This
replaces the former discovery-only design. The runtime reuses
`SearchRequest.AllowedItemIDs`; there is no parallel constraint type, index,
result store, or lineage authority.

Worker memory derives each ancestor's allowed IDs from verified event lineage
at its fork cutoff. A result ID is a lookup key, not an authorization grant.
Canonical scope, lifecycle, supersession, validity, kind, and confidence still
apply even when an ID is explicitly allowed.

## Constraint representation

- `AllowedItemIDs == nil` adds no lineage restriction.
- A non-nil empty list authorizes no items.
- `WithAllowedItemIDs` copies, bytewise sorts, and deduplicates exact IDs. It
  does not trim whitespace or change case. Retrieval rejects an empty ID with
  `ErrInvalidAllowedItemID`; normalization cannot silently discard it.
- Hybrid retrieval freezes the input and gives each source a separate copy.
  A source cannot mutate a slice to change another source's authorization or
  the final common guard.

## Source and fusion boundaries

`SearchExact` and `SearchLexical` apply allowed-ID membership together with
canonical authorization, before ordering and `LIMIT`. The predicate binds a
JSON string array as one SQL parameter:

```sql
id IN (SELECT value FROM json_each(?))
```

The JSON encoder produces the parameter; IDs never enter SQL syntax. Duplicate
IDs cannot multiply rows, and large sets do not consume one bound variable per
ID. An explicit empty set uses a false predicate. Explicit-ID canonical
queries use the existing primary-key index: a bound JSON array otherwise lets
SQLite choose a whole-scope scan when it cannot estimate the set size. Restricted
FTS uses the existing fixed join order and checks the FTS ID before looking up
the canonical row; all canonical scope and validity predicates remain required.

`SemanticIndex` continues to score only covered, canonically authorized rows
before top-K. Its canonical iterator now sends IDs into the SQLite predicate,
so unauthorized ancestors are excluded before hydration as well as scoring.
Deleted, stale, or unauthorized vector references do not acquire authority
from vector metadata.

`HybridRetrieve` passes the same restriction to exact, lexical, and semantic
requests and rechecks it before fusion. It retains exact-prefix selection,
RRF, scope ranking, MMR/content deduplication, file-path boosts, and existing
tie breakers. Cancellation before or during MMR returns an error without
publishing partial selected results.

Worker memory retains its final scope and lineage guards after retrieval.
This protects the output if an alternate vector backend returns out-of-scope
or stale entries despite its input restriction.

## Ancestor recall completeness

A restricted ancestor request retrieves up to the entire allowed set's size,
with a minimum positive query limit for an empty set. Exact and FTS cannot
spend this allowance on post-fork records. Worker tier ranking, deduplication,
and item/token budgets then operate on the authorized candidates.

The old fixed 100,000 overfetch ceiling no longer limits restricted ancestors.
The active branch's existing 100,000 retrieval allowance is unchanged; this
work does not claim global unbounded recall. Persistent cross-session recall
also retains its existing limits and policy.

A large authorized matching set can still make existing fusion/MMR expensive.
Pre-limit authorization eliminates unauthorized candidate pressure; it does
not replace MMR or promise constant cost for arbitrarily large allowed sets.

## Verification and cost evidence

Behavior tests cover nil versus empty restrictions, opaque identities, empty-ID
rejection, a 40,000-ID set, canonical scope enforcement, backend mutation,
vector sentinels, cancellation, and complete authorized-oracle ordering.
The end-to-end worker fixture has 100,010 matching records, including more
than 100,000 post-fork records; all eight allowed ancestor records must match
the exhaustive oracle in order. Only those authorized rows reach fusion.

`BenchmarkLineageRetrieval` covers exact, FTS, and the real semantic index;
`BenchmarkWorkerAncestorRecall` covers the worker service. Both use fixed
fixtures at 1K, 10K, 100K, and 100,002 rows, with five warm serial samples.
The SQLite benchmark's driver wrapper counts actual canonical content rows,
including rows filtered after hydration in the baseline iterator. Projection
and metadata rows are excluded from that metric. Each benchmark logs ordered
IDs; neither result counts nor timings alone prove authorization correctness.

Benchmarks use local synthetic data and a fake embedder. They do not measure
real provider latency, production SLOs, or user-task success. The plan's small
fixture latency/allocation gate and final repository validation must pass
before this implementation is declared complete.
