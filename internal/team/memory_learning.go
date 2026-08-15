package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

const memoryEventSchemaVersion = 1

var memoryReasonCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_:-]{0,63}$`)

type MemoryScoreParts struct {
	BaseRelevance           float64 `json:"base_relevance"`
	Applicability           float64 `json:"applicability,omitempty"`
	UtilityLowerBound       float64 `json:"utility_lower_bound,omitempty"`
	Freshness               float64 `json:"freshness,omitempty"`
	TrustFactor             float64 `json:"trust_factor,omitempty"`
	HarmfulUsePenalty       float64 `json:"harmful_use_penalty,omitempty"`
	StaleEnvironmentPenalty float64 `json:"stale_environment_penalty,omitempty"`
}

func (c *Coordinator) validateMemoryUseClaims(ctx context.Context, taskID string, result *TaskResult) error {
	if result == nil || len(result.MemoryUses) == 0 {
		return nil
	}
	if result.Source == "parsed_free_text" {
		return fmt.Errorf("free-text result cannot claim applied memory")
	}
	if c == nil || c.taskTracker == nil || c.contextRepo == nil {
		return fmt.Errorf("memory attribution requires an active canonical context repository")
	}
	if result.TaskID != "" && result.TaskID != taskID {
		return fmt.Errorf("memory attribution cannot cross task %q to %q", result.TaskID, taskID)
	}
	attempt := result.Attempt
	if attempt <= 0 {
		attempt = c.currentTaskAttempt(taskID)
	}
	var manifest *MemoryInjectionManifest
	var taskAgent string
	for _, task := range c.taskTracker.TodoList().Items() {
		if task == nil || task.ID != taskID {
			continue
		}
		taskAgent = task.Agent
		for i := range task.MemoryManifests {
			candidate := &task.MemoryManifests[i]
			if candidate.RunID == c.executionRunID && candidate.Attempt == attempt {
				manifest = candidate
				break
			}
		}
		break
	}
	if manifest == nil {
		return fmt.Errorf("memory attribution has no manifest for task %q attempt %d", taskID, attempt)
	}
	// Identity is runtime-owned. submit_result does not expose an attempt field,
	// so seal the resolved current attempt onto the durable typed result before
	// usage events and receipts are produced.
	result.TaskID = taskID
	result.Attempt = attempt
	if manifest.TaskID != taskID || manifest.Attempt != attempt || (taskAgent != "" && manifest.Agent != taskAgent) {
		return fmt.Errorf("memory attribution manifest identity mismatch")
	}
	allowed := make(map[string]MemoryInjectionItem, len(manifest.Items))
	for _, item := range manifest.Items {
		allowed[item.ContextItemID] = item
	}
	seen := make(map[string]bool, len(result.MemoryUses))
	ids := make([]string, 0, len(result.MemoryUses))
	for i, use := range result.MemoryUses {
		if use.RetrievalID != manifest.RetrievalID {
			return fmt.Errorf("memory_uses[%d] retrieval_id is not valid for task %q attempt %d", i, taskID, attempt)
		}
		if _, ok := allowed[use.ContextItemID]; !ok {
			return fmt.Errorf("memory_uses[%d] context item %q was not injected", i, use.ContextItemID)
		}
		if seen[use.ContextItemID] {
			return fmt.Errorf("memory_uses contains duplicate context item %q", use.ContextItemID)
		}
		seen[use.ContextItemID] = true
		switch use.Disposition {
		case MemoryUseApplied, MemoryUseConsulted, MemoryUseRejected:
		default:
			return fmt.Errorf("memory_uses[%d] has invalid disposition %q", i, use.Disposition)
		}
		if use.Confidence < 0 || use.Confidence > 1 {
			return fmt.Errorf("memory_uses[%d] confidence must be in [0,1]", i)
		}
		if use.ReasonCode != "" && !memoryReasonCodePattern.MatchString(use.ReasonCode) {
			return fmt.Errorf("memory_uses[%d] reason_code must be a bounded opaque code", i)
		}
		ids = append(ids, use.ContextItemID)
	}
	records, err := c.contextRepo.GetMany(ctx, ids)
	if err != nil {
		return fmt.Errorf("validate memory lifecycle: %w", err)
	}
	byID := make(map[string]contextstore.ContextItem, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	now := time.Now().UTC()
	for _, id := range ids {
		record, ok := byID[id]
		if !ok || record.Lifecycle != contextstore.LifecycleConfirmed || record.SupersededBy != "" || (record.ExpiresAt != nil && !now.Before(*record.ExpiresAt)) {
			return fmt.Errorf("memory context item %q is no longer eligible for attribution", id)
		}
	}
	return nil
}

func (c *Coordinator) emitMemoryUsageEvents(result *TaskResult) {
	if c == nil || result == nil || len(result.MemoryUses) == 0 {
		return
	}
	attempt := result.Attempt
	for _, use := range result.MemoryUses {
		var manifest *MemoryInjectionManifest
		for _, task := range c.taskTracker.TodoList().Items() {
			if task == nil || task.ID != result.TaskID {
				continue
			}
			for i := range task.MemoryManifests {
				if task.MemoryManifests[i].Attempt == attempt && task.MemoryManifests[i].RetrievalID == use.RetrievalID {
					manifest = &task.MemoryManifests[i]
					break
				}
			}
		}
		if manifest == nil {
			continue
		}
		policy := c.session.Config.MemoryLearning
		payload := memoryEventPayload{SchemaVersion: memoryEventSchemaVersion, RetrievalID: use.RetrievalID, ContextItemID: use.ContextItemID, PolicyVersion: manifest.PolicyVersion, ProjectID: c.contextScope().ProjectID, ReasonCode: use.ReasonCode, PriorAlpha: policy.PriorAlpha, PriorBeta: policy.PriorBeta, UtilityPercentile: policy.UtilityPercentile}
		raw, _ := json.Marshal(struct {
			memoryEventPayload
			Disposition string  `json:"disposition"`
			Confidence  float64 `json:"confidence"`
		}{payload, use.Disposition, use.Confidence})
		key := memorySignalKey("usage", manifest, use.ContextItemID)
		event := RunEvent{Type: "memory_usage_recorded", Actor: manifest.Agent, TaskID: result.TaskID, Attempt: attempt, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), IdempotencyKey: key, Payload: raw}
		if emitted, err := c.emitEventOnce(key, event); err == nil && emitted {
			c.reduceMemoryEvent(event)
		}
	}
}

type MemoryInjectionItem struct {
	ContextItemID string           `json:"context_item_id"`
	Source        string           `json:"source"`
	Rank          int              `json:"rank"`
	TokenCount    int              `json:"token_count,omitempty"`
	BaseScore     float64          `json:"base_score"`
	FinalScore    float64          `json:"final_score"`
	ScoreParts    MemoryScoreParts `json:"score_parts"`
}

type MemoryInjectionManifest struct {
	RetrievalID   string `json:"retrieval_id"`
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	Attempt       int    `json:"attempt"`
	Agent         string `json:"agent"`
	PolicyVersion string `json:"policy_version"`
	// QueryHash is the privacy-safe identity of the retrieval query that
	// selected the injected items. It binds the retrieval ID to the exact
	// policy/query that produced the prompt, so explain-memory can return a
	// retrieval ID only when it matches the request being explained (spec §5.1,
	// §7 HF-MEM4-005). Older manifests omit it.
	QueryHash   string                `json:"query_hash,omitempty"`
	Items       []MemoryInjectionItem `json:"items"`
	Fingerprint string                `json:"fingerprint"`
	CreatedAt   time.Time             `json:"created_at"`
}

type MemoryLearningReport struct {
	Mode              agent.MemoryLearningMode `json:"mode"`
	PolicyVersion     string                   `json:"policy_version"`
	RetrievalCount    int                      `json:"retrieval_count"`
	ExposureCount     int                      `json:"exposure_count"`
	AppliedCount      int                      `json:"applied_count"`
	OutcomeCount      int                      `json:"outcome_count"`
	PendingRepairGaps int                      `json:"pending_repair_gaps"`
}

func (c *Coordinator) MemoryLearningReport() MemoryLearningReport {
	var report MemoryLearningReport
	if c == nil || c.session == nil {
		return report
	}
	report.Mode = c.session.Config.MemoryLearning.Mode
	report.PolicyVersion = c.session.Config.MemoryLearning.PolicyVersion
	if c.sessionData != nil {
		for _, gap := range c.sessionData.LearningGaps {
			if gap.PendingRepair {
				report.PendingRepairGaps++
			}
		}
	}
	if c.eventStore == nil {
		return report
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return report
	}
	retrievals := make(map[string]struct{})
	for _, event := range events {
		var payload memoryEventPayload
		_ = json.Unmarshal(event.Payload, &payload)
		switch event.Type {
		case "memory_retrieved":
			report.ExposureCount++
			if payload.RetrievalID != "" {
				retrievals[payload.RetrievalID] = struct{}{}
			}
		case "memory_usage_recorded":
			var usage struct {
				Disposition string `json:"disposition"`
			}
			_ = json.Unmarshal(event.Payload, &usage)
			if usage.Disposition == MemoryUseApplied {
				report.AppliedCount++
			}
		case "memory_outcome_recorded":
			report.OutcomeCount++
		}
	}
	report.RetrievalCount = len(retrievals)
	return report
}

type memoryEventPayload struct {
	SchemaVersion     int     `json:"schema_version"`
	RetrievalID       string  `json:"retrieval_id,omitempty"`
	ContextItemID     string  `json:"context_item_id,omitempty"`
	PolicyVersion     string  `json:"policy_version,omitempty"`
	ProjectID         string  `json:"project_id,omitempty"`
	ReasonCode        string  `json:"reason_code,omitempty"`
	EvidenceRef       string  `json:"evidence_ref,omitempty"`
	Source            string  `json:"source,omitempty"`
	Rank              int     `json:"rank,omitempty"`
	BaseScore         float64 `json:"base_score,omitempty"`
	FinalScore        float64 `json:"final_score,omitempty"`
	Fingerprint       string  `json:"fingerprint,omitempty"`
	TokenCount        int     `json:"token_count,omitempty"`
	PriorAlpha        float64 `json:"prior_alpha,omitempty"`
	PriorBeta         float64 `json:"prior_beta,omitempty"`
	UtilityPercentile float64 `json:"utility_percentile,omitempty"`
}

func memoryLearningEnabled(policy agent.MemoryLearningPolicy) bool {
	switch policy.Mode {
	case agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive:
		return true
	default:
		return false
	}
}

func buildMemoryInjectionManifest(compiled CompiledContext, runID, taskID string, attempt int, agentName, query string, policy agent.MemoryLearningPolicy) *MemoryInjectionManifest {
	return buildMemoryInjectionManifestFromContextManifest(compiled, nil, runID, taskID, attempt, agentName, query, policy)
}

// buildMemoryInjectionManifestFromContextManifest preserves the legacy
// learning/outcome contract while making the general manifest the sole owner
// of selection and ordering. Compiled items only enrich the selected IDs with
// score parts required by existing attribution; they cannot add an item.
func buildMemoryInjectionManifestFromContextManifest(compiled CompiledContext, general *ContextInjectionManifest, runID, taskID string, attempt int, agentName, query string, policy agent.MemoryLearningPolicy) *MemoryInjectionManifest {
	if !memoryLearningEnabled(policy) {
		return nil
	}
	compiledByID := make(map[string]ContextItem)
	for _, included := range compiled.IncludedItems {
		if strings.HasPrefix(included.ID, "context:") {
			compiledByID[strings.TrimPrefix(included.ID, "context:")] = included
		}
	}
	selectedIDs := make([]string, 0, len(compiledByID))
	if general != nil {
		for _, item := range general.Items {
			if item.Included {
				if _, ok := compiledByID[item.ID]; ok {
					selectedIDs = append(selectedIDs, item.ID)
				}
			}
		}
	} else {
		for _, included := range compiled.IncludedItems {
			if strings.HasPrefix(included.ID, "context:") {
				selectedIDs = append(selectedIDs, strings.TrimPrefix(included.ID, "context:"))
			}
		}
	}
	items := make([]MemoryInjectionItem, 0)
	seen := make(map[string]bool)
	for _, contextItemID := range selectedIDs {
		if seen[contextItemID] {
			continue
		}
		seen[contextItemID] = true
		included := compiledByID[contextItemID]
		base := included.BaseScore
		if base == 0 {
			base = included.Confidence
		}
		final := included.FinalScore
		if final == 0 {
			final = base
		}
		parts := included.ScoreParts
		if parts.BaseRelevance == 0 {
			parts.BaseRelevance = base
		}
		tokenCount := included.TokenCount
		if tokenCount <= 0 {
			tokenCount = max(1, len([]rune(included.Content))/4)
		}
		items = append(items, MemoryInjectionItem{
			ContextItemID: contextItemID, Source: included.Source,
			Rank: len(items) + 1, TokenCount: tokenCount, BaseScore: base, FinalScore: final, ScoreParts: parts,
		})
	}
	if len(items) == 0 {
		return nil
	}
	orderedIDs := make([]string, len(items))
	for i := range items {
		orderedIDs[i] = items[i].ContextItemID
	}
	identity := strings.Join([]string{runID, taskID, agentName, policy.PolicyVersion, strings.Join(orderedIDs, "\x00")}, "\x1f")
	sum := sha256.Sum256([]byte(identity))
	fingerprint := hex.EncodeToString(sum[:])
	return &MemoryInjectionManifest{
		RetrievalID: "retrieval-" + fingerprint[:20], RunID: runID, TaskID: taskID,
		Attempt: attempt, Agent: agentName, PolicyVersion: policy.PolicyVersion,
		QueryHash: hashContentKey(query), Items: items, Fingerprint: fingerprint, CreatedAt: time.Now().UTC(),
	}
}

func memorySignalKey(signal string, manifest *MemoryInjectionManifest, contextItemID string) string {
	return fmt.Sprintf("memory:%s:%s:%s:%d:%s:%s", signal, manifest.RunID, manifest.TaskID, manifest.Attempt, manifest.RetrievalID, contextItemID)
}

// persistMemoryManifest establishes the attempt-scoped attribution boundary
// before a model call. SaveSession errors are returned to the dispatch path;
// a worker is never started with an unverifiable manifest.
func (c *Coordinator) persistMemoryManifest(manifest *MemoryInjectionManifest) error {
	if manifest == nil {
		return nil
	}
	if c == nil || c.taskTracker == nil || c.sessionData == nil || c.session == nil {
		return fmt.Errorf("persist memory manifest: coordinator session is unavailable")
	}
	if err := c.taskTracker.TodoList().SetMemoryManifest(manifest.TaskID, manifest); err != nil {
		return err
	}
	c.sessionData.Tasks = c.taskTracker.TodoList().Items()
	if err := c.SessionStore().SaveSession(c.session.Workspace, c.sessionData); err != nil {
		return fmt.Errorf("persist memory manifest checkpoint: %w", err)
	}
	c.emitMemoryRetrievalEvents(manifest)
	return nil
}

func (c *Coordinator) emitMemoryRetrievalEvents(manifest *MemoryInjectionManifest) {
	if c == nil || manifest == nil {
		return
	}
	emittedCount := 0
	for _, item := range manifest.Items {
		projectID := ""
		if c.session != nil {
			projectID = c.contextScope().ProjectID
		}
		payload := memoryEventPayload{
			SchemaVersion: memoryEventSchemaVersion, RetrievalID: manifest.RetrievalID,
			ContextItemID: item.ContextItemID, PolicyVersion: manifest.PolicyVersion,
			ProjectID: projectID,
			Source:    item.Source, Rank: item.Rank, BaseScore: item.BaseScore,
			FinalScore: item.FinalScore, Fingerprint: manifest.Fingerprint, TokenCount: item.TokenCount,
		}
		if c.session != nil {
			policy := c.session.Config.MemoryLearning
			payload.PriorAlpha, payload.PriorBeta, payload.UtilityPercentile = policy.PriorAlpha, policy.PriorBeta, policy.UtilityPercentile
		}
		if item.ScoreParts.StaleEnvironmentPenalty > 0 {
			payload.ReasonCode = "stale_environment"
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			continue
		}
		key := memorySignalKey("retrieved", manifest, item.ContextItemID)
		event := RunEvent{
			Type: "memory_retrieved", Actor: "runtime", TaskID: manifest.TaskID,
			Attempt: manifest.Attempt, Timestamp: manifest.CreatedAt.Format(time.RFC3339Nano), IdempotencyKey: key, Payload: raw,
		}
		emitted, err := c.emitEventOnce(key, event)
		if err != nil {
			// The manifest remains authoritative and checkpointed. The event gap
			// is repairable without replaying the worker, so dispatch may proceed.
			continue
		}
		if emitted {
			c.reduceMemoryEvent(event)
			emittedCount++
		}
	}
	if emittedCount > 0 {
		c.report(c.newEvent("memory_learning").withAgent(manifest.Agent).withMessage(fmt.Sprintf("memory policy %s: %d exposure(s)", manifest.PolicyVersion, emittedCount)))
	}
}

func cloneMemoryInjectionManifest(src *MemoryInjectionManifest) *MemoryInjectionManifest {
	if src == nil {
		return nil
	}
	clone := *src
	clone.Items = append([]MemoryInjectionItem(nil), src.Items...)
	return &clone
}
