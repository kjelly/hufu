package team

import (
	"bytes"
	"context"
	"database/sql"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/runtimestore"
)

const runReportVersion = 1
const maxRunReportBytes = 8 << 20

// ErrRunReportUnavailable means no immutable report was captured. Historical
// readers must not substitute a current checkpoint or a loose report.md.
var ErrRunReportUnavailable = errors.New("saved run report unavailable")

// RunReport is a versioned projection of canonical events at run_finished.
// Event identity, rather than the active branch or latest session, owns it.
// Tasks includes occurrences touched by this run, with contracts replayed from
// its ancestors; it excludes untouched tasks from earlier runs.
type RunReport struct {
	StatsAvailable        bool                               `json:"stats_available"`
	MetricsAvailable      bool                               `json:"metrics_available"`
	DeprecatedMemory      []DeprecatedMemoryToolUsage        `json:"deprecated_memory,omitempty"`
	Cost                  *cost.View                         `json:"cost,omitempty"`
	CostUnavailableReason string                             `json:"cost_unavailable_reason,omitempty"`
	ModelProfiles         []modelprofile.TelemetryProjection `json:"model_profiles,omitempty"`
	ContextRouting        ContextManifestSummary             `json:"context_routing"`
	ControlDecisions      []ControlDecisionSummary           `json:"control_decisions,omitempty"`

	SchemaVersion           int                       `json:"schema_version"`
	SourceEventID           string                    `json:"source_event_id"`
	SourceHash              string                    `json:"source_hash"`
	Team                    string                    `json:"team"`
	RunID                   string                    `json:"run_id"`
	SessionID               string                    `json:"session_id"`
	InvocationID            string                    `json:"invocation_id,omitempty"`
	BranchID                string                    `json:"branch_id"`
	StartedAt               time.Time                 `json:"started_at,omitzero"`
	CompletedAt             time.Time                 `json:"completed_at"`
	OriginalPrompt          string                    `json:"original_prompt,omitempty"`
	Result                  *RunResult                `json:"result"`
	Tasks                   []*TodoItem               `json:"tasks"`
	RequiredResourceLockSet *LockedResourceSet        `json:"required_resource_lock_set,omitempty"`
	DecisionPrimitives      []DecisionPrimitiveRecord `json:"decision_primitives,omitempty"`
}

// persistRunReport runs in the event append transaction. Projection failures
// are recorded explicitly without changing the run's acceptance or outcome.
// A storage failure rolls back both rows, retaining the existing terminal
// durability/reconciliation semantics. Idempotent event retries never recapture.
func persistRunReport(ctx context.Context, q runtimestore.Queryer, previous []RunEvent, terminal RunEvent) error {
	tree, exists, err := loadSessionTreeFrom(ctx, q)
	if err == nil && !exists {
		tree = NewSessionTree()
	}
	var body []byte
	if err == nil {
		var report *RunReport
		report, err = projectRunReport(append(slices.Clone(previous), terminal), tree, terminal)
		if err == nil {
			body, err = marshalRunReport(report)
		}
	}
	state, reason := "available", ""
	if err != nil {
		state, reason, body = "unavailable", "canonical report projection could not be captured", nil
	}
	_, writeErr := q.ExecContext(ctx, `INSERT INTO run_reports(source_event_id, source_hash, schema_version, state, reason, snapshot) VALUES(?, ?, ?, ?, ?, ?)`, terminal.ID, terminal.Hash, runReportVersion, state, reason, body)
	return writeErr
}

func marshalRunReport(report *RunReport) ([]byte, error) {
	// Canonical task/result contracts encode durations as integer nanoseconds.
	body, err := json.Marshal(report, jsonv1.FormatDurationAsNano(true), json.Deterministic(true))
	if err == nil && len(body) > maxRunReportBytes {
		return nil, fmt.Errorf("report exceeds %d bytes", maxRunReportBytes)
	}
	return body, err
}

