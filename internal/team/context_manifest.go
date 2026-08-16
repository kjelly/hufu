package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

const ContextManifestSchemaVersion = 1

type ContextManifestItem struct {
	ID              string                `json:"id"`
	Kind            string                `json:"kind"`
	Source          string                `json:"source,omitempty"`
	Included        bool                  `json:"included"`
	Reason          ContextDecisionReason `json:"reason"`
	Tokens          int                   `json:"tokens"`
	Compressed      bool                  `json:"compressed,omitempty"`
	BaseScore       float64               `json:"base_score,omitempty"`
	FinalScore      float64               `json:"final_score,omitempty"`
	DisclosureLevel string                `json:"disclosure_level,omitempty"`
}

type ContextInjectionManifest struct {
	SchemaVersion             int                   `json:"schema_version"`
	RequestID                 string                `json:"request_id"`
	RequestHash               string                `json:"request_hash"`
	RunID                     string                `json:"run_id"`
	TaskID                    string                `json:"task_id,omitempty"`
	Attempt                   int                   `json:"attempt"`
	Agent                     string                `json:"agent"`
	AgentRole                 string                `json:"agent_role,omitempty"`
	ModelExecutionID          string                `json:"model_execution_id,omitempty"`
	Environment               string                `json:"environment,omitempty"`
	Phase                     Phase                 `json:"phase"`
	Trigger                   ContextTrigger        `json:"trigger"`
	Purpose                   string                `json:"purpose,omitempty"`
	ParentTrigger             ContextTrigger        `json:"parent_trigger,omitempty"`
	ParentRequestID           string                `json:"parent_request_id,omitempty"`
	ParentManifestFingerprint string                `json:"parent_manifest_fingerprint,omitempty"`
	ModelCalled               bool                  `json:"model_called"`
	Outcome                   string                `json:"outcome,omitempty"`
	ToolCallID                string                `json:"tool_call_id,omitempty"`
	FailureClass              string                `json:"failure_class,omitempty"`
	Items                     []ContextManifestItem `json:"items"`
	Fingerprint               string                `json:"fingerprint"`
	CreatedAt                 time.Time             `json:"created_at"`
}

func manifestItemID(id string) string { return strings.TrimPrefix(id, "context:") }

func BuildContextInjectionManifest(request ContextRequest, compiled CompiledContext, decisions []ContextRouteDecision, agentName string, createdAt time.Time) ContextInjectionManifest {
	decisionByID := make(map[string]ContextRouteDecision, len(decisions))
	for _, decision := range decisions {
		decisionByID[manifestItemID(decision.ContextItemID)] = decision
	}
	items := make([]ContextManifestItem, 0, len(compiled.IncludedItems)+len(compiled.OmittedItems)+len(decisions))
	seen := make(map[string]bool)
	appendCompiled := func(item ContextItem, included bool) {
		id := manifestItemID(item.ID)
		reason := ContextIncludedRelevant
		if item.Required {
			reason = ContextIncludedRequired
		}
		if !included {
			reason = ContextOmittedBudget
			for _, kept := range compiled.IncludedItems {
				if kept.NormalizedDedupKey() == item.NormalizedDedupKey() {
					reason = ContextOmittedDuplicate
					break
				}
			}
		}
		decision := decisionByID[id]
		if decision.Reason != "" && included == decision.Included {
			reason = decision.Reason
		}
		items = append(items, ContextManifestItem{ID: id, Kind: item.Kind, Source: item.Source, Included: included, Reason: reason, Tokens: item.TokenCount, Compressed: item.Compressed, BaseScore: decision.BaseScore, FinalScore: decision.FinalScore, DisclosureLevel: contextDisclosureLevel(item.Kind)})
		seen[id] = true
	}
	for _, item := range compiled.IncludedItems {
		appendCompiled(item, true)
	}
	for _, item := range compiled.OmittedItems {
		appendCompiled(item, false)
	}
	for _, decision := range decisions {
		id := manifestItemID(decision.ContextItemID)
		if seen[id] {
			continue
		}
		items = append(items, ContextManifestItem{ID: id, Kind: "canonical_memory", Included: decision.Included, Reason: decision.Reason, BaseScore: decision.BaseScore, FinalScore: decision.FinalScore})
	}
	manifest := ContextInjectionManifest{SchemaVersion: ContextManifestSchemaVersion, RequestID: request.RequestID, RequestHash: request.Fingerprint(), RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt, Agent: agentName, AgentRole: request.AgentRole, ModelExecutionID: request.ModelExecutionID, Environment: request.EnvironmentFingerprint, Phase: request.Phase, Trigger: request.Trigger, Purpose: request.Purpose, ParentTrigger: request.ParentTrigger, ParentRequestID: request.ParentRequestID, ParentManifestFingerprint: request.ParentManifestFingerprint, ModelCalled: true, Outcome: "model_call", Items: items, CreatedAt: createdAt.UTC()}
	if request.Failure != nil {
		if len(request.Failure.EvidenceRefs) > 0 {
			manifest.ToolCallID = request.Failure.EvidenceRefs[0]
		}
		manifest.FailureClass = request.Failure.ErrorClass
		if manifest.FailureClass == "" {
			manifest.FailureClass = string(request.Failure.Class)
		}
	}
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return manifest
}

