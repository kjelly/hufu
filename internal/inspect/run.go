package inspect

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

type TaskSummary struct {
	Total      int `json:"total"`
	Done       int `json:"done"`
	Unresolved int `json:"unresolved"`
}

type AttemptSummary struct {
	Total  int `json:"total"`
	Failed int `json:"failed"`
}

type RunData struct {
	RunID           string         `json:"run_id"`
	TerminalEventID string         `json:"terminal_event_id,omitempty"`
	Outcome         string         `json:"outcome,omitempty"`
	GoalSatisfied   bool           `json:"goal_satisfied,omitzero"`
	StopReason      string         `json:"stop_reason,omitempty"`
	Acceptance      string         `json:"acceptance"`
	Completion      string         `json:"completion"`
	TaskSummary     TaskSummary    `json:"task_summary"`
	AttemptSummary  AttemptSummary `json:"attempt_summary"`
	EvidenceRefs    []string       `json:"evidence_refs"`
}

type AttemptData struct {
	Attempt            int    `json:"attempt"`
	ModelExecutionID   string `json:"model_execution_id,omitempty"`
	ProducerID         string `json:"producer_id,omitempty"`
	Backend            string `json:"backend,omitempty"`
	ExecutionTarget    string `json:"execution_target,omitempty"`
	ExitCode           *int   `json:"exit_code,omitempty"`
	VerificationStatus string `json:"verification_status"`
	VerificationRef    string `json:"verification_ref,omitempty"`
	Winning            bool   `json:"winning,omitzero"`
}

type TaskData struct {
	RunID             string                 `json:"run_id"`
	TaskID            string                 `json:"task_id"`
	Status            string                 `json:"status"`
	Phase             string                 `json:"phase,omitempty"`
	AgentID           string                 `json:"agent_id,omitempty"`
	ExecutionTarget   string                 `json:"execution_target,omitempty"`
	ExecutionTopology []string               `json:"execution_topology"`
	Attempts          []AttemptData          `json:"attempts"`
	RetryDisposition  string                 `json:"retry_disposition,omitempty"`
	FailureClass      string                 `json:"failure_class,omitempty"`
	ReasonCode        string                 `json:"reason_code,omitempty"`
	RecoveryState     string                 `json:"recovery_state,omitempty"`
	ArtifactRefs      []string               `json:"artifact_refs"`
	ContextRefs       []string               `json:"context_refs"`
	MemoryRefs        []string               `json:"memory_refs"`
	KnowledgeCoverage *KnowledgeCoverageData `json:"knowledge_coverage,omitempty"`
}

type selectedRun struct {
	raw           []team.RunEvent
	runEvents     []team.RunEvent
	terminal      *team.RunEvent
	terminalIndex int
}

func InspectRun(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindRun); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: replay tasks for run %q: %v", ErrIntegrity, query.RunID, err)
	}
	explanation, err := auditverify.ExplainLineageRun(ctx, query.Workspace, query.RunID, selected.raw)
	if err != nil {
		return nil, classifyProjectionError(query.RunID, err)
	}

	data := RunData{
		RunID:          query.RunID,
		Acceptance:     "unavailable",
		Completion:     "unavailable",
		TaskSummary:    summarizeTasks(tasks),
		AttemptSummary: summarizeAttempts(tasks, query.RunID),
		EvidenceRefs:   []string{},
	}
	if selected.terminal != nil {
		data.TerminalEventID = selected.terminal.ID
		projected := team.ReduceToSessionData(selected.raw[:selected.terminalIndex+1])
		if projected == nil || projected.RunResult == nil || projected.RunResult.RunID != query.RunID {
			return nil, fmt.Errorf("%w: terminal event %q did not reduce to run %q", ErrIntegrity, selected.terminal.ID, query.RunID)
		}
		result := projected.RunResult
		data.Outcome = string(result.Outcome)
		data.GoalSatisfied = result.GoalSatisfied
		data.StopReason = string(result.StopReason)
		if result.Acceptance != nil {
			data.Acceptance = string(result.Acceptance.EffectiveState())
		} else {
			data.Acceptance = string(team.AcceptanceNotConfigured)
		}
		if result.EvidenceManifest != nil {
			data.EvidenceRefs = evidenceRefs(result.EvidenceManifest)
		}
	}
	if explanation != nil && explanation.Verification != nil {
		data.Completion = string(explanation.Verification.Completion.Status)
	}
	return envelope(KindRun, query, lineage.BranchID, data), nil
}

