# provider-session-bound

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
with historical `provider_session_bound` payloads.

Case 5: a qualified task is disambiguated only by its durable legacy session
binding. The materializer must append a canonical `BackendBinding` carrying
the same session identity.