func contextDisclosureLevel(kind string) string {
	switch {
	case strings.HasPrefix(kind, "skill_summary"):
		return "1"
	case strings.HasPrefix(kind, "skill_section"):
		return "2"
	case strings.HasPrefix(kind, "skill_full"):
		return "3"
	case strings.HasPrefix(kind, "skill_"):
		return "0"
	default:
		return ""
	}
}

func contextManifestFingerprint(manifest ContextInjectionManifest) string {
	manifest.Fingerprint = ""
	manifest.CreatedAt = time.Time{}
	data, _ := json.Marshal(manifest)
	return hashContentKey(string(data))
}

type ContextManifestSummary struct {
	Requests       int            `json:"requests"`
	ModelCalls     int            `json:"model_calls"`
	Fallbacks      int            `json:"fallbacks"`
	Included       int            `json:"included"`
	Omitted        int            `json:"omitted"`
	IncludedTokens int            `json:"included_tokens"`
	OmittedTokens  int            `json:"omitted_tokens"`
	OmitReasons    map[string]int `json:"omit_reasons,omitempty"`
	Purposes       map[string]int `json:"purposes,omitempty"`
}

func SummarizeContextManifests(manifests []ContextInjectionManifest) ContextManifestSummary {
	summary := ContextManifestSummary{Requests: len(manifests), OmitReasons: make(map[string]int), Purposes: make(map[string]int)}
	for _, manifest := range manifests {
		if manifest.ModelCalled {
			summary.ModelCalls++
		} else {
			summary.Fallbacks++
		}
		if manifest.Purpose != "" {
			summary.Purposes[manifest.Purpose]++
		}
		for _, item := range manifest.Items {
			if item.Included {
				summary.Included++
				summary.IncludedTokens += item.Tokens
			} else {
				summary.Omitted++
				summary.OmittedTokens += item.Tokens
				summary.OmitReasons[string(item.Reason)]++
			}
		}
	}
	return summary
}

func ContextManifestsFromTodos(items []*TodoItem) []ContextInjectionManifest {
	var manifests []ContextInjectionManifest
	for _, item := range items {
		if item != nil {
			manifests = append(manifests, item.ContextManifests...)
		}
	}
	sort.SliceStable(manifests, func(i, j int) bool {
		if manifests[i].RunID != manifests[j].RunID {
			return manifests[i].RunID < manifests[j].RunID
		}
		if manifests[i].TaskID != manifests[j].TaskID {
			return manifests[i].TaskID < manifests[j].TaskID
		}
		if manifests[i].Attempt != manifests[j].Attempt {
			return manifests[i].Attempt < manifests[j].Attempt
		}
		if manifests[i].ModelExecutionID != manifests[j].ModelExecutionID {
			return manifests[i].ModelExecutionID < manifests[j].ModelExecutionID
		}
		return manifests[i].RequestID < manifests[j].RequestID
	})
	return manifests
}

func cloneContextInjectionManifest(manifest *ContextInjectionManifest) *ContextInjectionManifest {
	if manifest == nil {
		return nil
	}
	copyManifest := *manifest
	copyManifest.Items = append([]ContextManifestItem(nil), manifest.Items...)
	return &copyManifest
}

