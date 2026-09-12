# policy-v3-route

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
with a validated v3 execution-policy snapshot.

Case 11: a v3 route's `legacy_provider` and `local` backend must materialize
to a self-contained v4 canonical policy snapshot without live configuration.
