package team

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
)

// EventRecoveryChangeObserved records what one finished attempt changed
// relative to the previous attempt of the same task occurrence. It is an
// operator diagnostic for retries that changed nothing; no dispatch, retry,
// replay, stop, or no-progress decision reads it.
const EventRecoveryChangeObserved EventType = "recovery_change_observed"

// RecoveryChangeSchemaVersion is the payload schema of recovery_change_observed.
const RecoveryChangeSchemaVersion = 1

// RecoveryChangeObservation is the content-free payload: identities, enum
// states, and digests, never goals, arguments, prose, or model output.
type RecoveryChangeObservation struct {
	SchemaVersion     int                     `json:"schema_version"`
	OccurrenceID      string                  `json:"occurrence_id"`
	RunID             string                  `json:"run_id"`
	TaskID            string                  `json:"task_id"`
	Attempt           int                     `json:"attempt"`
	ModelExecutionID  string                  `json:"model_execution_id"`
	PreviousAttempt   int                     `json:"previous_attempt,omitempty"`
	Comparison        RecoveryComparison      `json:"comparison"`
	Reason            string                  `json:"reason,omitempty"`
	ChangedDimensions []RecoveryDimension     `json:"changed_dimensions,omitempty"`
	UnknownDimensions []RecoveryDimension     `json:"unknown_dimensions,omitempty"`
	NotTracked        []string                `json:"not_tracked"`
	Dimensions        RecoveryChangeSignature `json:"dimensions"`
	// PreviousFailureFingerprint and HypothesisStrategy are shown next to the
	// comparison for reference; neither affects it.
	PreviousFailureFingerprint string `json:"previous_failure_fingerprint,omitempty"`
	HypothesisStrategy         string `json:"hypothesis_strategy,omitempty"`
}

// recoveryChangeLog remembers, per occurrence, the latest observed attempt and
// which attempt identities were already observed. It is rebuilt from the
// event log on first use, so a resumed run compares with the attempt before
// the crash.
type recoveryChangeLog struct {
	mu       sync.Mutex
	loaded   bool
	last     map[string]RecoveryChangeObservation
	observed map[string]bool
	// unchanged counts no_structural_change observations by run ID.
	unchanged map[string]int
}

// retriesWithoutStructuralChange counts this run's attempts that repeated the
// previous attempt's inputs.
func (c *Coordinator) retriesWithoutStructuralChange() int {
	if c == nil {
		return 0
	}
	c.recoveryChanges.mu.Lock()
	defer c.recoveryChanges.mu.Unlock()
	return c.recoveryChanges.unchanged[c.executionRunID]
}

func (l *recoveryChangeLog) record(key string, observation RecoveryChangeObservation) {
	l.last[observation.OccurrenceID] = observation
	l.observed[key] = true
	if observation.Comparison == RecoveryNoStructuralChange {
		l.unchanged[observation.RunID]++
	}
}

// setAttemptReceipt is the coordinator's single path for persisting an
// attempt receipt. Once the attempt has finished, it records how the attempt
// differs from the occurrence's previous one.
func (c *Coordinator) setAttemptReceipt(todoID string, receipt *ExecutionReceipt) error {
	if err := c.taskTracker.TodoList().SetExecutionReceipt(todoID, receipt); err != nil {
		return err
	}
	if receipt != nil && !receipt.FinishedAt.IsZero() {
		c.observeRecoveryChange(todoID, *receipt)
		c.observeExecutedStrategyChange(todoID, *receipt)
	}
	return nil
}

// recoveryOccurrenceID names a task occurrence by the run of its first
// attempt, which stays fixed across resume and across later runs that reuse
// the same todo ID.
func recoveryOccurrenceID(item *TodoItem, receipt ExecutionReceipt) string {
	runID := receipt.RunID
	if len(item.ExecutionReceipts) > 0 && item.ExecutionReceipts[0].RunID != "" {
		runID = item.ExecutionReceipts[0].RunID
	}
	return runID + "/" + item.ID
}

func recoveryAttemptKey(occurrence string, receipt ExecutionReceipt) string {
	return strings.Join([]string{occurrence, receipt.RunID, fmt.Sprint(receipt.Attempt), receipt.ModelExecutionID}, "\x1f")
}