func projectRunReport(events []RunEvent, tree *SessionTree, terminal RunEvent) (*RunReport, error) {
	branch := effectiveBranchID(terminal.BranchID)
	lineage, err := ProjectValidatedEventsForBranch(events, tree, branch)
	if err != nil {
		return nil, err
	}
	end := slices.IndexFunc(lineage, func(e RunEvent) bool { return e.ID == terminal.ID })
	if end < 0 {
		return nil, errors.New("terminal event outside source lineage")
	}
	lineage = lineage[:end+1]
	// Session identity is part of task identity. Fork ancestors remain available
	// for task contracts, but a fresh session cannot inherit an unrelated task.
	lineage = slices.DeleteFunc(slices.Clone(lineage), func(e RunEvent) bool { return e.SessionID != terminal.SessionID })
	report := &RunReport{SchemaVersion: runReportVersion, SourceEventID: terminal.ID, SourceHash: terminal.Hash, RunID: terminal.RunID, SessionID: terminal.SessionID, InvocationID: terminal.InvocationID, BranchID: branch, Tasks: []*TodoItem{}}
	report.CompletedAt, err = time.Parse(time.RFC3339Nano, terminal.Timestamp)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Team    string      `json:"team"`
		Stats   *RunStats   `json:"stats"`
		Metrics *RunMetrics `json:"metrics"`
	}
	if err := json.Unmarshal(terminal.Payload, &payload); err != nil {
		return nil, err
	}
	report.StatsAvailable, report.MetricsAvailable = payload.Stats != nil, payload.Metrics != nil
	report.Team = strings.ToLower(strings.TrimSpace(payload.Team))
	var current []RunEvent
	for _, event := range lineage {
		if event.RunID != terminal.RunID {
			continue
		}
		current = append(current, event)
		switch event.Type {
		case "run_started":
			if report.StartedAt.IsZero() {
				report.StartedAt, _ = time.Parse(time.RFC3339Nano, event.Timestamp)
			}
			if report.Team == "" {
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return nil, err
				}
				report.Team = strings.ToLower(strings.TrimSpace(payload.Team))
			}
		case string(EventUserMessageAdded):
			if report.OriginalPrompt == "" {
				var message SessionMessageEventPayload
				if err := json.Unmarshal(event.Payload, &message); err != nil {
					return nil, err
				}
				report.OriginalPrompt = message.Content
			}
		}
	}
	scoped := ReduceToSessionData(current)
	if scoped.RunResult == nil || scoped.RunResult.RunID != terminal.RunID || scoped.RecoveryRequired {
		return nil, errors.New("canonical run cannot be projected")
	}
	report.Result = scoped.RunResult
	if manifest := report.Result.EvidenceManifest; manifest != nil {
		sealed := *manifest
		if manifest.RunID != terminal.RunID || manifest.ManifestHash == "" {
			return nil, errors.New("report evidence manifest is not bound to this run")
		}
		if err := sealed.Seal(); err != nil {
			return nil, err
		}
		if sealed.ManifestHash != manifest.ManifestHash {
			return nil, errors.New("report evidence manifest digest mismatch")
		}
	}
	report.Tasks, err = reportTasksForRun(lineage, terminal.RunID, report.Team)
	if err != nil {
		return nil, err
	}

	report.RequiredResourceLockSet = scoped.RequiredResourceLockSet
	report.DecisionPrimitives = scoped.DecisionPrimitiveResults
	report.ControlDecisions = SummarizeControlDecisions(current)
	report.DeprecatedMemory = computeDeprecatedMemoryToolUsage(terminal.RunID, current)
	manifests := slices.Clone(scoped.CoordinatorContextManifests)
	for _, manifest := range ContextManifestsFromTodos(report.Tasks) {
		if manifest.RunID == terminal.RunID {
			manifests = mergeContextInjectionManifests(manifests, []ContextInjectionManifest{manifest})
		}
	}
	report.ContextRouting = SummarizeContextManifests(manifests)
	view, costErr := ProjectCostView(lineage, terminal.RunID, "")
	if costErr != nil {
		report.CostUnavailableReason = "canonical cost projection unavailable"
	} else if view.Available && slices.ContainsFunc(current, func(e RunEvent) bool { return isCostEventType(EventType(e.Type)) }) {
		report.Cost = &view
	} else {
		report.CostUnavailableReason = "no canonical cost events captured"
	}
	for _, event := range current {
		if event.Type != string(EventModelProfileResolved) {
			continue
		}
		var profile modelprofile.TelemetryProjection
		if err := json.Unmarshal(event.Payload, &profile); err != nil {
			return nil, err
		}
		report.ModelProfiles = append(report.ModelProfiles, profile)
	}
	return report, nil
}

