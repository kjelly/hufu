# corrupt-hash-chain

Producer: fixed legacy fixture builder v1, which first used
`EventStore.AppendPersisted` and then intentionally corrupted the persisted
payload without recomputing the hash.

Case 14: inspector must hard-error on the broken hash chain and must leave the
workspace untouched.
