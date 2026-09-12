# session-typed-local

Producer: fixed legacy fixture builder v1 (pre canonical execution identity).

The typed task target, topology, and backend binding use the historical
`local` spelling. The append-only materializer must canonicalize all three to
`ollama`.
