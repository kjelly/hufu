package team

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
)

const (
	EventDiagnosticEpisodeOpened   = "diagnostic_episode_opened"
	EventDiagnosticEpisodeShadowed = "diagnostic_episode_shadowed"
	EventDiagnosticEpisodeObserved = "diagnostic_episode_observed"
	diagnosticActor                = "diagnostic-runtime"
)

type diagnosticEpisodeOpened struct {
	SchemaVersion int                        `json:"schema_version"`
	Mode          string                     `json:"mode"`
	Source        string                     `json:"source"`
	Epoch         belief.Epoch               `json:"epoch"`
	Revisions     belief.DiagnosticRevisions `json:"revisions"`
	FixtureRef    ArtifactRef                `json:"fixture_ref"`
	FixtureDigest string                     `json:"fixture_digest"`
	ModelDigest   string                     `json:"model_digest"`
	PolicyDigest  string                     `json:"policy_digest"`
	MaxEvents     int                        `json:"max_events"`
}

// DiagnosticShadowDecision is a historical non-executable choice. Validation
// belongs to the containing episode; unavailable artifacts cannot verify it.
type DiagnosticShadowDecision struct {
	SchemaVersion int                     `json:"schema_version"`
	EpisodeID     string                  `json:"episode_id"`
	ID            string                  `json:"id"`
	Generation    uint64                  `json:"generation"`
	CursorID      string                  `json:"cursor_id"`
	CursorHash    string                  `json:"cursor_hash"`
	StateDigest   string                  `json:"state_digest"`
	Choice        belief.DiagnosticChoice `json:"choice"`
}

type diagnosticEpisodeObserved struct {
	SchemaVersion int                `json:"schema_version"`
	EpisodeID     string             `json:"episode_id"`
	DecisionID    string             `json:"decision_id"`
	Observation   belief.Observation `json:"observation"`
}

// DiagnosticEpisodeState is rebuilt from a validated branch lineage. It
// represents offline fixture bookkeeping, with no dispatch or budget authority.
type DiagnosticEpisodeState struct {
	diagnosticEpisodeOpened
	Status       string                     `json:"status"`
	CursorID     string                     `json:"cursor_id"`
	CursorHash   string                     `json:"cursor_hash"`
	EventCount   int                        `json:"event_count"`
	State        *belief.State              `json:"state"`
	Observations []belief.Observation       `json:"observations"`
	Decisions    []DiagnosticShadowDecision `json:"decisions"`
	Validation   string                     `json:"validation"`
	Unavailable  string                     `json:"unavailable,omitempty"`
	ReplayDigest string                     `json:"replay_digest,omitempty"`
}

type diagnosticEpisodeReducer struct {
	view    DiagnosticEpisodeState
	fixture *DiagnosticFixture
}

// ProjectDiagnosticEpisodes reads only frozen artifacts, never current team
// configuration, providers or verifiers. The caller validates the global chain
// and selects the branch lineage before invoking this pure event reducer.
func ProjectDiagnosticEpisodes(ctx context.Context, events []RunEvent, artifacts ArtifactStore, runID string) ([]DiagnosticEpisodeState, error) {
	episodes := map[string]*diagnosticEpisodeReducer{}
	for _, event := range events {
		if event.RunID != runID || !strings.HasPrefix(event.Type, "diagnostic_episode_") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Actor != diagnosticActor || event.ID == "" || len(event.Payload) > belief.MaxPolicyBytes {
			return nil, fmt.Errorf("invalid diagnostic episode event envelope")
		}
		if err := applyDiagnosticEvent(ctx, episodes, event, artifacts); err != nil {
			return nil, fmt.Errorf("diagnostic event %s: %w", event.ID, err)
		}
	}
	out := make([]DiagnosticEpisodeState, 0, len(episodes))
	for _, episode := range episodes {
		view := episode.view
		if episode.fixture != nil {
			state, err := belief.Project(episode.fixture.Model, view.Epoch, view.Observations)
			if err != nil {
				return nil, err
			}
			view.State = &state
		}
		if view.Validation == "verified" {
			var err error
			view.ReplayDigest, err = diagnosticContractDigest("hufu-diagnostic-replay-v1", view)
			if err != nil {
				return nil, err
			}
		}
		out = append(out, view)
	}
	slices.SortFunc(out, func(a, b DiagnosticEpisodeState) int { return cmp.Compare(a.Epoch.ID, b.Epoch.ID) })
	return out, nil
}

