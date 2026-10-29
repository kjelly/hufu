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

The event store must validate the complete grant envelope and write its event
and reference in the same transaction. The low-level reference helper checks
that the event type, branch, cycle/run, ID and hash match. Exact admission replay
is idempotent; a different source cannot replace an existing cycle reference.
Reading an index with a missing or changed source fails closed.

The higher-level envelope, recipient authorization, CAS presentation, task
binding and model-context delivery remain WP3 implementation work. This storage
foundation alone does not enable `--inherit-result` or grant any worker access.

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
