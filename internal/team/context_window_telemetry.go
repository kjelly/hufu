package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

const contextWindowTelemetrySchemaVersion = 1

// ContextWindowTelemetryEvent is the only payload persisted for context
// admission telemetry. It is deliberately limited to identities, enums,
// counts, budgets, and a policy digest; it never contains model content.
type ContextWindowTelemetryEvent struct {
	SchemaVersion        int    `json:"schema_version"`
	TelemetryID          string `json:"telemetry_id"`
	RunID                string `json:"run_id"`
	TeamID               string `json:"team_id"`
	BranchID             string `json:"branch_id"`
	CoordinatorAttemptID string `json:"coordinator_attempt_id"`
	StreamAttemptID      string `json:"stream_attempt_id"`
	Phase                string `json:"phase"`
	Model                string `json:"model"`
	RequestedTokens      int    `json:"requested_tokens"`
	AvailableTokens      int    `json:"available_tokens"`
	ReservedTokens       int    `json:"reserved_tokens"`
	SafetyTokens         int    `json:"safety_tokens"`
	WindowTokens         int    `json:"window_tokens"`
	Decision             string `json:"decision"`
	Step                 int    `json:"step"`
	CompactionCount      int    `json:"compaction_count"`
	PolicyDigest         string `json:"policy_digest"`
	FallbackReason       string `json:"fallback_reason,omitempty"`
	Attempt              int    `json:"attempt,omitempty"`
}

// ContextWindowTelemetrySummary is a content-free projection suitable for
// run results and restart-safe reporting.
type ContextWindowTelemetrySummary struct {
	AdmissionEvents      int    `json:"admission_events,omitempty"`
	Admitted             int    `json:"admitted,omitempty"`
	CannotFit            int    `json:"cannot_fit,omitempty"`
	CompactionCommits    int    `json:"compaction_commits,omitempty"`
	Downshifts           int    `json:"downshifts,omitempty"`
	DownshiftExhaustions int    `json:"downshift_exhaustions,omitempty"`
	LastDecision         string `json:"last_decision,omitempty"`
	LastModel            string `json:"last_model,omitempty"`
	LastRequestedTokens  int    `json:"last_requested_tokens,omitempty"`
	LastAvailableTokens  int    `json:"last_available_tokens,omitempty"`
	LastCompactionCount  int    `json:"last_compaction_count,omitempty"`
	PolicyDigest         string `json:"policy_digest,omitempty"`
}

func compactionPolicyDigest(policy CompactionPolicy) string {
	data, _ := json.Marshal(policy)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Coordinator) newContextWindowTelemetry(eventType EventType, request ContextWindowRequest, admission ContextWindowAdmission, phase string, taskID string, attempt int) ContextWindowTelemetryEvent {
	runID, teamID := "run-unavailable", "team-unavailable"
	if c != nil {
		if c.executionRunID != "" {
			runID = c.executionRunID
		}
		if c.session != nil && c.session.Config.Name != "" {
			teamID = c.session.Config.Name
		}
	}
	branchID := "main"
	if c != nil && c.compactionBranchID != "" {
		branchID = c.compactionBranchID
	}
	sequence := uint64(0)
	if c != nil {
		sequence = c.contextRequestSeq.Add(1)
	}
	streamID := strings.TrimSpace(taskID)
	if streamID == "" {
		streamID = "coordinator-stream"
	}
	streamID = fmt.Sprintf("%s:%d", streamID, attempt)
	// The request sequence identifies this telemetry event only. Initial and
	// final admissions for one PrepareStep must share the execution attempt
	// identity, while a retry must receive a different identity.
	coordID := "coordinator-attempt-" + hashContentKey(strings.Join([]string{runID, branchID, streamID}, "\x00"))[:16]
	decision := string(admission.Decision)
	return ContextWindowTelemetryEvent{
		SchemaVersion: contextWindowTelemetrySchemaVersion, TelemetryID: fmt.Sprintf("%s-%d", strings.ReplaceAll(string(eventType), "_", "-"), sequence),
		RunID: runID, TeamID: teamID, BranchID: branchID, CoordinatorAttemptID: coordID, StreamAttemptID: streamID,
		Phase: phase, Model: request.ModelID, RequestedTokens: admission.RequestTokens, AvailableTokens: admission.Budget.Available,
		ReservedTokens: admission.Budget.ReservedReply, SafetyTokens: admission.Budget.SafetyMargin, WindowTokens: admission.Budget.Window,
		Decision: decision, Step: request.StepNumber, CompactionCount: c.Metrics().Compactions, PolicyDigest: compactionPolicyDigest(c.compactionPolicy()), Attempt: attempt,
	}
}

func (c *Coordinator) recordContextWindowTelemetry(eventType EventType, event ContextWindowTelemetryEvent, taskID string) error {
	if c == nil || c.eventStore == nil {
		return nil
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal context window telemetry: %w", err)
	}
	persisted, err := c.eventStore.AppendPersisted(RunEvent{Type: string(eventType), Actor: "coordinator", TaskID: taskID, Attempt: event.Attempt, Payload: data})
	if err != nil {
		return fmt.Errorf("persist context window telemetry: %w", err)
	}
	if persisted.ID != "" {
		event.TelemetryID = persisted.ID
	}
	c.addContextWindowTelemetrySummary(eventType, event)
	status := c.newEvent(string(eventType)).withModel(event.Model).withStep(event.Step).withMessage(fmt.Sprintf("context window %s: decision=%s requested=%d available=%d", event.Phase, event.Decision, event.RequestedTokens, event.AvailableTokens))
	status.ContextWindowTelemetry = &event
	c.report(status)
	return nil
}

func (c *Coordinator) addContextWindowTelemetrySummary(eventType EventType, event ContextWindowTelemetryEvent) {
	if c == nil {
		return
	}
	c.metricsMu.Lock()
	summary := &c.contextWindowTelemetry
	switch eventType {
	case EventContextWindowAdmission:
		summary.AdmissionEvents++
		if event.Decision == string(ContextWindowCannotFit) {
			summary.CannotFit++
		} else {
			summary.Admitted++
		}
	case EventContextWindowCompactionCommitted:
		summary.CompactionCommits++
	case EventContextWindowDownshift:
		if event.Decision == "exhausted" {
			summary.DownshiftExhaustions++
		} else {
			summary.Downshifts++
		}
	}
	summary.LastDecision = event.Decision
	summary.LastModel = event.Model
	summary.LastRequestedTokens = event.RequestedTokens
	summary.LastAvailableTokens = event.AvailableTokens
	summary.LastCompactionCount = event.CompactionCount
	summary.PolicyDigest = event.PolicyDigest
	c.metricsMu.Unlock()
}

func (c *Coordinator) hydrateContextWindowTelemetry(events []RunEvent) {
	if c == nil {
		return
	}
	for _, persisted := range events {
		switch EventType(persisted.Type) {
		case EventContextWindowAdmission, EventContextWindowCompactionCommitted, EventContextWindowDownshift:
			var event ContextWindowTelemetryEvent
			if json.Unmarshal(persisted.Payload, &event) == nil {
				c.addContextWindowTelemetrySummary(EventType(persisted.Type), event)
			}
		}
	}
}

func (c *Coordinator) resetContextWindowTelemetrySummary() {
	if c == nil {
		return
	}
	c.metricsMu.Lock()
	c.contextWindowTelemetry = ContextWindowTelemetrySummary{}
	c.metricsMu.Unlock()
}