func mergeContextInjectionManifests(existing, incoming []ContextInjectionManifest) []ContextInjectionManifest {
	merged := make([]ContextInjectionManifest, 0, len(existing)+len(incoming))
	index := make(map[string]int, len(existing)+len(incoming))
	appendOrReplace := func(manifest ContextInjectionManifest) {
		copyManifest := *cloneContextInjectionManifest(&manifest)
		key := manifest.RequestID + "\x00" + manifest.ModelExecutionID
		if at, ok := index[key]; ok {
			merged[at] = copyManifest
			return
		}
		index[key] = len(merged)
		merged = append(merged, copyManifest)
	}
	for _, manifest := range existing {
		appendOrReplace(manifest)
	}
	for _, manifest := range incoming {
		appendOrReplace(manifest)
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Attempt != merged[j].Attempt {
			return merged[i].Attempt < merged[j].Attempt
		}
		return merged[i].RequestID < merged[j].RequestID
	})
	return merged
}

// persistContextManifest checkpoints the content-free attribution boundary
// before a model call. Task manifests are dual-written to the task event
// lineage; coordinator manifests are stored on the session and event store.
func (c *Coordinator) persistContextManifest(manifest *ContextInjectionManifest) error {
	if manifest == nil {
		return nil
	}
	if c == nil || c.session == nil {
		return fmt.Errorf("persist context manifest: coordinator session is unavailable")
	}
	if c.sessionData == nil {
		c.sessionData = &SessionData{CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	if manifest.TaskID != "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil && c.taskTracker.TodoList().Has(manifest.TaskID) {
		if err := c.taskTracker.TodoList().SetContextManifest(manifest.TaskID, manifest); err != nil {
			return err
		}
		c.sessionData.Tasks = c.taskTracker.TodoList().Items()
		if err := c.SessionStore().SaveSession(c.session.Workspace, c.sessionData); err != nil {
			return fmt.Errorf("persist context manifest checkpoint: %w", err)
		}
		payload := map[string]any{"id": manifest.TaskID, "context_manifests": []ContextInjectionManifest{*manifest}}
		if err := c.emitEvent("task_context_manifest", manifest.Agent, manifest.TaskID, payload); err != nil {
			return fmt.Errorf("persist context manifest event: %w", err)
		}
		if err := c.recordContextSelectionObservations(manifest); err != nil {
			return err
		}
		c.reportContextRouted(manifest)
		return nil
	}
	// Auxiliary and task-less recovery calls are durable coordinator-scope
	// model decisions. They deliberately do not manufacture a Todo item; the
	// session/event lineage remains their canonical replay projection.
	replaced := false
	for i := range c.sessionData.CoordinatorContextManifests {
		if sameContextManifestIdentity(c.sessionData.CoordinatorContextManifests[i], *manifest) {
			c.sessionData.CoordinatorContextManifests[i] = *cloneContextInjectionManifest(manifest)
			replaced = true
			break
		}
	}
	if !replaced {
		c.sessionData.CoordinatorContextManifests = append(c.sessionData.CoordinatorContextManifests, *cloneContextInjectionManifest(manifest))
	}
	if err := c.SessionStore().SaveSession(c.session.Workspace, c.sessionData); err != nil {
		return fmt.Errorf("persist coordinator context manifest checkpoint: %w", err)
	}
	if err := c.emitEvent("context_manifest", manifest.Agent, "", manifest); err != nil {
		return fmt.Errorf("persist coordinator context manifest event: %w", err)
	}
	if err := c.recordContextSelectionObservations(manifest); err != nil {
		return err
	}
	c.reportContextRouted(manifest)
	return nil
}

func sameContextManifestIdentity(a, b ContextInjectionManifest) bool {
	return a.RequestID == b.RequestID && a.ModelExecutionID == b.ModelExecutionID
}

func (c *Coordinator) reportContextRouted(manifest *ContextInjectionManifest) {
	if c == nil || manifest == nil {
		return
	}
	included, omitted, includedTokens := 0, 0, 0
	omitReasons := make(map[string]int)
	for _, item := range manifest.Items {
		if item.Included {
			included++
			includedTokens += item.Tokens
		} else {
			omitted++
			omitReasons[string(item.Reason)]++
		}
	}
	reasons := make([]string, 0, len(omitReasons))
	for reason, count := range omitReasons {
		reasons = append(reasons, fmt.Sprintf("%s:%d", reason, count))
	}
	sort.Strings(reasons)
	state := "called"
	if !manifest.ModelCalled {
		state = "fallback/no-model"
	}
	message := fmt.Sprintf("context routed: request=%s attempt=%d %s/%s included=%d tokens=%d omitted=%d reasons=%s state=%s", manifest.RequestID, manifest.Attempt, manifest.Trigger, manifest.Purpose, included, includedTokens, omitted, strings.Join(reasons, ","), state)
	c.report(c.newEvent("context_routed").withAgent(manifest.Agent).withTodoID(manifest.TaskID).withMessage(message))
}

type contextOutcomeRecorder interface {
	RecordContextOutcomeObservation(context.Context, contextstore.ContextOutcomeObservation) (bool, error)
}

// contextOutcomeObservation preserves the full injection identity on every
// outcome row. The idempotency key remains opaque; consumers can instead join
// the observation to a replayable manifest through these typed fields.
func contextOutcomeObservation(manifest *ContextInjectionManifest, contextItemID, outcome, verificationOutcome string) contextstore.ContextOutcomeObservation {
	observation := contextstore.ContextOutcomeObservation{
		ContextItemID:       contextItemID,
		Phase:               string(manifest.Phase),
		Trigger:             string(manifest.Trigger),
		AgentRole:           manifest.AgentRole,
		Environment:         manifest.Environment,
		Outcome:             outcome,
		RequestID:           manifest.RequestID,
		ManifestFingerprint: manifest.Fingerprint,
		RunID:               manifest.RunID,
		TaskID:              manifest.TaskID,
		Attempt:             manifest.Attempt,
		ModelExecutionID:    manifest.ModelExecutionID,
		VerificationOutcome: verificationOutcome,
		AcceptanceOutcome:   "not_assessed",
		JudgeOutcome:        "not_assessed",
		SkepticOutcome:      "not_assessed",
		ObservedAt:          time.Now().UTC(),
	}
	if manifest.Trigger == ContextTriggerJudge {
		observation.JudgeOutcome = manifest.Outcome
	}
	if manifest.Trigger == ContextTriggerSkeptic {
		observation.SkepticOutcome = manifest.Outcome
	}
	return observation
}

func (c *Coordinator) recordContextSelectionObservations(manifest *ContextInjectionManifest) error {
	recorder, ok := c.contextRepo.(contextOutcomeRecorder)
	if !ok || manifest == nil {
		return nil
	}
	policyRevision := c.session.Config.MemoryLearning.PolicyVersion
	for _, item := range manifest.Items {
		if !item.Included || item.ID == "" || (item.Source != "shared_persistent" && item.Source != "shared_session" && item.Source != "context_get") {
			continue
		}
		observation := contextOutcomeObservation(manifest, item.ID, "selected", "not_assessed")
		observation.IdempotencyKey = manifest.Fingerprint + ":" + item.ID + ":selected"
		observation.PolicyRevision = policyRevision
		observation.ObservedAt = manifest.CreatedAt
		if _, err := recorder.RecordContextOutcomeObservation(context.Background(), observation); err != nil {
			return fmt.Errorf("record context selection observation: %w", err)
		}
	}
	return nil
}

// recordContextToolConsulted records an actual context_get disclosure. A
// selected item is not necessarily read by a worker; this separate event is
// emitted before the tool response reveals the bounded content.
func (c *Coordinator) recordContextToolConsulted(manifest *ContextInjectionManifest, contextItemID string) error {
	recorder, ok := c.contextRepo.(contextOutcomeRecorder)
	if !ok || manifest == nil || strings.TrimSpace(contextItemID) == "" {
		return nil
	}
	observation := contextOutcomeObservation(manifest, contextItemID, "tool_consulted", "not_assessed")
	observation.IdempotencyKey = manifest.Fingerprint + ":" + contextItemID + ":tool_consulted"
	if c.session != nil {
		observation.PolicyRevision = c.session.Config.MemoryLearning.PolicyVersion
	}
	if _, err := recorder.RecordContextOutcomeObservation(context.Background(), observation); err != nil {
		return fmt.Errorf("record context tool consultation: %w", err)
	}
	return nil
}

// recordContextAcceptanceObservations closes the outcome loop after the sole
// global acceptance authority has decided. It never reuses raw context
// content: every row refers back to the exact persisted manifest instead.
func (c *Coordinator) recordContextAcceptanceObservations(acceptance *AcceptanceResult) error {
	if c == nil || c.session == nil {
		return nil
	}
	recorder, ok := c.contextRepo.(contextOutcomeRecorder)
	if !ok {
		return nil
	}
	acceptanceOutcome := string(AcceptanceNotConfigured)
	if acceptance != nil {
		acceptanceOutcome = string(acceptance.EffectiveState())
	}
	var manifests []ContextInjectionManifest
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		manifests = append(manifests, ContextManifestsFromTodos(c.taskTracker.TodoList().Items())...)
	}
	if c.sessionData != nil {
		manifests = append(manifests, c.sessionData.CoordinatorContextManifests...)
	}
	policyRevision := c.session.Config.MemoryLearning.PolicyVersion
	for i := range manifests {
		manifest := &manifests[i]
		for _, item := range manifest.Items {
			if !item.Included || item.ID == "" || (item.Source != "shared_persistent" && item.Source != "shared_session" && item.Source != "context_get") {
				continue
			}
			observation := contextOutcomeObservation(manifest, item.ID, "acceptance_assessed", "not_assessed")
			observation.IdempotencyKey = manifest.Fingerprint + ":" + item.ID + ":acceptance:" + acceptanceOutcome
			observation.PolicyRevision = policyRevision
			observation.AcceptanceOutcome = acceptanceOutcome
			if _, err := recorder.RecordContextOutcomeObservation(context.Background(), observation); err != nil {
				return fmt.Errorf("record context acceptance observation: %w", err)
			}
		}
	}
	return nil
}

