package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
)

// DiagnosticJournal owns the offline episode namespace in the canonical event
// journal. It reuses branch validation and global-head CAS; it cannot dispatch
// tasks, admit runtime receipts, call providers or reserve execution budgets.
type DiagnosticJournal struct{ scope *BeliefJournal }

func NewDiagnosticJournal(journal ConditionalEventJournal, artifacts ArtifactStore, tree *SessionTree, branchID, runID, sessionID string) (*DiagnosticJournal, error) {
	scope, err := NewBeliefJournal(journal, artifacts, tree, branchID, runID, sessionID)
	if err != nil || sessionID == "" {
		return nil, fmt.Errorf("diagnostic journal requires a pinned branch, run, session and conditional journal")
	}
	return &DiagnosticJournal{scope: scope}, nil
}

// OpenFixture stores all immutable replay inputs before committing the opening
// event. A crash between those operations can leave an unreferenced artifact,
// never an event that refers to inputs that were not successfully persisted.
func (j *DiagnosticJournal) OpenFixture(ctx context.Context, fixture DiagnosticFixture) error {
	s := j.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	if fixture.Epoch.Scope.RunID != s.runID || fixture.Epoch.Scope.BranchID != s.branchID || fixture.Epoch.Scope.SessionID != s.sessionID {
		return fmt.Errorf("diagnostic fixture does not match journal scope")
	}
	data, digest, err := diagnosticFixtureBytes(fixture)
	if err != nil {
		return err
	}
	events, head, err := s.policyEvents(ctx)
	if err != nil {
		return err
	}
	views, err := ProjectDiagnosticEpisodes(ctx, events, s.artifacts, s.runID)
	if err != nil {
		return err
	}
	for _, view := range views {
		if view.Epoch.ID == fixture.Epoch.ID {
			if view.FixtureDigest != digest || view.Unavailable != "" {
				return fmt.Errorf("diagnostic episode identity conflicts or frozen fixture is unavailable")
			}
			return nil
		}
	}
	stored, err := s.artifacts.Put(ctx, PutArtifactRequest{Content: data, Kind: "diagnostic_fixture", Role: "fixture", MediaType: "application/json", Path: "belief/diagnostic/" + digest + ".json", RunID: s.runID})
	if err != nil {
		return err
	}
	modelDigest, _ := belief.ModelDigest(fixture.Model)
	policyDigest, _ := belief.DiagnosticPolicyDigest(fixture.Policy)
	opened := diagnosticEpisodeOpened{SchemaVersion: 1, Mode: "shadow", Source: "offline_fixture", Epoch: fixture.Epoch, Revisions: fixture.Revisions, ModelDigest: modelDigest, PolicyDigest: policyDigest, MaxEvents: fixture.Policy.Limits.MaxEvents, FixtureDigest: digest, FixtureRef: ArtifactRef{ID: stored.ID, SHA256: stored.SHA256, Kind: "diagnostic_fixture"}}
	return j.append(ctx, events, head, fixture.Epoch, EventDiagnosticEpisodeOpened, "open", opened)
}

// AdvanceFixture commits exactly one shadow decision or one fixture
// observation. The required cursor fences stale callers, including retries
// after a lost append acknowledgment. Recovery reads the durable state first.
func (j *DiagnosticJournal) AdvanceFixture(ctx context.Context, episodeID, expectedCursorID string) error {
	s := j.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	events, head, err := s.policyEvents(ctx)
	if err != nil {
		return err
	}
	views, err := ProjectDiagnosticEpisodes(ctx, events, s.artifacts, s.runID)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(views, func(v DiagnosticEpisodeState) bool { return v.Epoch.ID == episodeID })
	if i < 0 {
		return fmt.Errorf("diagnostic episode not found")
	}
	view := views[i]
	if view.Epoch.Scope.BranchID != s.branchID || view.Epoch.Scope.SessionID != s.sessionID || view.Unavailable != "" || expectedCursorID == "" || view.CursorID != expectedCursorID {
		return fmt.Errorf("diagnostic episode is inherited, unavailable or cursor is stale")
	}
	fixture, reason := loadDiagnosticFixture(ctx, s.artifacts, view.diagnosticEpisodeOpened)
	if reason != "" {
		return fmt.Errorf("diagnostic fixture unavailable: %s", reason)
	}
	r := diagnosticEpisodeReducer{view: view, fixture: fixture}
	switch view.Status {
	case "ready":
		decision, err := r.nextDecision()
		if err != nil {
			return err
		}
		return j.append(ctx, events, head, view.Epoch, EventDiagnosticEpisodeShadowed, fmt.Sprintf("decision:%d", decision.Generation), decision)
	case "awaiting_fixture_observation":
		decision := view.Decisions[len(view.Decisions)-1]
		observation, err := diagnosticFixtureObservation(*fixture, decision.Choice.CandidateID)
		if err != nil {
			return err
		}
		payload := diagnosticEpisodeObserved{SchemaVersion: 1, EpisodeID: episodeID, DecisionID: decision.ID, Observation: observation}
		return j.append(ctx, events, head, view.Epoch, EventDiagnosticEpisodeObserved, "observation:"+observation.ID, payload)
	default:
		return fmt.Errorf("diagnostic episode is terminal")
	}
}

func (j *DiagnosticJournal) append(ctx context.Context, events []RunEvent, head string, epoch belief.Epoch, kind, key string, payload any) error {
	s := j.scope
	data, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return err
	}
	candidate := RunEvent{ID: "diagnostic-preflight", Actor: diagnosticActor, Type: kind, RunID: s.runID, SessionID: s.sessionID, BranchID: s.branchID, TaskID: epoch.Scope.TaskID, Attempt: epoch.Scope.Attempt, IdempotencyKey: "diagnostic:" + s.runID + ":" + epoch.ID + ":" + key, Payload: data}
	views, err := ProjectDiagnosticEpisodes(ctx, append(slices.Clone(events), candidate), s.artifacts, s.runID)
	if err != nil {
		return err
	}
	for _, view := range views {
		if view.Epoch.ID == epoch.ID && view.Unavailable != "" {
			return fmt.Errorf("diagnostic append cannot verify frozen inputs: %s", view.Unavailable)
		}
	}
	candidate.ID = ""
	_, err = s.journal.(ConditionalEventJournal).AppendAtHead(ctx, candidate, head)
	return err
}

// Replay never repairs, writes artifacts or advances an episode.
func (j *DiagnosticJournal) Replay(ctx context.Context) ([]DiagnosticEpisodeState, error) {
	s := j.scope
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := s.events(ctx)
	if err != nil {
		return nil, err
	}
	return ProjectDiagnosticEpisodes(ctx, events, s.artifacts, s.runID)
}
