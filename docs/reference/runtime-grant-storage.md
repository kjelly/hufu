# Historical result grant storage compatibility

> Status: active
> Authority: reference
> Verified-Commit: 95e66636
> Supersedes: —
> Superseded-By: —

## Format and authority

Runtime-store migration 5 adds `historical_grant_cycles`, keyed by consumer
branch and cycle. A row contains only the exact event ID and hash of the
`historical_results_granted` event. It is an index into the existing canonical
journal, not another result store or a standalone authorization grant.

The event store validates the versioned reference envelope and writes its event
and reference in the same transaction, including event-log import. The low-level reference helper checks
that the event type, branch, cycle/run, ID and hash match. Exact admission replay
is idempotent; a different source cannot replace an existing cycle reference.
Reading an index with a missing or changed source fails closed.

The envelope has schema version 1. It binds the consumer's absolute control
workspace, canonical team, session, branch and cycle to the policy fingerprint.
Its sorted, unique recipient/result pairs identify the exact accepted source
session, branch, run, task, agent, occurrence attempt and dispatch attempt.
Source-event, typed-payload and presentation SHA-256 digests remain separate;
only the presentation's byte count is stored here, never its body. The envelope
allows at most 16 pairs and 1 MiB of declared presentation bytes. Admission must
still reconstruct the presentations and verify those claims against canonical
sources before they can authorize delivery.

The event reader, writer and importer reject malformed envelopes, duplicate
JSON members, unknown fields, unsupported versions and mismatched identities.
Writer validation runs before redaction can collapse duplicate members and
again on the final redacted event. Reusing an admission key requires equivalent
frozen metadata and the original intact index; a changed grant or missing index
fails closed. Another key cannot replace the same branch/cycle admission.
The journal's first grant event for that branch/cycle remains authoritative
even when the index has been deleted. Index writes reject a later event, and
index reads reject a reference redirected to a later event with a valid hash.

Read-only source preparation resolves worker aliases to their canonical
identity, deduplicates and sorts recipient/result pairs, and rejects
coordinator/orchestrator recipients, disabled historical reuse, unmanaged or
mismatched control roots, unavailable policies and invalid sources. It uses the
existing canonical reader for the exact consumer team and branch; IDs are
locators, never credentials. One missing required source rejects the entire
set. Disabling historical memory alone does not disable explicit references.

The reader provides a complete redacted JSON projection and a separate
`payload_sha256` over the original stored `typed_result` bytes. Unknown fields
and exact JSON integers survive projection. Canonical top-level runtime outputs
keep their receipt-bound values; untrusted nested objects cannot gain that
exception by naming their own fields `source` and `runtime_outputs`.
Projection and presentation encoding preserve pre-escaped raw JSON strings, so
canonical structured values such as `\u003c` are not rewritten to `<` underneath
their stored SHA-256. No receipt hash is recomputed to accommodate a projection.

Preparation renders summary, complete typed JSON and source/validity citation
as fenced historical data. It imports no raw output, conversation or artifact
bytes and grants no artifact reads. Native output and structured-payload hashes
must remain valid. The complete UTF-8 presentation, including its labels, is
counted against the 1 MiB total; it is never truncated. Rebuilding a frozen
envelope requires matching source, payload, presentation and policy digests.

This preparation API is private-content runtime plumbing: it does not write an
admission event or create tasks. CLI entry eligibility, cycle activation, task
binding, required model-context delivery, dispatch proof and source-content
restoration remain WP3 implementation work. These stages do not enable
`--inherit-result` or grant any worker access.

## Cycle projection and recovery

The session checkpoint stores only `historical_result_grants`, a detached
branch/cycle/event/hash reference. Replay retains the original grant through
resume invocations and clears it at the next `workflow_cycle_started` boundary.
The previous grant event and index remain available as historical evidence.
Conflicting grants, grants for another cycle, and grants after task creation
are invalid lineage and require recovery.

Startup, branch rebuild and active-branch refresh verify the grant against the
canonical policy admitted before it, its control workspace and its original
transactional index. A missing index is an error; recovery does not manufacture
a replacement event or index. A missing checkpoint can be rebuilt from these
authorities. A checkpoint that names a different grant in the same cycle is
rejected.

An ordinary fork retains a grant only when its exact ancestor event is visible
before the fork cutoff. The reference keeps the ancestor's branch identity;
checkout creates no child grant index. This authenticates the envelope only:
restoring source content must still revalidate source visibility, validity and
all frozen content hashes on the consumer branch before model delivery.

## Upgrade and downgrade

A writable open uses the existing checksum-verified migration lifecycle.
Migration 5 applies to the entire `runtime.sqlite`, even when no inherited
result flag has been used. A binary supporting only migrations through 4 must
reject this database. There is no down-migration.

New read-only opens retain support for schema 3 and schema 4. They validate the
original migration names and checksums and do not migrate or rewrite the
source. An old schema contains no grant references; the reader does not infer
them from history. A new schema whose required grant table is missing is an
error, rather than an old-format fallback.

Use a current binary's existing read-only inspect/export interfaces to examine
an upgraded database. Historical old-format fixtures remain readable. No part
of the local compatibility test operates on a user database.

## Evidence

The storage tests cover first admission, duplicate replay, branch/cycle
isolation, conflicting replacement, source identity/type/hash mismatch,
transaction rollback, corrupted reference, cancellation, old-schema byte
preservation and rejection by the previous schema verifier.

A binary built once from the retained `4e13abfb` archive was exercised against
an initialized managed required-mode fixture created with the current CLI's
snapshot/fork/checkout path. Run, resume, session fork, checkout and results all
exited non-zero. Provider call count remained zero; project files, control files
and complete project-state file hashes were unchanged. The current reader then
reopened the fixture successfully (package time 1.009s).

Run, fork, checkout and results reported the unsupported schema. The old
resume command instead reported no session because its `HasSession` masks the
schema-read failure. The new format cannot improve an old binary's diagnostic.

An intentionally incomplete registered fixture with no initialized version
store also refused all operations and changed no project/control files, but
old fork created an empty version database and lock before reading the session
tree. This remains a recorded limitation for that layout; the initialized-fixture
result must not be generalized to it. No events or worker/provider work ran.

The accepted downgrade guarantee distinguishes these layouts: every supported
layout rejects the new database before model calls or task execution, without
new runtime events or changes to project content. An initialized version store
additionally guarantees no file changes. An uninitialized layout may acquire the
old binary's empty version database and lock; it has no blanket zero-file-change
guarantee. This limitation was accepted on 2026-10-29 and does not authorize
running an old binary on a user's upgraded workspace.

The local probe uses temporary fixture data and does not require Git history or
an old binary in CI. CI verifies old migration checksum rules and byte-preserving
schema-3/4 reads. These storage tests do not substitute for WP3's envelope,
recipient and context-delivery integration or its final compatibility audit.
