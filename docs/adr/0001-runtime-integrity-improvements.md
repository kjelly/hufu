# Runtime integrity improvements

## Status

Accepted

## Context

Hufu currently admits context capacity only in coordinator streams, while
worker, direct-agent, repair, extra-model, and sidecar requests call Fantasy
directly. Token accounting also lacks a provider-serialization contract, so
tool schemas, multimodal parts, and output reservations can be omitted from
the admission decision. Unbound workers can resolve ordinary workspace paths
that overlap artifact backing storage. Several lifecycle and persistence paths
have unbounded or racy behavior: model discovery lacks an invocation proxy,
preflight leases can deadlock on reentry, compaction telemetry reads branch
state without the compaction lock, and compaction checkpoints grow without
reachability-aware retention.

The runtime must preserve existing positive-capacity admission behavior,
redaction-before-EventID ordering, failed-compaction trimming, safe legacy
migration, and fork/checkout history semantics while making every model
request and artifact access obey one enforceable contract.

## Decision

1. Add one provider-request serializer/estimator contract in `internal/agent`
   and one admission wrapper owned by `internal/team`. Every Hufu-owned model
   invocation uses that wrapper. Admission counts the provider-equivalent
   serialized system/messages/tool schemas/multimodal parts, output reserve,
   and margin. Positive estimated capacity remains admissible; windowless
   capacity is rejected. The implementation must match Fantasy's
   OpenAI-compatible wire representation and does not add post-provider
   overflow retries.
2. Always create an attempt artifact scope and policy for workers and declared
   tools. An unbound scope authorizes no artifact references and blocks
   artifact data/meta backing roots; filesystem path permissions are not
   artifact-reference authorization.
3. Bound model discovery through an invocation proxy, release preflight leases
   with `defer`, reject reentry with a typed error, snapshot branch identity
   under `compactionMu`, and emit downshift telemetry only after provider model
   construction succeeds.
4. Retain compaction checkpoints by reachability: preserve active branch heads
   and extant fork ancestry, and prune only unreachable snapshots/generations.
   Linear predecessor generations needed by a live head remain durable; their
   source-range ancestry is validated with one active-path interval index rather
   than repeated full predecessor-chain scans. This bounds checkpoint retention
   and makes validation incremental without invalidating time-travel lineage.
5. Preserve the already-fixed admission, compaction ordering/failure/
   migration, and checkout-history invariants. Update repository guidance only
   after runtime behavior and regression tests are implemented.

## Consequences

All Hufu-owned requests share one deterministic capacity decision, reducing
provider-side context overflow surprises at the cost of maintaining an
explicit serialization adapter for the provider wire format. Unbound workers
lose incidental access to artifact storage and must use declared opaque
references. Compaction checkpoint storage is bounded relative to live branch
reachability; live linear generation ancestry remains available for time travel,
while unreachable historical data may be pruned. Indexed lineage validation
avoids quadratic work as that live ancestry grows. The additional lifecycle
checks make failures explicit and observable rather than blocking or reporting
events that did not occur.

## Alternatives considered

- Add guards only to coordinator streams: rejected because other model paths
  would remain outside the admission invariant.
- Add downstream nil checks or fallback paths for artifact access: rejected
  because the authorization owner is the attempt scope/policy.
- Use a fixed checkpoint count: rejected because it can break fork and
  time-travel lineage.
