package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

type memoryOutcomePayload struct {
	memoryEventPayload
	Signal                string  `json:"signal"`
	Disposition           string  `json:"disposition,omitempty"`
	AttributionConfidence float64 `json:"attribution_confidence,omitempty"`
	CausalConfidence      float64 `json:"causal_confidence,omitempty"`
	EvidenceWeight        float64 `json:"evidence_weight,omitempty"`
	EffectiveWeight       float64 `json:"effective_weight,omitempty"`
	Direction             string  `json:"direction,omitempty"`
}

func (c *Coordinator) reduceMemoryEvent(event RunEvent) {
	repo, ok := c.contextRepo.(contextstore.ExperienceRepository)
	if !ok || event.IdempotencyKey == "" {
		return
	}
	observation, ok := memoryObservationFromEvent(event, c.session.Config.MemoryLearning)
	if !ok {
		return
	}
	if _, err := repo.ApplyExperienceObservation(context.Background(), observation); err != nil {
		c.recordLearningGap(RunEvent{Type: "memory_aggregate_repair", TaskID: event.TaskID, IdempotencyKey: event.IdempotencyKey}, err)
	}
}

func memoryObservationFromEvent(event RunEvent, policy agent.MemoryLearningPolicy) (contextstore.ExperienceObservation, bool) {
	var base memoryEventPayload
	if err := json.Unmarshal(event.Payload, &base); err != nil || base.ContextItemID == "" {
		return contextstore.ExperienceObservation{}, false
	}
	if base.PolicyVersion == "" {
		base.PolicyVersion = policy.PolicyVersion
	}
	observed, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return contextstore.ExperienceObservation{}, false
	}
	observation := contextstore.ExperienceObservation{
		IdempotencyKey: event.IdempotencyKey, ContextItemID: base.ContextItemID,
		PolicyVersion: base.PolicyVersion, TaskID: event.TaskID, ObservedAt: observed,
		ProjectID:  base.ProjectID,
		PriorAlpha: base.PriorAlpha, PriorBeta: base.PriorBeta,
		UtilityPercentile: base.UtilityPercentile,
	}
	if observation.PriorAlpha <= 0 {
		observation.PriorAlpha = policy.PriorAlpha
	}
	if observation.PriorBeta <= 0 {
		observation.PriorBeta = policy.PriorBeta
	}
	if observation.UtilityPercentile <= 0 || observation.UtilityPercentile >= 1 {
		observation.UtilityPercentile = policy.UtilityPercentile
	}
	switch event.Type {
	case "memory_retrieved":
		observation.ExposureDelta = 1
	case "memory_usage_recorded":
		var usage struct {
			Disposition string `json:"disposition"`
		}
		_ = json.Unmarshal(event.Payload, &usage)
		switch usage.Disposition {
		case MemoryUseApplied:
			observation.AppliedDelta = 1
		case MemoryUseConsulted:
			observation.ConsultedDelta = 1
		case MemoryUseRejected:
			observation.RejectedDelta = 1
		}
	case "memory_outcome_recorded":
		var outcome memoryOutcomePayload
		if err := json.Unmarshal(event.Payload, &outcome); err != nil {
			return contextstore.ExperienceObservation{}, false
		}
		switch outcome.Direction {
		case "positive":
			observation.PositiveWeight = outcome.EffectiveWeight
			if outcome.Signal == "verification_passed" {
				observation.VerifiedSupportDelta = 1
			}
		case "negative":
			observation.NegativeWeight = outcome.EffectiveWeight
			if outcome.CausalConfidence > 0 && outcome.EffectiveWeight > 0 {
				observation.CausalFailureDelta = 1
			}
		}
	default:
		return contextstore.ExperienceObservation{}, false
	}
	return observation, true
}

