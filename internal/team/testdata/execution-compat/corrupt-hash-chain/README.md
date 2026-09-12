# corrupt-hash-chain

Producer: fixed legacy fixture builder v1, which first used
`EventStore.AppendPersisted` and then intentionally corrupted the first byte.

Case 14: inspector must hard-error on the broken JSONL/hash-chain input and
must leave the workspace untouched.
