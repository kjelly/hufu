# Semantic memory retrieval

> Status: active
> Authority: reference
> Verified-Commit: `3ea2749c`
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
  mode: shadow                     # off (default) | shadow | active | auto
  # fusion: score_normalized       # how requests that use the lane fuse it
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
| `auto` | Decides per request: requests the lexical lane can read run exactly as `off`; the others run as `active`. See [Automatic routing](#automatic-routing). |

`fusion` (`legacy`, `rrf_normalized`, or `score_normalized`; default
`score_normalized`) applies only to requests that use the lane: `active`, and
`auto` requests routed to it. Requests without the lane, and `shadow`, keep the
memory policy's fusion so their selection is unchanged. `legacy` fusion carries
raw BM25 scores that outrank every vector-only candidate: in the 2026-10-13
replays it left the semantic lane with no effect on selection.

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

The legacy `embedding-model` setting and the `--memory-model` flag are
deprecated and have no effect ([deprecation record](../deprecations/legacy-memory-store.md)).
`hufu context query`, `hufu context rebuild --vector`, and
`hufu context conflicts scan --vector` use these settings (see
[Querying from the command line](#querying-from-the-command-line)).

## Automatic routing

FTS keeps ASCII terms only, so it cannot read Chinese, Japanese, Korean,
Cyrillic, or other non-Latin text. `auto` measures the share of the request
goal (worker recall: its query) that the lexical lane drops: non-ASCII letters
and digits, weighted by the token estimate below (CJK × 1.3, other × 0.4).
At 0.15 or more the request uses the semantic lane with `fusion`; below it the
request runs exactly as `off`, with no embedding request. Each shared ranking
or worker recall is routed once.

The rule reads only the script of each character; it never interprets words.
Latin-script languages with few accents, such as German, stay on the lexical
lane, which still reads their ASCII words. The threshold comes from hufu
history: all 200 English review goals scored below 0.15 (198 below 0.01), and
49 of 54 Chinese research goals scored 0.15 or more.

The runtime still loads the model and keeps the index fresh in `auto`, so a
routed request does not wait. A request routed to the lane writes a trace line
and records a `semantic_retrieval` identity (mode `active`) in its manifest; a
request routed lexical records neither, exactly like `off`.

## Runtime behavior

- **Cold start.** With `shadow`, `active`, or `auto`, the coordinator loads the model
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
- **Authorization before top-K.** For requests that use the lane, the shared persistent
  ranking passes the authorized item IDs to every lane before the candidate
  limit.
- **Cancellation.** The caller's own cancellation and canonical store
  failures propagate; only encoder-side failures fall back.

Only `active` and `auto` settings are pinned in the execution policy snapshot,
so turning `shadow` on or off never blocks a resume. Switching to or from
`active` or `auto`, or changing their settings, blocks a resume like any other
policy change; start a new session (`--new`) instead.

## Fallback reasons

Manifests, receipts, and traces record one content-free reason:
`none`, `projection_missing`, `projection_incomplete`, `projection_corrupt`,
`embedding_warming`, `embedding_busy`, `embedding_timeout`,
`embedding_unavailable`, `embedding_input_too_long`,
`embedding_profile_mismatch`, `embedding_failed`, `invalid_vector`, or
`empty_remainder` (the goal held only exact identifiers).

## Operator commands

```bash
hufu context semantic status  -w <workspace> [--project <id>] [--team <name>] [--json]
hufu context semantic build   -w <workspace> [--project <id>] [--team <name>] [--dry-run]
hufu context semantic verify  -w <workspace> [--project <id>] [--team <name>] [--strict] [--offline]
```

- `status` is read-only and offline: settings, the active generation, and
  per-item coverage.
- `build` embeds new or edited items and activates a generation. `--dry-run`
  reports the plan without contacting the encoder or writing.
- `verify` checks vector integrity and coverage and, unless `--offline`,
  whether the active generation still matches the encoder's current profile
  and digest. Uncovered items are a warning unless `--strict`.

`--project` may be omitted when the store holds one project. The settings are
the ones a run of the team uses: hufu.yaml merged with the team's own
`semantic-retrieval` block. The team is `--team` (it must be in the team
search path), else the only team with memories in the project; an inferred
team that is not found, or a project with several teams, uses hufu.yaml
alone. `status` and `verify` print the source, for example
`hufu.yaml + team web-research`, and JSON adds `team` and `settings_source`.

## Querying from the command line

`hufu context query`, `hufu context rebuild --vector`, and
`hufu context conflicts scan --vector` read the same generations and settings
as a run (hufu.yaml merged with the `--team` block). Before using the lane
they refresh the project's generation like `build`: only new or edited items
are embedded, and a failed refresh is a warning, after which requests whose
candidates are not covered fall back to exact and lexical.

- `context query` picks the lane like a run: `off` and `shadow` stay exact and
  lexical without contacting the encoder, `auto` routes by the query text,
  and `active` always uses the lane with `fusion`. `--semantic-mode` overrides
  the mode for one query. An unreachable encoder is a warning and the query
  falls back.
- `context rebuild --vector --project <p>` rebuilds FTS5 and then refreshes
  the generation regardless of the mode, like `build`.
- `context conflicts scan --vector` needs `active` or `auto`; otherwise it
  reports `vector=unavailable`. Neighbors are scored from stored document
  vectors, so the scan embeds no query text.

Earlier versions kept a separate chromem index in `<workspace>/context-vectors/`
and re-embedded every memory on each query. Nothing reads that directory any
more; delete it when convenient.

## Traces

In `shadow`, `active`, and `auto`, every ranking request appends one line to
`<workspace>/semantic-retrieval-traces.jsonl`:

- **Attribution:** run, task, attempt, context request ID, phase, trigger,
  role, agent, and `source` (`shared_persistent` or `worker_private`).
- **Route:** `route` is `semantic` when the lane ran and `lexical` when `auto`
  kept the request lexical (mode `off`, no embedding request);
  `route_share` is the routed text's non-ASCII share, rounded to two decimals.
- **Retrieval:** mode, model and generation identity, result counts, the
  overlap with lexical results, latency, the fallback reason, and HMAC hashes
  of the query and the top result IDs.
- **Encoder:** covered and uncovered candidates, query tokens, whether the
  query was cut to the budget, the wait for an encoder slot, and the
  embedding time.

Query and memory text are never written. The HMAC key is
`<workspace>/semantic-trace.key` (mode 0600). When a run finishes, its runtime
status is appended to `<workspace>/semantic-runtime-status.jsonl`: warm state,
model, and generation as of the finish, and that run's query, fallback, route,
warmup, and refresh counts.

## Inspecting

```bash
hufu inspect semantic-retrieval [run-id] [--task <task-id>] [--format json]
```

Shows the runtime status recorded when the run (or the latest run) finished, a table
per source, route, and mode (retrievals, served without fallback, fallbacks,
truncated queries, p50/p95 latency, p95 queue wait, p50/p95 embedding time),
the fallback reasons, and one row per task attempt with its route and route
share. Each trace is one retrieval: a ranking request whose full query finds
nothing retries with its goal line and counts twice. Text output lists the
latest 40 task rows; JSON lists all. The run
report adds the same runtime status and route table as **Semantic Retrieval
Runtime**.

## Rollback

Set `mode: off` and start a new session. Canonical memory and generations are
untouched; `off` sends no request and changes nothing in prompts.

## Explaining and replaying

`hufu context explain-memory` and `hufu context ranking-replay` recompute the
semantic lane and auto routing as a run of the team would: the settings are
hufu.yaml merged with the team's own `semantic-retrieval` block (found by
`--team` in the team search path). They load the encoder and read the active
generation, but never refresh the index or write traces, so memories added
since the last refresh report `projection_incomplete` until a run or
`hufu context semantic build` embeds them.

```bash
hufu context explain-memory <id> --project <p> --team <t> --query <goal> [--semantic-mode off|shadow|active|auto]
hufu context ranking-replay --project <p> --team <t> --query <goal> \
  [--semantic-mode <mode>] [--candidate-semantic-mode <mode>] [--candidate-fusion <fusion>]
```

- `--semantic-mode` overrides the configured mode; `off` gives the lexical
  ranking alone.
- explain-memory adds a `semantic` block: mode, fusion, encoder state, model,
  generation, the query's route and route share, and the lane's fallback.
  `fusion` at the top level is the fusion the ranking actually used.
- ranking-replay compares the baseline with a candidate that may differ in
  semantic mode, fusion, or both. With `--candidate-semantic-mode` and no
  `--candidate-fusion`, the candidate keeps the baseline fusions, which
  isolates the effect of the lane; a `--candidate-fusion` replaces the lane's
  fusion too. The report adds per-query routes and fallbacks and their counts.
  Each query is embedded once per replay and every ranking of it sees the
  same lane result.

## Doctor

`hufu doctor` checks semantic retrieval without loading or pulling a model:

- `semantic.config`: the resolved settings of hufu.yaml and of each team
  that overrides them; an invalid block fails.
- `semantic.encoder`: for each distinct enabled encoder, that Ollama serves
  the model and its context fits the input limit, with the digest prefix and
  dimensions. An unreachable endpoint or a model that is not pulled is a
  warning, because retrieval then falls back to lexical.
- `semantic.legacy-embedding-model`: a warning when the deprecated
  `embedding-model` setting is still in hufu.yaml; it has no effect.

## Known limits

- Quality (2026-10-13, embeddinggemma, offline replay judged by an LLM;
  useful@4 is the share of injected memories judged directly useful): on 200
  English review tasks the lane did not help (0.352 without it, 0.336 with
  `score_normalized`); on 54 Chinese research tasks it did (0.472 without it,
  0.694 with `score_normalized`). The Chinese set promoted session memories to
  shared ones for the replay. This is why `auto` keeps English requests on
  the lexical lane. Methods and the CPU benchmark are in the
  [evaluation](../archive/analyses/semantic-embedding-evaluation-2026-10-13.md);
  the design history is in the
  [implementation record](../archive/implementation-plans/semantic-retrieval-runtime.md).