func (c *Coordinator) recordMemoryOutcomeForTask(item *TodoItem, terminalEvent string) {
	c.recordGeneralContextOutcome(item, terminalEvent)
	if c == nil || item == nil || item.TypedResult == nil || len(item.TypedResult.MemoryUses) == 0 || !memoryLearningEnabled(c.session.Config.MemoryLearning) {
		return
	}
	signal, direction, evidenceWeight := "task_outcome_unknown", "", 0.0
	causalConfidence := func(MemoryUseRef) float64 { return 0 }
	if terminalEvent == "task_completed" {
		signal, direction, evidenceWeight = "task_terminal_success", "positive", 0.2
		causalConfidence = func(MemoryUseRef) float64 { return 1 }
		if item.VerifyResult != nil && isVerifySuccess(item.VerifyResult) {
			signal, evidenceWeight = "verification_passed", 1
		}
	} else if item.VerifyResult != nil && !isVerifySuccess(item.VerifyResult) {
		signal, direction, evidenceWeight = "verification_failed", "negative", 1
		causalConfidence = func(use MemoryUseRef) float64 {
			if c.memoryActionMatches(use.ContextItemID, item) {
				return 1
			}
			return 0
		}
	}
	c.recordMemoryOutcomeSignal(item, signal, direction, evidenceWeight, causalConfidence)
	if terminalEvent == "task_completed" && item.Retries > 0 {
		c.recordMemoryOutcomeSignal(item, "retry_rescued", "positive", 0.5, func(MemoryUseRef) float64 { return 1 })
	}
}

func (c *Coordinator) recordGeneralContextOutcome(item *TodoItem, terminalEvent string) {
	if c == nil || item == nil {
		return
	}
	recorder, ok := c.contextRepo.(contextOutcomeRecorder)
	if !ok {
		return
	}
	outcome := "failure"
	if terminalEvent == "task_completed" {
		outcome = "success"
	}
	used := make(map[string]bool)
	if item.TypedResult != nil {
		for _, use := range item.TypedResult.MemoryUses {
			if use.Disposition == MemoryUseApplied || use.Disposition == MemoryUseConsulted {
				used[use.ContextItemID] = true
			}
		}
	}
	for _, manifest := range item.ContextManifests {
		for _, manifestItem := range manifest.Items {
			if !manifestItem.Included || (manifestItem.Source != "shared_persistent" && manifestItem.Source != "shared_session" && manifestItem.Source != "context_get") {
				continue
			}
			verificationOutcome := "not_assessed"
			if item.VerifyResult != nil {
				if isVerifySuccess(item.VerifyResult) {
					verificationOutcome = "passed"
				} else {
					verificationOutcome = "failed"
				}
			}
			base := contextOutcomeObservation(&manifest, manifestItem.ID, outcome, verificationOutcome)
			base.PolicyRevision = c.session.Config.MemoryLearning.PolicyVersion
			// Objective verification is the only authority permitted to emit a
			// verification assessment. A terminal model response without a
			// verifier remains explicitly not_assessed.
			if item.VerifyResult != nil {
				verification := base
				verification.IdempotencyKey, verification.Outcome = manifest.Fingerprint+":"+manifestItem.ID+":verification:"+verificationOutcome, "verification_assessed"
				_, _ = recorder.RecordContextOutcomeObservation(context.Background(), verification)
				if verificationOutcome == "failed" {
					failure := base
					failure.IdempotencyKey, failure.Outcome = manifest.Fingerprint+":"+manifestItem.ID+":failure_attributed", "failure_attributed"
					_, _ = recorder.RecordContextOutcomeObservation(context.Background(), failure)
				}
			}
			if used[manifestItem.ID] {
				useObservation := base
				useObservation.IdempotencyKey, useObservation.Outcome = manifest.Fingerprint+":"+manifestItem.ID+":used", "used"
				_, _ = recorder.RecordContextOutcomeObservation(context.Background(), useObservation)
			}
			base.IdempotencyKey, base.Outcome = manifest.Fingerprint+":"+manifestItem.ID+":"+terminalEvent, outcome
			_, _ = recorder.RecordContextOutcomeObservation(context.Background(), base)
		}
	}
}

