# unknown-unrelated-event

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
with a forward-compatible unknown event envelope.

Case 16: inspector and apply ignore the unknown event for compatibility
classification while canonical replay preserves normal forward compatibility.
