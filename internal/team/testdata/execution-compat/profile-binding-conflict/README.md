# profile-binding-conflict

Producer: fixed legacy fixture builder v1, using `EventStore.AppendPersisted`
with historical task and `model_profile_resolved` payloads.

Case 8: the task binding and its matching same-run profile disagree. The
inspector must classify the task as ambiguous and apply must append nothing.
