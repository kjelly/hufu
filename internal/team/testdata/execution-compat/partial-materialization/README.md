# partial-materialization

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
and one prior append-only compatibility migration before a second legacy task.

Case 15: an existing canonical migration stays valid while a later legacy
subject is migrated on an idempotent rerun.