func (c *Coordinator) recordMemoryOutcomeSignal(item *TodoItem, signal, direction string, evidenceWeight float64, causalConfidence func(MemoryUseRef) float64) {
	if c == nil || item == nil || item.TypedResult == nil || !memoryLearningEnabled(c.session.Config.MemoryLearning) {
		return
	}
	type attributedUse struct {
		use      MemoryUseRef
		manifest *MemoryInjectionManifest
		causal   float64
		raw      float64
	}
	var attributed []attributedUse
	confidenceSum := 0.0
	for _, use := range item.TypedResult.MemoryUses {
		if use.Disposition == MemoryUseApplied {
			confidenceSum += use.Confidence
		}
	}
	if confidenceSum <= 0 {
		return
	}
	rawTotal := 0.0
	for _, use := range item.TypedResult.MemoryUses {
		if use.Disposition != MemoryUseApplied {
			continue
		}
		manifest := memoryManifestForUse(item, item.TypedResult.Attempt, use.RetrievalID)
		if manifest == nil {
			continue
		}
		causal := 0.0
		if causalConfidence != nil {
			causal = causalConfidence(use)
		}
		raw := evidenceWeight * causal * (use.Confidence / confidenceSum)
		rawTotal += raw
		attributed = append(attributed, attributedUse{use: use, manifest: manifest, causal: causal, raw: raw})
	}
	policy := c.session.Config.MemoryLearning
	remaining := policy.MaxCreditPerSignal - c.memoryOutcomeWeightForSignal(item.ID, signal, direction)
	if remaining < 0 {
		remaining = 0
	}
	scale := 1.0
	if rawTotal > remaining && rawTotal > 0 {
		scale = remaining / rawTotal
	}
	for _, attribution := range attributed {
		use, manifest := attribution.use, attribution.manifest
		effective := attribution.raw * scale
		payload := memoryOutcomePayload{
			memoryEventPayload: memoryEventPayload{SchemaVersion: memoryEventSchemaVersion, RetrievalID: use.RetrievalID, ContextItemID: use.ContextItemID, PolicyVersion: manifest.PolicyVersion, ProjectID: c.contextScope().ProjectID, ReasonCode: signal, PriorAlpha: policy.PriorAlpha, PriorBeta: policy.PriorBeta, UtilityPercentile: policy.UtilityPercentile},
			Signal:             signal, Disposition: use.Disposition, AttributionConfidence: use.Confidence,
			CausalConfidence: attribution.causal, EvidenceWeight: evidenceWeight, EffectiveWeight: effective, Direction: direction,
		}
		raw, _ := json.Marshal(payload)
		key := memorySignalKey("outcome_"+signal, manifest, use.ContextItemID)
		event := RunEvent{Type: "memory_outcome_recorded", Actor: "runtime", TaskID: item.ID, Attempt: manifest.Attempt, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), IdempotencyKey: key, Payload: raw}
		if emitted, err := c.emitEventOnce(key, event); err == nil && emitted {
			c.reduceMemoryEvent(event)
		}
	}
}

// memoryOutcomeWeightForSignal returns the effective credit already recorded
// for one outcome signal on a task. The max-credit-per-signal cap is scoped to
// the signal (spec §5.4: each outcome signal's sum(effective_weight) is capped
// at 1.0), so verification, skeptic, acceptance, and terminal signals each get
// an independent budget instead of sharing a per-direction pool.
func (c *Coordinator) memoryOutcomeWeightForSignal(taskID, signal, direction string) float64 {
	if signal == "" || direction == "" || c == nil || c.eventStore == nil {
		return 0
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return 0
	}
	total := 0.0
	for _, event := range events {
		if event.Type != "memory_outcome_recorded" || event.TaskID != taskID {
			continue
		}
		var payload memoryOutcomePayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Direction == direction && payload.Signal == signal {
			total += payload.EffectiveWeight
		}
	}
	return total
}

