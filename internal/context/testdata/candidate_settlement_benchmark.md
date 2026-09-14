# Candidate-settlement typed-query benchmark

Captured on 2026-09-14 on Linux amd64, Intel Core i7-6700K CPU @ 4.00GHz.

`BenchmarkCandidateSettlementQuery` compares the former broad maintenance query plus Go filtering with the typed lifecycle, origin-run, and source-type predicates. Fixtures contain 1% matching candidates and realistic non-matching confirmed, wrong-run, and wrong-source rows. Fixture construction is outside benchmark timing.

Five samples produced these medians:

```text
fixture  query   latency   rows decoded/returned  B/op       allocs/op
10K      legacy  258.0ms   10,000                 50.43 MiB   739,621
10K      typed    11.08ms     100                  429.8 KiB    7,521
100K     legacy    2.490s  100,000                536.9 MiB  7,399,629
100K     typed    96.53ms    1,000                  4.35 MiB   74,124
```

At 100K rows the typed query reduced median caller latency by about 96%, returned/decoded rows by 99%, allocation volume by about 99%, and allocation count by about 99%.

The stable query-plan assertion records:

```text
SEARCH context_items USING INDEX idx_context_scope
  (project_id=? AND team_id=? AND session_id=? AND branch_id=? AND agent_id=? AND task_id=?)
USE TEMP B-TREE FOR ORDER BY
```

The JSON lifecycle/run/source predicates remain residual filters over the authorized scope. This evidence supports reduced Go decoding and caller latency; it does not claim that SQLite scans only matching JSON rows. The proposed origin-run expression index remains deferred to the canonical index review because its write and database-size costs have not passed that gate.