// reportTasksForRun uses the same occurrence boundary as historical results:
// a fresh task_created in another run starts a new contract, while retry/resume
// transitions retain the originating contract. Task IDs alone are not identities.
func reportTasksForRun(lineage []RunEvent, runID, teamName string) ([]*TodoItem, error) {
	type taskHistory struct {
		origin  string
		events  []RunEvent
		touched bool
	}
	teams := make(map[string]string)
	histories := make(map[string]*taskHistory)
	var order []string
	for _, event := range lineage {
		var metadata struct {
			Team           string `json:"team"`
			ID             string `json:"id"`
			ResetForRetry  bool   `json:"reset_for_retry"`
			ResetForResume bool   `json:"reset_for_resume"`
		}
		if len(event.Payload) > 0 {
			if err := json.Unmarshal(event.Payload, &metadata); err != nil {
				return nil, err
			}
		}
		if metadata.Team != "" {
			name := strings.ToLower(strings.TrimSpace(metadata.Team))
			if prior := teams[event.RunID]; prior != "" && prior != name {
				return nil, errors.New("conflicting report team identity")
			}
			teams[event.RunID] = name
		}
		if teams[event.RunID] != teamName {
			continue
		}
		id := event.TaskID
		if id == "" && strings.HasPrefix(event.Type, "task_") {
			id = metadata.ID
		}
		if id == "" {
			continue
		}
		state := histories[id]
		if state == nil {
			state = &taskHistory{origin: event.RunID}
			histories[id] = state
			order = append(order, id)
		} else if event.Type == "task_created" && state.origin != event.RunID && !metadata.ResetForRetry && !metadata.ResetForResume {
			state = &taskHistory{origin: event.RunID}
			histories[id] = state
		}
		state.events = append(state.events, event)
		if event.RunID == runID {
			state.touched = true
		}
	}
	result := []*TodoItem{}
	for _, id := range order {
		state := histories[id]
		if !state.touched {
			continue
		}
		tasks, err := ReplayTodoList(state.events)
		if err != nil {
			return nil, err
		}
		for _, task := range tasks {
			if task != nil && task.ID == id {
				result = append(result, task)
			}
		}
	}
	return result, nil
}

// LoadRunReport reads a single immutable snapshot and verifies its full binding
// against the validated journal, using one private read-only SQLite snapshot.
// It never migrates, backfills, reads mutable artifacts, or follows report paths.
// Callers select sourceEventID through their authorized branch/team result scope.
func LoadRunReport(ctx context.Context, workspace, sourceEventID string) (*RunReport, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := OpenEventStoreReadOnly(workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(events, func(e RunEvent) bool { return e.ID == sourceEventID && e.Type == "run_finished" })
	if index < 0 {
		return nil, fmt.Errorf("%w: source run_finished not found", ErrRunReportUnavailable)
	}
	var tableExists int
	if err := store.store.DB().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_reports'`).Scan(&tableExists); err != nil {
		return nil, err
	}
	if tableExists == 0 {
		return nil, fmt.Errorf("%w: run predates report capture", ErrRunReportUnavailable)
	}
	var sourceHash, state, reason string
	var version int
	var body []byte
	err = store.store.DB().QueryRowContext(ctx, `SELECT source_hash,schema_version,state,reason,CASE WHEN length(snapshot)<=? THEN snapshot ELSE NULL END FROM run_reports WHERE source_event_id=?`, maxRunReportBytes, sourceEventID).Scan(&sourceHash, &version, &state, &reason, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: run predates report capture", ErrRunReportUnavailable)
	}
	if err != nil {
		return nil, err
	}
	if sourceHash != events[index].Hash || version != runReportVersion {
		return nil, errors.New("run report source binding or schema mismatch")
	}
	if state == "unavailable" {
		return nil, fmt.Errorf("%w: %s", ErrRunReportUnavailable, reason)
	}
	if state != "available" || len(body) == 0 {
		return nil, errors.New("invalid saved run report")
	}
	tree, exists, err := loadSessionTreeFrom(ctx, store.store.DB())
	if err != nil {
		return nil, err
	}
	if !exists {
		tree = NewSessionTree()
	}
	expected, err := projectRunReport(events[:index+1], tree, events[index])
	if err != nil {
		return nil, fmt.Errorf("verify report projection: %w", err)
	}
	encoded, err := marshalRunReport(expected)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(encoded, body) {
		return nil, errors.New("saved run report does not match canonical source events")
	}
	// Return the saved data, rather than silently rebuilding a missing report.
	var report RunReport
	if err := json.Unmarshal(body, &report, jsonv1.FormatDurationAsNano(true)); err != nil {
		return nil, err
	}
	return &report, ctx.Err()
}