// recordAuxiliaryContextSignal links a judge or skeptic result to only the
// canonical items that the corresponding invocation actually included. The
// reviewer prompt normally has no shared memory by design, in which case this
// deliberately records nothing rather than manufacturing exposure evidence.
func (c *Coordinator) recordAuxiliaryContextSignal(taskID, purpose, eventKind, signal string) error {
	if c == nil || c.session == nil {
		return nil
	}
	recorder, ok := c.contextRepo.(contextOutcomeRecorder)
	if !ok {
		return nil
	}
	var manifests []ContextInjectionManifest
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil && taskID != "" {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && item.ID == taskID {
				manifests = append(manifests, item.ContextManifests...)
				break
			}
		}
	}
	if c.sessionData != nil {
		for _, manifest := range c.sessionData.CoordinatorContextManifests {
			if taskID == "" || manifest.TaskID == taskID || manifest.TaskID == "auxiliary-"+purpose {
				manifests = append(manifests, manifest)
			}
		}
	}
	policyRevision := c.session.Config.MemoryLearning.PolicyVersion
	for i := range manifests {
		manifest := &manifests[i]
		if manifest.Purpose != purpose || !manifest.ModelCalled {
			continue
		}
		for _, item := range manifest.Items {
			if !item.Included || item.ID == "" || (item.Source != "shared_persistent" && item.Source != "shared_session" && item.Source != "context_get") {
				continue
			}
			observation := contextOutcomeObservation(manifest, item.ID, eventKind, "not_assessed")
			observation.IdempotencyKey = manifest.Fingerprint + ":" + item.ID + ":" + eventKind + ":" + signal
			observation.PolicyRevision = policyRevision
			switch purpose {
			case "judge":
				observation.JudgeOutcome = signal
			case "skeptic":
				observation.SkepticOutcome = signal
			}
			if _, err := recorder.RecordContextOutcomeObservation(context.Background(), observation); err != nil {
				return fmt.Errorf("record %s context signal: %w", purpose, err)
			}
		}
	}
	return nil
}

func (c *Coordinator) ContextManifestReport() ContextManifestSummary {
	if c == nil {
		return ContextManifestSummary{}
	}
	var manifests []ContextInjectionManifest
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		manifests = append(manifests, ContextManifestsFromTodos(c.taskTracker.TodoList().Items())...)
	}
	if c.sessionData != nil {
		manifests = append(manifests, c.sessionData.CoordinatorContextManifests...)
	}
	return SummarizeContextManifests(manifests)
}
