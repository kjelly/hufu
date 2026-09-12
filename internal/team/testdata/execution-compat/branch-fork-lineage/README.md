# branch-fork-lineage

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
and `SaveSessionTree` to create a branch fork point.

Case 12: the inherited legacy occurrence remains independently migratable on
both main and child branches, while the child-only typed task is canonical.
