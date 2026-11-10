package inspect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"

	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

// The caller has already checked the exact branch lineage against the native
// intent and source prefix. This adapter exposes metadata, never source bodies
// or new recovery authority.
func derivedForkBranchView(lineage Lineage) (*operatorpkg.DerivedForkView, *team.SessionData, error) {
	if !slices.ContainsFunc(lineage.Events, func(event IndexedEvent) bool { return event.Event.Type == string(team.EventDerivedForkCreated) }) {
		return nil, nil, nil
	}
	raw := rawIndexedEvents(lineage.Events)
	canonical := team.ReduceToSessionData(raw)
	ref := canonical.DerivedFork
	if ref == nil {
		return nil, nil, fmt.Errorf("%w: native derived fork projection is unavailable", ErrIntegrity)
	}
	var payload team.DerivedForkCreatedPayload
	for _, indexed := range lineage.Events {
		if indexed.Event.ID != ref.EventID {
			continue
		}
		if indexed.Event.Hash != ref.EventHash || json.Unmarshal(indexed.Event.Payload, &payload) != nil {
			return nil, nil, fmt.Errorf("%w: derived fork source changed", ErrIntegrity)
		}
		break
	}
	intent := payload.Intent
	view := &operatorpkg.DerivedForkView{
		BranchID: ref.BranchID, EventID: ref.EventID, EventHash: ref.EventHash, SnapshotID: ref.SnapshotID,
		SourceBranchID: intent.SourceBranchID, SourceEventID: intent.SourceEventID, SourceEventHash: intent.SourceEventHash,
		SourceSnapshotID: intent.SourceSnapshotID, SourceRootTreeHash: intent.SourceRootTreeHash,
		CaptureOrigin: intent.CaptureOrigin, CaptureSnapshotID: intent.CaptureSnapshotID,
		CaptureEventID: intent.CaptureEventID, CaptureEventHash: intent.CaptureEventHash,
		AwaitingNewRequest: ref.AwaitingNewRequest, FirstCycleID: ref.FirstCycleID, SourceRunMembership: "unavailable",
	}
	for _, indexed := range lineage.Events {
		if indexed.Event.Type == string(team.EventRunStarted) {
			view.SourceRunID, view.SourceRunMembership = indexed.Event.RunID, "visible"
		}
		if indexed.Event.ID == view.SourceEventID {
			return view, canonical, nil
		}
	}
	return nil, nil, fmt.Errorf("%w: derived fork source cutoff is unavailable", ErrIntegrity)
}

func rawIndexedEvents(events []IndexedEvent) []team.RunEvent {
	raw := make([]team.RunEvent, len(events))
	for index, event := range events {
		raw[index] = event.Event
	}
	return raw
}

func bindPendingDerivedFork(request operatorpkg.BindingRequest, lineage Lineage, view *operatorpkg.DerivedForkView, canonical, live *team.SessionData) (BoundReadTarget, error) {
	var created team.RunEvent
	for _, indexed := range lineage.Events {
		if indexed.Event.ID == view.EventID {
			created = indexed.Event
			break
		}
	}
	if err := requireCompatibleID("session", request.SessionID, created.SessionID); err != nil {
		return BoundReadTarget{}, err
	}
	projectID, teamName, err := persistedScopeIDs(lineage, view.SourceRunID)
	if err != nil {
		return BoundReadTarget{}, err
	}
	var payload team.DerivedForkCreatedPayload
	if err := json.Unmarshal(created.Payload, &payload); err != nil {
		return BoundReadTarget{}, fmt.Errorf("%w: derived fork metadata is invalid", ErrIntegrity)
	}
	if err := requireCompatibleID("team", teamName, payload.Intent.Team); err != nil {
		return BoundReadTarget{}, err
	}
	teamName = payload.Intent.Team
	for _, requested := range []string{request.TeamID, request.Workspace.TeamName, lineage.SelectedTeam} {
		if err := requireCompatibleID("team", requested, teamName); err != nil {
			return BoundReadTarget{}, err
		}
	}
	if err := requireCompatibleID("project", request.ProjectID, projectID); err != nil {
		return BoundReadTarget{}, err
	}
	if lineage.BranchID == lineage.ActiveBranchID && live != nil {
		if err := team.CompareCanonicalProjection(live, rawIndexedEvents(lineage.Events)); err != nil || live.RecoveryRequired || live.PendingTerminalCommit != nil {
			return BoundReadTarget{}, fmt.Errorf("%w: pending derived checkpoint requires inspection", ErrIntegrity)
		}
	}
	return BoundReadTarget{Scope: operatorpkg.ResolvedScope{
		RequestedPath: request.Workspace.RequestedPath, RequestedSemantics: request.Workspace.RequestedSemantics,
		WorkspaceExact: request.Workspace.WorkspaceExact, WorkspaceRoot: request.Workspace.WorkspaceRoot,
		ProjectDir: request.Workspace.ProjectDir, ProjectID: projectID, TeamName: teamName,
		SessionID: created.SessionID, BranchID: lineage.BranchID, SelectionSource: "derived_fork", BindingStatus: "verified",
	}, Lineage: lineage, Session: canonical, DerivedFork: view}, nil
}

func pendingDerivedForkOverview(ctx context.Context, query InspectQuery, bound BoundReadTarget) (*Envelope, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	anchor := bound.Lineage.Events[len(bound.Lineage.Events)-1]
	snapshot := operatorpkg.OperatorSnapshot{
		SchemaVersion: operatorpkg.SchemaVersion, Scope: bound.Scope, DerivedFork: bound.DerivedFork,
		Activity: operatorpkg.DeriveActivity(operatorpkg.ActivityFacts{
			BindingStatus: "verified", EventChain: "verified", RequiredProjection: "consistent", PendingInput: true,
			RawReasonCodes: []string{"awaiting_new_request"},
		}),
		Outcome:   operatorpkg.OutcomeView{CompletionState: "awaiting_new_request", AcceptanceState: "not_evaluated"},
		Integrity: operatorpkg.IntegrityView{Status: "valid", EventChain: "verified", Projection: "consistent"},
		Freshness: operatorpkg.FreshnessView{QueriedAt: time.Now().UTC().Format(time.RFC3339Nano),
			EventID: anchor.Event.ID, EventHash: anchor.Event.Hash, EventOrdinal: anchor.Ordinal, LiveState: "not_applicable"},
		Attention: operatorpkg.DeriveAttention(operatorpkg.ActivityWaitingInput, "", "valid", ""),
		Learning:  operatorpkg.LearningView{Status: "not_applicable"},
	}
	var err error
	snapshot.PrimaryAction, snapshot.SecondaryActions, err = operatorpkg.SelectActions(operatorpkg.ActionSelectionFacts{Snapshot: snapshot})
	if err != nil {
		return nil, err
	}
	if snapshot.PrimaryAction != nil {
		snapshot.PrimaryAction.SourceRefs = []string{bound.DerivedFork.EventID, bound.DerivedFork.SourceEventID}
	}
	snapshot, err = operatorpkg.FinalizeSnapshot(snapshot)
	if err != nil {
		return nil, fmt.Errorf("%w: pending derived overview: %v", ErrIntegrity, err)
	}
	return envelope(KindOverview, query, bound.Scope.BranchID, OverviewData{
		Operation: OperationView{Status: "succeeded"}, Snapshot: &snapshot, Warnings: []operatorpkg.DiagnosticView{},
	}), nil
}