func (c *Coordinator) recordMemoryOutcomeSignalForTaskID(taskID, signal, direction string, evidenceWeight float64) {
	if c == nil || c.taskTracker == nil {
		return
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == taskID {
			c.recordMemoryOutcomeSignal(item, signal, direction, evidenceWeight, func(MemoryUseRef) float64 { return 1 })
			return
		}
	}
}

func (c *Coordinator) recordMemoryRunOutcome(signal, direction string, evidenceWeight float64) {
	if c == nil || c.taskTracker == nil {
		return
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.Status == TaskDone {
			c.recordMemoryOutcomeSignal(item, signal, direction, evidenceWeight, func(MemoryUseRef) float64 { return 1 })
		}
	}
}

func (c *Coordinator) recordMemoryRollbackOutcome() {
	if c == nil || c.taskTracker == nil {
		return
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.TypedResult == nil {
			continue
		}
		c.recordMemoryOutcomeSignal(item, "rollback", "negative", 1, func(use MemoryUseRef) float64 {
			if c.memoryActionMatches(use.ContextItemID, item) {
				return 1
			}
			return 0
		})
	}
}

func memoryManifestForUse(item *TodoItem, attempt int, retrievalID string) *MemoryInjectionManifest {
	for i := range item.MemoryManifests {
		manifest := &item.MemoryManifests[i]
		if manifest.RetrievalID == retrievalID && (attempt <= 0 || manifest.Attempt == attempt) {
			return manifest
		}
	}
	return nil
}

func (c *Coordinator) memoryActionMatches(contextItemID string, item *TodoItem) bool {
	record, err := c.contextRepo.Get(context.Background(), contextItemID)
	if err != nil || record.Lifecycle != contextstore.LifecycleConfirmed || !isProceduralMemory(record) || record.Metadata["action_fingerprint"] == "" {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(record.Metadata["action_fingerprint"]))
	for _, command := range item.TypedResult.Commands {
		normalized := strings.Join(strings.Fields(command.Command), " ")
		sum := sha256.Sum256([]byte(normalized))
		if want == hex.EncodeToString(sum[:]) {
			return true
		}
	}
	for _, receipt := range c.executionStepReceiptRegistry().claimedReceiptsInOrder(item.TypedResult.ReceiptIDs) {
		if want == strings.ToLower(receipt.InputSHA256) {
			return true
		}
	}
	return false
}

func isProceduralMemory(item contextstore.ContextItem) bool {
	switch item.Kind {
	case contextstore.ContextPattern, contextstore.ContextInstruction, contextstore.ContextConvention:
		return true
	default:
		return item.Metadata["memory_type"] == "procedural"
	}
}

func (c *Coordinator) RebuildExperienceAggregates(ctx context.Context) error {
	repo, ok := c.contextRepo.(contextstore.ExperienceRepository)
	if !ok || c.eventStore == nil {
		return fmt.Errorf("experience aggregate repository or event store unavailable")
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return err
	}
	policy := c.session.Config.MemoryLearning
	observations := make([]contextstore.ExperienceObservation, 0)
	for _, event := range events {
		observation, include := memoryObservationFromEvent(event, policy)
		if include {
			observations = append(observations, observation)
		}
	}
	return repo.RebuildExperienceAggregates(ctx, observations)
}

// ExperienceObservationsFromEvents converts the append-only memory ledger to
// deterministic reducer inputs for maintenance commands and tests.
func ExperienceObservationsFromEvents(events []RunEvent, policy agent.MemoryLearningPolicy) []contextstore.ExperienceObservation {
	observations := make([]contextstore.ExperienceObservation, 0)
	for _, event := range events {
		observation, include := memoryObservationFromEvent(event, policy)
		if include {
			observations = append(observations, observation)
		}
	}
	return observations
}