func InspectTask(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindTask); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: replay tasks for run %q: %v", ErrIntegrity, query.RunID, err)
	}
	var matches []*team.TodoItem
	for _, item := range tasks {
		if item != nil && item.ID == query.TaskID {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("%w: task %q in run %q", ErrNotFound, query.TaskID, query.RunID)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("%w: task %q in run %q", ErrAmbiguous, query.TaskID, query.RunID)
	}

	explanation, err := auditverify.ExplainLineageRun(ctx, query.Workspace, query.RunID, selected.raw)
	if err != nil {
		return nil, classifyProjectionError(query.RunID, err)
	}
	data := projectTaskWithEvents(matches[0], query, runIndexedEvents(lineage, query))
	if query.Attempt > 0 && len(data.Attempts) == 0 {
		return nil, fmt.Errorf("%w: attempt %d for task %q in run %q", ErrNotFound, query.Attempt, query.TaskID, query.RunID)
	}
	if explanation != nil {
		applyWinningAttempts(&data, explanation.AttemptHistory)
	}
	return envelope(KindTask, query, lineage.BranchID, data), nil
}

func selectRun(lineage Lineage, query InspectQuery) (selectedRun, error) {
	raw := make([]team.RunEvent, 0, len(lineage.Events))
	var runEvents []team.RunEvent
	for _, indexed := range lineage.Events {
		if query.SessionID == "" || indexed.Event.SessionID == query.SessionID {
			raw = append(raw, indexed.Event)
		}
		if indexed.Event.RunID != query.RunID {
			continue
		}
		if query.SessionID != "" && indexed.Event.SessionID != query.SessionID {
			continue
		}
		runEvents = append(runEvents, indexed.Event)
	}
	if len(runEvents) == 0 {
		return selectedRun{}, fmt.Errorf("%w: run %q", ErrNotFound, query.RunID)
	}

	sessions := make(map[string]struct{})
	for _, event := range runEvents {
		if event.SessionID != "" {
			sessions[event.SessionID] = struct{}{}
		}
	}
	if query.SessionID == "" && len(sessions) > 1 {
		return selectedRun{}, fmt.Errorf("%w: run %q appears in multiple sessions", ErrAmbiguous, query.RunID)
	}

	selected := selectedRun{raw: raw, runEvents: runEvents, terminalIndex: -1}
	for index := range raw {
		event := &raw[index]
		if event.RunID != query.RunID || event.Type != "run_finished" {
			continue
		}
		if query.SessionID != "" && event.SessionID != query.SessionID {
			continue
		}
		if selected.terminal != nil {
			return selectedRun{}, fmt.Errorf("%w: run %q has multiple terminal events", ErrIntegrity, query.RunID)
		}
		selected.terminal = event
		selected.terminalIndex = index
	}
	return selected, nil
}

func envelope(kind Kind, query InspectQuery, branchID string, data any) *Envelope {
	query.Workspace = ""
	query.BranchID = branchID
	return &Envelope{
		SchemaVersion: SchemaVersion,
		Kind:          kind,
		Query:         query,
		Integrity:     Integrity{EventChain: "verified", Projection: "consistent"},
		Data:          data,
		Diagnostics:   []Diagnostic{},
	}
}

func summarizeTasks(tasks []*team.TodoItem) TaskSummary {
	stats := team.SummarizeRunStats(tasks)
	return TaskSummary{Total: stats.TasksTotal, Done: stats.TasksDone, Unresolved: stats.TasksUnresolved}
}

func summarizeAttempts(tasks []*team.TodoItem, runID string) AttemptSummary {
	var summary AttemptSummary
	for _, item := range tasks {
		if item == nil {
			continue
		}
		for _, receipt := range item.ExecutionReceipts {
			if receipt.RunID != runID || receipt.TaskID != item.ID {
				continue
			}
			summary.Total++
			if receipt.ExitCode != nil && *receipt.ExitCode != 0 {
				summary.Failed++
			}
		}
	}
	return summary
}

func evidenceRefs(manifest *team.EvidenceManifest) []string {
	refs := appendOpaqueRef(nil, manifest.ManifestHash)
	for _, ref := range manifest.ArtifactRefs {
		refs = appendOpaqueRef(refs, ref.ID, ref.SHA256)
	}
	return normalizeRefs(refs)
}

func projectTask(item *team.TodoItem, query InspectQuery) TaskData {
	return projectTaskWithEvents(item, query, nil)
}

