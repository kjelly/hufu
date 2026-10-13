# Semantic memory retrieval

> Status: active
> Authority: reference
> Verified-Commit: — (working-tree implementation; tests are authoritative)
> Supersedes: —
> Superseded-By: —

Semantic retrieval adds an embedding lane next to exact and FTS5 retrieval
for canonical memory. Exact and lexical retrieval only see ASCII terms, so a
Chinese goal cannot find a Chinese memory and an English goal cannot find a
Chinese one; an embedding model can. The lane is off by default.

`context.sqlite` stays the only source of truth. Vectors live in rebuildable
semantic generations (see the
[schema reference](context-sqlite-schema.md#semantic-embedding-projection-tables));
they never authorize an item. Scope, lifecycle, expiry, activation, and
VERIFY isolation still run before any vector is scored.

## Configuration

Add a `semantic-retrieval:` block to `~/.config/hufu/hufu.yaml`,
`./hufu.yaml`, or a team's `team.yaml`. Later layers override earlier ones
field by field; the team block wins.

```yaml
semantic-retrieval:
  mode: shadow                     # off (default) | shadow | active
  model: ollama/embeddinggemma:latest
  endpoint: http://127.0.0.1:11434
  # allow-remote-endpoint: true    # required for a non-loopback endpoint
  # max-input-tokens: 2048         # default: the model preset (see below)
  # query-max-tokens: 512          # default: the model preset
  # query-timeout: 3s
  # load-timeout: 30s
  # index-timeout: 5m
  # keep-alive: 10m
  # max-concurrent: 1
  # index-batch-size: 8
```

| Mode | Effect |
|---|---|
| `off` | No request to the encoder, no goroutine, no prompt change. |
| `shadow` | Embeds queries and writes content-free traces, but never changes which memories are selected. |
| `active` | Fuses the semantic lane into ranking for shared persistent memory and worker private memory. |

Only measured models are accepted:

| Model | Input limit | Query budget | Prompts |
|---|---|---|---|
| `embeddinggemma` (any tag) | 2048 tokens (model maximum) | 512 tokens | `task: search result \| query: ` / `title: none \| text: ` |
| `qwen3-embedding:0.6b*` | 4096 tokens | 128 tokens | Qwen3 retrieval instruction / none |

The budgets come from a CPU benchmark on hufu history data (embeddinggemma
was about five times faster; qwen3-embedding:0.6b missed a 3-second query
deadline on 9% of real task goals). On other hardware choose
`query-max-tokens ≤ query-timeout × measured tokens/s × 0.5`.

hufu never pulls a model. A model missing from `ollama list` leaves the lane
failed and retrieval falls back to exact and lexical.

The endpoint must be loopback unless `allow-remote-endpoint: true`, because
memory text is sent to it. Under `--no-net` a non-loopback endpoint is refused.

The legacy `embedding-model` setting is separate: it names the chromem index
used by `hufu context query` and `hufu context rebuild --vector`. Its default
is the same `ollama/embeddinggemma:latest`.

## Runtime behavior

- **Cold start.** With `shadow` or `active`, the coordinator loads the model
  in the background (`/api/embed` with an empty input) and checks `/api/ps`
  for the pinned digest and context length. Until that finishes, retrieval
  falls back to exact and lexical (`embedding_warming`); a query never waits
  for the model. The load timeout does not count against the query timeout.
- **Pinned identity.** The encoder profile (model digest, dimensions,
  document prompt, normalization, input limit, adapter version) is hashed
  into the generation identity. A new digest under the same tag never reads
  old vectors; the next refresh builds a new generation.
- **Input limits.** Every request sends `options.num_ctx` equal to the input
  limit and `truncate: false`. A document over the limit is recorded as an
  `oversize` exclusion and stays reachable through exact and lexical
  retrieval. A query is cut to the query budget first (below).
- **Query budget.** The semantic lane embeds only the request goal, without
  the `phase:`/`trigger:`/`role:` lines; exact and lexical keep the full
  request. Goals longer than the budget are cut at the last sentence or word
  boundary inside it, estimated as CJK runes × 1.3 + other runes × 0.4.
  Queuing for the encoder counts toward the query timeout; a query that
  cannot get a slot in time falls back (`embedding_busy`), and one that runs
  past the timeout falls back (`embedding_timeout`) and triggers a background
  re-check of the loaded model.
- **Per-item freshness.** A request uses the semantic lane only when every
  authorized candidate has a vector (or an exclusion) for its current content
  hash. Otherwise that request falls back (`projection_incomplete`) and the
  refresher is notified. Writes outside the candidate set (for example other
  workers' private notes) never invalidate the lane.
- **Incremental refresh.** The refresher copies unchanged vectors forward
  and embeds only new or edited items, polling every two seconds. The index
  covers candidate and confirmed items of every scope that are not
  superseded (inventory policy `retrievable-v1`).
- **Authorization before top-K.** In `active` mode the shared persistent
  ranking passes the authorized item IDs to every lane before the candidate
  limit.
- **Cancellation.** The caller's own cancellation and canonical store
  failures propagate; only encoder-side failures fall back.

Only `active` settings are pinned in the execution policy snapshot, so turning
`shadow` on or off never blocks a resume. Changing `active` settings blocks a
resume like any other policy change; start a new session instead.

## Fallback reasons

Manifests, receipts, and traces record one content-free reason:
`none`, `projection_missing`, `projection_incomplete`, `projection_corrupt`,
`embedding_warming`, `embedding_busy`, `embedding_timeout`,
`embedding_unavailable`, `embedding_input_too_long`,
`embedding_profile_mismatch`, `embedding_failed`, `invalid_vector`, or
`empty_remainder` (the goal held only exact identifiers).

## Operator commands

```bash
hufu context semantic status  -w <workspace> [--project <id>] [--json]
hufu context semantic build   -w <workspace> [--project <id>] [--dry-run]
hufu context semantic verify  -w <workspace> [--project <id>] [--strict] [--offline]
```

- `status` is read-only and offline: settings, the active generation, and
  per-item coverage.
- `build` embeds new or edited items and activates a generation. `--dry-run`
  reports the plan without contacting the encoder or writing.
- `verify` checks vector integrity and coverage and, unless `--offline`,
  whether the active generation still matches the encoder's current profile
  and digest. Uncovered items are a warning unless `--strict`.

`--project` may be omitted when the store holds one project.

## Traces

In `shadow` and `active`, every semantic query appends one line to
`<workspace>/semantic-retrieval-traces.jsonl`: mode, model and generation
identity, result counts, the overlap with lexical results, latency, the
fallback reason, and HMAC hashes of the query and the top result IDs. Query
and memory text are never written. The HMAC key is `<workspace>/semantic-trace.key`
(mode 0600).

## Rollback

Set `mode: off` and start a new session. Canonical memory and generations are
untouched; `off` sends no request and changes nothing in prompts.

## Known limits

- `hufu context explain-memory` and `hufu context ranking-replay` reproduce
  exact and lexical ranking only; in `active` mode they do not replay the
  semantic lane.
- Quality is measured on a 16-pair sample only: both models recalled Chinese
  task results that ASCII lexical retrieval missed, while lexical retrieval
  was better on English. Keep `shadow` until traces show the lane helps.