func applyDiagnosticEvent(ctx context.Context, episodes map[string]*diagnosticEpisodeReducer, event RunEvent, artifacts ArtifactStore) error {
	if event.Type == EventDiagnosticEpisodeOpened {
		var opened diagnosticEpisodeOpened
		if err := json.Unmarshal(event.Payload, &opened, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("invalid diagnostic opening JSON")
		}
		if err := validateDiagnosticOpening(opened, event); err != nil {
			return err
		}
		if episodes[opened.Epoch.ID] != nil || len(episodes) >= 1024 {
			return fmt.Errorf("duplicate diagnostic episode or history capacity exhausted")
		}
		fixture, reason := loadDiagnosticFixture(ctx, artifacts, opened)
		validation := "verified"
		if reason != "" {
			validation = "unavailable"
		}
		episodes[opened.Epoch.ID] = &diagnosticEpisodeReducer{fixture: fixture, view: DiagnosticEpisodeState{diagnosticEpisodeOpened: opened, Status: "ready", CursorID: event.ID, CursorHash: event.Hash, EventCount: 1, Validation: validation, Unavailable: reason, Observations: []belief.Observation{}, Decisions: []DiagnosticShadowDecision{}}}
		return nil
	}
	var id struct {
		EpisodeID string `json:"episode_id"`
	}
	if err := json.Unmarshal(event.Payload, &id); err != nil {
		return fmt.Errorf("invalid diagnostic episode identity")
	}
	episode := episodes[id.EpisodeID]
	if episode == nil || !beliefEventMatchesScope(event, episode.view.Epoch.Scope) || event.SessionID != episode.view.Epoch.Scope.SessionID {
		return fmt.Errorf("diagnostic event scope mismatch")
	}
	if episode.view.EventCount >= episode.view.MaxEvents || episode.view.Status == "stopped" {
		return fmt.Errorf("diagnostic episode is terminal or event capacity exhausted")
	}
	switch event.Type {
	case EventDiagnosticEpisodeShadowed:
		if err := episode.shadow(event); err != nil {
			return err
		}
	case EventDiagnosticEpisodeObserved:
		if err := episode.observe(event); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported diagnostic episode event")
	}
	episode.view.EventCount++
	episode.view.CursorID, episode.view.CursorHash = event.ID, event.Hash
	return nil
}

func validateDiagnosticOpening(opened diagnosticEpisodeOpened, event RunEvent) error {
	if opened.SchemaVersion != 1 || opened.Mode != "shadow" || opened.Source != "offline_fixture" || !validDecisionIdentifier(opened.Epoch.ID) || !beliefEventMatchesScope(event, opened.Epoch.Scope) || opened.Epoch.Scope.SessionID == "" || event.SessionID != opened.Epoch.Scope.SessionID || opened.MaxEvents < 8 || opened.MaxEvents > 4096 {
		return fmt.Errorf("unsupported diagnostic episode opening or scope")
	}
	for _, id := range []string{opened.Epoch.Scope.RunID, opened.Epoch.Scope.BranchID, opened.Epoch.Scope.TaskID, opened.Epoch.Scope.SessionID} {
		if !validDecisionIdentifier(id) {
			return fmt.Errorf("invalid diagnostic episode scope identity")
		}
	}
	if opened.Epoch.Scope.Attempt < 1 {
		return fmt.Errorf("invalid diagnostic episode occurrence")
	}
	for _, digest := range []string{opened.FixtureDigest, opened.ModelDigest, opened.PolicyDigest, opened.Revisions.Subject, opened.Revisions.Source, opened.Revisions.Environment} {
		if !belief.ValidDiagnosticDigest(digest) {
			return fmt.Errorf("invalid diagnostic episode digest")
		}
	}
	if opened.Epoch.ModelDigest != opened.ModelDigest || opened.Epoch.SubjectRevision != opened.Revisions.Subject || opened.Epoch.EnvironmentRevision != opened.Revisions.Environment || opened.FixtureRef.Kind != "diagnostic_fixture" || opened.FixtureRef.ID == "" || opened.FixtureRef.SHA256 != opened.FixtureDigest {
		return fmt.Errorf("invalid diagnostic episode artifact binding")
	}
	return nil
}

func (r *diagnosticEpisodeReducer) nextDecision() (DiagnosticShadowDecision, error) {
	if r.fixture == nil || r.view.Status != "ready" {
		return DiagnosticShadowDecision{}, fmt.Errorf("diagnostic fixture is unavailable or not ready")
	}
	f := r.fixture
	state, err := belief.Project(f.Model, f.Epoch, r.view.Observations)
	if err != nil {
		return DiagnosticShadowDecision{}, err
	}
	choice, err := belief.RankDiagnostic(f.Model, f.Epoch, r.view.Observations, f.Policy)
	if err != nil {
		return DiagnosticShadowDecision{}, err
	}
	if choice.StopReason == "" && r.fixtureBudgetExhausted(choice.CandidateID) {
		choice = belief.DiagnosticChoice{StopReason: "fixture_budget_exhausted", Scores: choice.Scores}
	}
	if choice.StopReason == "" {
		observation, err := diagnosticFixtureObservation(*f, choice.CandidateID)
		if err != nil {
			return DiagnosticShadowDecision{}, err
		}
		data, err := json.Marshal(observation)
		if err != nil {
			return DiagnosticShadowDecision{}, err
		}
		if len(data) > f.Policy.Limits.MaxObservationBytes {
			choice = belief.DiagnosticChoice{StopReason: "fixture_observation_limit", Scores: choice.Scores}
		}
	}
	// These cap offline trace length only. No execution quota is spent.
	if choice.StopReason == "" && r.view.EventCount+3 > r.view.MaxEvents {
		choice = belief.DiagnosticChoice{StopReason: "event_limit", Scores: choice.Scores}
	}
	decision := DiagnosticShadowDecision{SchemaVersion: 1, EpisodeID: f.Epoch.ID, Generation: uint64(len(r.view.Decisions) + 1), CursorID: r.view.CursorID, CursorHash: r.view.CursorHash, StateDigest: state.Digest, Choice: choice}
	decision.ID = beliefIdentity("hufu-diagnostic-shadow-decision-v1", decision)
	return decision, nil
}