func projectTaskWithEvents(item *team.TodoItem, query InspectQuery, events []IndexedEvent) TaskData {
	data := TaskData{
		RunID:             query.RunID,
		TaskID:            item.ID,
		Status:            string(item.Status),
		Phase:             string(item.Phase),
		AgentID:           item.Agent,
		ExecutionTarget:   item.ExecutionTarget.String(),
		ExecutionTopology: []string{},
		Attempts:          []AttemptData{},
		RecoveryState:     item.RecoveryState,
		ArtifactRefs:      []string{},
		ContextRefs:       []string{},
		MemoryRefs:        []string{},
	}
	for _, target := range item.ExecutionTopology {
		data.ExecutionTopology = append(data.ExecutionTopology, target.String())
	}
	if item.FailureEvent != nil {
		data.RetryDisposition = string(item.FailureEvent.RetryDisposition)
		data.FailureClass = string(item.FailureEvent.FailureClass)
		data.ReasonCode = item.FailureEvent.FailureType
	}
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID != query.RunID || receipt.TaskID != item.ID || query.Attempt > 0 && receipt.Attempt != query.Attempt {
			continue
		}
		attempt := AttemptData{
			Attempt:            receipt.Attempt,
			ModelExecutionID:   receipt.ModelExecutionID,
			ProducerID:         receipt.ProducerID,
			Backend:            receipt.Backend,
			ExecutionTarget:    receiptExecutionTarget(findReceiptAnchor(events, receipt), receipt),
			ExitCode:           receipt.ExitCode,
			VerificationStatus: "not_run",
		}
		if receipt.VerifyResult != nil {
			attempt.VerificationRef = receipt.VerifyResult.Fingerprint
			if receipt.VerifyResult.ExitCode == 0 {
				attempt.VerificationStatus = "passed"
			} else {
				attempt.VerificationStatus = "failed"
			}
		}
		data.Attempts = append(data.Attempts, attempt)
		data.ArtifactRefs = appendOpaqueRef(data.ArtifactRefs, receipt.TranscriptRef)
		data.ArtifactRefs = appendOpaqueRef(data.ArtifactRefs, receipt.ProviderTranscriptRef)
	}
	if item.TypedResult != nil {
		if query.Attempt == 0 || item.TypedResult.Attempt == query.Attempt {
			data.KnowledgeCoverage = projectKnowledgeCoverage(item.TypedResult.KnowledgeCoverage)
		}
		for _, ref := range item.TypedResult.Artifacts {
			data.ArtifactRefs = appendOpaqueRef(data.ArtifactRefs, ref.ID, ref.SHA256)
		}
		if item.TypedResult.RawOutputRef != nil {
			data.ArtifactRefs = appendOpaqueRef(data.ArtifactRefs, item.TypedResult.RawOutputRef.ID, item.TypedResult.RawOutputRef.SHA256)
		}
	}
	for _, ref := range item.DecisionArtifacts {
		data.ArtifactRefs = appendOpaqueRef(data.ArtifactRefs, ref.ID, ref.SHA256)
	}
	for _, manifest := range item.ContextManifests {
		if query.Attempt > 0 && manifest.Attempt != query.Attempt {
			continue
		}
		for _, contextItem := range manifest.Items {
			data.ContextRefs = appendOpaqueRef(data.ContextRefs, strings.TrimPrefix(contextItem.ID, "context:"))
		}
	}
	for _, manifest := range item.MemoryManifests {
		if query.Attempt > 0 && manifest.Attempt != query.Attempt {
			continue
		}
		for _, memoryItem := range manifest.Items {
			data.MemoryRefs = appendOpaqueRef(data.MemoryRefs, memoryItem.ContextItemID)
		}
	}
	slices.SortFunc(data.Attempts, func(left, right AttemptData) int {
		if order := cmp.Compare(left.Attempt, right.Attempt); order != 0 {
			return order
		}
		return cmp.Compare(left.ModelExecutionID, right.ModelExecutionID)
	})
	data.ArtifactRefs = normalizeRefs(data.ArtifactRefs)
	data.ContextRefs = normalizeRefs(data.ContextRefs)
	data.MemoryRefs = normalizeRefs(data.MemoryRefs)
	return data
}

func applyWinningAttempts(data *TaskData, histories []auditverify.TaskAttemptHistory) {
	for _, history := range histories {
		if history.TaskID != data.TaskID {
			continue
		}
		for index := range data.Attempts {
			attempt := &data.Attempts[index]
			attempt.Winning = attempt.Attempt == history.Winning.Attempt &&
				attempt.ModelExecutionID == history.Winning.ModelExecutionID
		}
		return
	}
}

func appendOpaqueRef(refs []string, candidates ...string) []string {
	for _, candidate := range candidates {
		if candidate = safeOpaqueRef(candidate); candidate != "" {
			refs = append(refs, candidate)
			return refs
		}
	}
	return refs
}

func safeOpaqueRef(candidate string) string {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || strings.ContainsAny(candidate, "/\\\r\n\t") {
		return ""
	}
	return candidate
}

func normalizeRefs(refs []string) []string {
	filtered := refs[:0]
	for _, ref := range refs {
		if ref = strings.TrimSpace(ref); ref != "" {
			filtered = append(filtered, ref)
		}
	}
	slices.Sort(filtered)
	return slices.Compact(filtered)
}

func classifyProjectionError(runID string, err error) error {
	if strings.Contains(err.Error(), "not found") {
		return fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	return fmt.Errorf("%w: project run %q: %v", ErrIntegrity, runID, err)
}
