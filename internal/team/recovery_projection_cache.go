package team

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"sync"
)

// Cache pure historical projections, never admission or a current task/lease.
// Every byte of the actual input (including payloads, not just claimed event
// hashes) participates in the key. Returned objects are independently decoded.
type recoveryProjectionCache struct {
	mu      sync.Mutex
	entries map[string][]byte
	bytes   int
}

func (cache *recoveryProjectionCache) resolve(ctx context.Context, kind string, events []RunEvent, terminal *RunEvent, compute func() (any, error)) ([]byte, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	hash := sha256.New()
	if err := json.MarshalWrite(hash, struct {
		Kind     string `json:"kind"`
		Count    int    `json:"count"`
		Terminal bool   `json:"terminal"`
	}{kind, len(events), terminal != nil}, json.Deterministic(true)); err != nil {
		return nil, err
	}
	writeEvent := func(event RunEvent) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		payload := event.Payload
		event.Payload = nil
		if err := json.MarshalWrite(hash, event, json.Deterministic(true)); err != nil {
			return err
		}
		_, _ = hash.Write(binary.BigEndian.AppendUint64(nil, uint64(len(payload))))
		_, _ = hash.Write(payload)
		return nil
	}
	for _, event := range events {
		if err := writeEvent(event); err != nil {
			return nil, err
		}
	}
	if terminal != nil {
		if err := writeEvent(*terminal); err != nil {
			return nil, err
		}
	}
	key := hex.EncodeToString(hash.Sum(nil))
	cache.mu.Lock()
	value, found := cache.entries[key]
	cache.mu.Unlock()
	if found {
		return value, context.Cause(ctx)
	}
	projection, err := compute()
	if err != nil {
		return nil, err
	}
	value, err = json.Marshal(projection, json.Deterministic(true), json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true), collectionDurationFormat)
	if err != nil || context.Cause(ctx) != nil {
		if err == nil {
			err = context.Cause(ctx)
		}
		return nil, err
	}
	const budget = 4 << 20
	if len(value) > budget {
		return value, nil
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if previous, exists := cache.entries[key]; exists {
		return previous, nil
	}
	if len(cache.entries) >= 16 || cache.bytes+len(value) > budget {
		clear(cache.entries)
		cache.bytes = 0
	}
	if cache.entries == nil {
		cache.entries = make(map[string][]byte)
	}
	cache.entries[key] = value
	cache.bytes += len(value)
	return value, nil
}

func (c *Coordinator) cachedExecutionPolicyProjection(ctx context.Context, events []RunEvent) (*ExecutionPolicySnapshot, error) {
	last := -1
	for i, event := range events {
		if executionPolicyProjectionEvent(event) {
			last = i
		}
	}
	if last < 0 {
		return nil, context.Cause(ctx)
	}
	// Non-policy events after the last policy boundary cannot alter this pure
	// projection. Recovery events still bind their complete preceding history.
	events = events[:last+1]
	data, err := c.recoveryProjectionCache.resolve(ctx, "execution_policy", events, nil, func() (any, error) {
		return executionPolicySnapshotFromEvents(events)
	})
	if err != nil {
		return nil, err
	}
	var snapshot *ExecutionPolicySnapshot
	err = json.Unmarshal(data, &snapshot)
	return snapshot, err
}

func (c *Coordinator) observationRecoveryPlan(ctx context.Context, event RunEvent, preceding []RunEvent) (*ContractRecoveryPlan, error) {
	data, err := c.recoveryProjectionCache.resolve(ctx, "observation_recovery", preceding, &event, func() (any, error) {
		admitted, err := executionPolicySnapshotFromEvents(preceding)
		if err != nil {
			return nil, err
		}
		return validateContractRecoveryLineage(event, preceding, admitted)
	})
	if err != nil {
		return nil, err
	}
	var plan ContractRecoveryPlan
	err = json.Unmarshal(data, &plan, collectionDurationFormat)
	return &plan, err
}