func (r *diagnosticEpisodeReducer) fixtureBudgetExhausted(candidateID string) bool {
	var total uint64
	for _, candidate := range r.fixture.Policy.Candidates {
		if candidate.ID == candidateID || slices.ContainsFunc(r.view.Observations, func(o belief.Observation) bool { return o.ExperimentID == candidate.ExperimentID }) {
			total += candidate.CostUnits
		}
	}
	return len(r.view.Observations) >= r.fixture.Policy.Limits.MaxAttempts || total > r.fixture.Policy.Limits.MaxCostUnits
}

func (r *diagnosticEpisodeReducer) shadow(event RunEvent) error {
	var decision DiagnosticShadowDecision
	if err := json.Unmarshal(event.Payload, &decision, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid diagnostic shadow JSON")
	}
	if r.view.Status != "ready" || decision.SchemaVersion != 1 || decision.Generation != uint64(len(r.view.Decisions)+1) || decision.CursorID != r.view.CursorID || decision.CursorHash != r.view.CursorHash || !belief.ValidDiagnosticDigest(decision.StateDigest) || len(decision.Choice.Scores) > belief.MaxExperiments || (decision.Choice.CandidateID == "") == (decision.Choice.StopReason == "") {
		return fmt.Errorf("invalid diagnostic shadow generation or cursor")
	}
	if decision.Choice.StopReason != "" && !slices.Contains([]string{"distribution_unavailable", "threshold_reached", "round_limit", "no_candidate", "no_information_gain", "fixture_budget_exhausted", "fixture_observation_limit", "event_limit"}, decision.Choice.StopReason) {
		return fmt.Errorf("unsupported diagnostic stop reason")
	}
	identity := decision
	identity.ID = ""
	if decision.ID != beliefIdentity("hufu-diagnostic-shadow-decision-v1", identity) {
		return fmt.Errorf("invalid diagnostic shadow identity")
	}
	if r.fixture != nil {
		expected, err := r.nextDecision()
		if err != nil || !reflect.DeepEqual(expected, decision) {
			return fmt.Errorf("diagnostic shadow does not match frozen calculation")
		}
	}
	r.view.Decisions = append(r.view.Decisions, decision)
	r.view.Status = "awaiting_fixture_observation"
	if decision.Choice.StopReason != "" {
		r.view.Status = "stopped"
	}
	return nil
}

func (r *diagnosticEpisodeReducer) observe(event RunEvent) error {
	var observed diagnosticEpisodeObserved
	if err := json.Unmarshal(event.Payload, &observed, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("invalid diagnostic observation JSON")
	}
	if r.view.Status != "awaiting_fixture_observation" || observed.SchemaVersion != 1 || len(r.view.Observations) >= belief.MaxObservations {
		return fmt.Errorf("diagnostic observation has no pending shadow choice")
	}
	decision := r.view.Decisions[len(r.view.Decisions)-1]
	if observed.DecisionID != decision.ID || !validDecisionIdentifier(observed.Observation.ExperimentID) || !belief.ValidDiagnosticDigest(observed.Observation.ID) || !belief.ValidDiagnosticDigest(observed.Observation.SourceID) || !slices.Contains([]string{"pass", "fail"}, observed.Observation.OutcomeID) {
		return fmt.Errorf("invalid diagnostic observation decision binding")
	}
	if slices.ContainsFunc(r.view.Observations, func(o belief.Observation) bool {
		return o.ID == observed.Observation.ID || o.ExperimentID == observed.Observation.ExperimentID || o.SourceID == observed.Observation.SourceID
	}) {
		return fmt.Errorf("duplicate diagnostic observation")
	}
	if r.fixture != nil {
		expected, err := diagnosticFixtureObservation(*r.fixture, decision.Choice.CandidateID)
		if err != nil || expected != observed.Observation {
			return fmt.Errorf("diagnostic observation does not match frozen fixture")
		}
	}
	r.view.Observations = append(r.view.Observations, observed.Observation)
	r.view.Status = "ready"
	return nil
}
