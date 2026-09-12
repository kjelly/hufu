package team

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/executioncompat"
)

const executionCompatibilityObservationSchemaVersion = 1

// ExecutionCompatibilityObservedPayload is intentionally a content-free
// local-telemetry record. Feature counts say which deprecated surfaces remain
// actionable without persisting task IDs, event IDs, model/provider values,
// workspace paths, prompts, or outputs.
type ExecutionCompatibilityObservedPayload struct {
	SchemaVersion int                             `json:"schema_version"`
	Counts        map[executioncompat.Feature]int `json:"counts"`
}

// ExecutionCompatibilityObserver holds a read-only compatibility preflight
// result until a coordinator owns the run-scoped EventStore. It is safe to
// reuse for continuation runs: each durable append is independently scoped by
// the current run and branch idempotency key.
type ExecutionCompatibilityObserver struct {
	counts map[executioncompat.Feature]int
	mu     sync.RWMutex
}

// NewExecutionCompatibilityObserver converts an inspector result into the
// smallest telemetry shape required by the runtime. Only unresolved subjects
// are observed: a verified materialization remains historical evidence, but
// does not repeatedly warn an operator to apply the same migration.
func NewExecutionCompatibilityObserver(report *executioncompat.InspectionReport) *ExecutionCompatibilityObserver {
	observer := &ExecutionCompatibilityObserver{counts: make(map[executioncompat.Feature]int)}
	if report == nil {
		return observer
	}
	for _, finding := range report.Findings {
		switch finding.Classification {
		case executioncompat.ClassificationMigratable, executioncompat.ClassificationAmbiguous, executioncompat.ClassificationUnmigratable:
		default:
			continue
		}
		for _, feature := range finding.Features {
			if isExecutionCompatibilityFeature(feature) {
				observer.counts[feature]++
			}
		}
	}
	return observer
}

// HasActionableState reports whether the observer should emit a user-facing
// warning and its later run-scoped metadata event.
func (o *ExecutionCompatibilityObserver) HasActionableState() bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.counts) > 0
}

// Payload returns a defensive, deterministic-compatible copy. encoding/json
// sorts map keys, so the durable payload is stable without retaining source
// identifiers from the inspector report.
func (o *ExecutionCompatibilityObserver) Payload() ExecutionCompatibilityObservedPayload {
	payload := ExecutionCompatibilityObservedPayload{
		SchemaVersion: executionCompatibilityObservationSchemaVersion,
		Counts:        make(map[executioncompat.Feature]int),
	}
	if o == nil {
		return payload
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	for feature, count := range o.counts {
		payload.Counts[feature] = count
	}
	return payload
}

// SetExecutionCompatibilityObserver installs a read-only preflight result.
// It does not append an event: the append must wait until a public invocation
// has allocated its durable run and branch identities.
func (c *Coordinator) SetExecutionCompatibilityObserver(observer *ExecutionCompatibilityObserver) {
	if c == nil {
		return
	}
	c.executionCompatibilityMu.Lock()
	defer c.executionCompatibilityMu.Unlock()
	c.executionCompatibilityObserver = observer
}

func (c *Coordinator) flushExecutionCompatibilityObservation(ctx context.Context) {
	if c == nil {
		return
	}
	c.executionCompatibilityMu.Lock()
	observer := c.executionCompatibilityObserver
	c.executionCompatibilityMu.Unlock()
	if !observer.HasActionableState() || c.EventStore() == nil {
		return
	}

	c.executionEventsMu.RLock()
	runID := strings.TrimSpace(c.executionRunID)
	c.executionEventsMu.RUnlock()
	if runID == "" {
		return
	}
	branchID := strings.TrimSpace(c.activeBranchID())
	if branchID == "" {
		branchID = "main"
	}
	payload := observer.Payload()
	if err := validateExecutionCompatibilityObservedPayload(payload); err != nil {
		log.Printf("warning: execution compatibility telemetry payload rejected: %v", err)
		c.dualWriteFailures.Add(1)
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		log.Printf("warning: marshal execution compatibility telemetry: %v", err)
		c.dualWriteFailures.Add(1)
		return
	}
	key := fmt.Sprintf("execution-compatibility-observed:v%d:%s:%s", payload.SchemaVersion, runID, branchID)
	if _, err := c.EventJournal().Append(ctx, RunEvent{
		Type: string(EventExecutionCompatibilityObserved), Actor: "coordinator",
		IdempotencyKey: key, Payload: encoded,
	}); err != nil {
		// This observation is explicitly best-effort telemetry. Do not make a
		// durable compatibility warning turn into a task-admission failure.
		log.Printf("warning: append execution compatibility telemetry: %v", err)
		c.dualWriteFailures.Add(1)
	}
}