// observeRecoveryChange records the observation for one finished attempt at
// most once. It is best effort: a failed append only logs.
func (c *Coordinator) observeRecoveryChange(todoID string, receipt ExecutionReceipt) {
	if c == nil || !c.hasDurableEventJournal() {
		return
	}
	item := c.todoItemByID(todoID)
	if item == nil {
		return
	}
	occurrence := recoveryOccurrenceID(item, receipt)
	key := recoveryAttemptKey(occurrence, receipt)
	signature := recoveryChangeSignature(item, receipt)

	c.recoveryChanges.mu.Lock()
	defer c.recoveryChanges.mu.Unlock()
	c.loadRecoveryChangesLocked()
	if c.recoveryChanges.observed[key] {
		return
	}
	observation := RecoveryChangeObservation{
		SchemaVersion: RecoveryChangeSchemaVersion, OccurrenceID: occurrence, RunID: receipt.RunID, TaskID: item.ID,
		Attempt: receipt.Attempt, ModelExecutionID: receipt.ModelExecutionID, NotTracked: recoveryNotTracked, Dimensions: signature,
	}
	if previous, ok := c.recoveryChanges.last[occurrence]; ok {
		observation.PreviousAttempt = previous.Attempt
		observation.Comparison, observation.ChangedDimensions, observation.UnknownDimensions = compareRecoverySignatures(previous.Dimensions, signature)
	} else {
		observation.Comparison, observation.Reason = RecoveryComparisonUnknown, recoveryReasonNoPriorAttempt
	}
	if n := len(item.FailureFingerprints); n > 0 {
		observation.PreviousFailureFingerprint = item.FailureFingerprints[n-1].Digest
	}
	if item.RecoveryHypothesis != nil {
		observation.HypothesisStrategy = string(item.RecoveryHypothesis.Strategy)
	}
	payload, err := json.Marshal(observation)
	if err != nil {
		log.Printf("warning: encode recovery change observation: %v", err)
		return
	}
	event := RunEvent{Type: string(EventRecoveryChangeObserved), Actor: "hufu", TaskID: item.ID, Attempt: receipt.Attempt, Payload: payload}
	if _, err := c.emitEventOnce("recovery:change:"+hashContentKey(key), event); err != nil {
		log.Printf("warning: persist recovery change observation: %v", err)
		return
	}
	c.recoveryChanges.record(key, observation)
}

// loadRecoveryChangesLocked rebuilds the per-occurrence state from durable
// observations once per coordinator.
func (c *Coordinator) loadRecoveryChangesLocked() {
	if c.recoveryChanges.loaded {
		return
	}
	c.recoveryChanges.loaded = true
	c.recoveryChanges.last = map[string]RecoveryChangeObservation{}
	c.recoveryChanges.observed = map[string]bool{}
	c.recoveryChanges.unchanged = map[string]int{}
	if c.eventStore == nil {
		return
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		log.Printf("warning: read recovery change observations: %v", err)
		return
	}
	for _, observation := range RecoveryChangeObservations(events) {
		c.recoveryChanges.record(recoveryAttemptKey(observation.OccurrenceID, ExecutionReceipt{RunID: observation.RunID, Attempt: observation.Attempt, ModelExecutionID: observation.ModelExecutionID}), observation)
	}
}

// RecoveryChangeObservations decodes every recovery_change_observed event in
// log order, skipping payloads it cannot read.
func RecoveryChangeObservations(events []RunEvent) []RecoveryChangeObservation {
	var out []RecoveryChangeObservation
	for _, event := range events {
		if event.Type != string(EventRecoveryChangeObserved) {
			continue
		}
		var observation RecoveryChangeObservation
		if json.Unmarshal(event.Payload, &observation) == nil && observation.SchemaVersion == RecoveryChangeSchemaVersion {
			out = append(out, observation)
		}
	}
	return out
}

func validateRecoveryChangeEvent(event RunEvent) error {
	var observation RecoveryChangeObservation
	if err := json.Unmarshal(event.Payload, &observation); err != nil {
		return fmt.Errorf("decode %s payload: %w", event.Type, err)
	}
	if observation.SchemaVersion != RecoveryChangeSchemaVersion {
		return fmt.Errorf("%s has unsupported schema_version %d", event.Type, observation.SchemaVersion)
	}
	if strings.TrimSpace(observation.OccurrenceID) == "" || strings.TrimSpace(observation.TaskID) == "" {
		return fmt.Errorf("%s requires occurrence and task identities", event.Type)
	}
	switch observation.Comparison {
	case RecoveryChangeDetected, RecoveryNoStructuralChange, RecoveryComparisonUnknown:
	default:
		return fmt.Errorf("%s has invalid comparison %q", event.Type, observation.Comparison)
	}
	return nil
}
