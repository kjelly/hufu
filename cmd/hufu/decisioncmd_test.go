package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/team"
)

// resetDecisionCLIFlags clears every decision subcommand's package-level flag
// variable. pflag only assigns a flag's variable when that flag appears in
// argv, so a value left by an earlier case would otherwise leak into the next
// one. Call this at the start of every case.
func resetDecisionCLIFlags() {
	decisionWorkspace, decisionJSON, decisionAll = "", false, false
	decisionResolveOutcome, decisionResolveBy, decisionResolveNotes = "", "", ""
	decisionResolveEvidence, decisionResolveLessons = nil, nil
	decisionResolveForecast = 0
	decisionAssumeStatus, decisionAssumeNote = "", ""
}

// buildDecisionCLIFixture writes a workspace holding two unresolved decisions
// and one artifact, through the same exported primitives the runtime uses.
func buildDecisionCLIFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i, entry := range []team.DecisionIndexEntry{
		{
			DecisionID: "dec-1", RunID: "run-42", TaskID: "t1", Profile: "high-stakes",
			EvidenceHash: "abc123", Question: "Should we migrate now?",
			FinalOption: "migrate", Probability: 0.7, ForecastRequired: true,
			FinalizationMode: "coordinator", FinalizationIdentity: "coordinator", FinalizationOutcome: "selected",
			FinalizationReason:      "api_key=stage4-cli-secret",
			FinalizationWarnings:    []string{"api_key=stage4-cli-warning"},
			FalsificationConditions: []string{"ingest lag exceeds 5m"},
			RecordPath:              "decisions/dec-1.json", CreatedAt: base,
		},
		{
			DecisionID: "dec-2", RunID: "run-43", Profile: "standard",
			Question: "Roll back?", FinalOption: "defer", Probability: 0.4,
			CreatedAt: base.Add(time.Hour),
			Assumptions: []team.DecisionAssumption{
				{ID: "A1", Statement: "traffic stays flat", Critical: true},
				{ID: "A2", Statement: "the vendor API is stable"},
			},
		},
	} {
		if err := index.Append(entry); err != nil {
			t.Fatalf("seeding entry %d: %v", i, err)
		}
	}

	metaDir := filepath.Join(workspace, "logs", "artifacts", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"art-9","path":"reports/lag.json","sha256":"deadbeef","media_type":"application/json"}`
	if err := os.WriteFile(filepath.Join(metaDir, "art-9.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// runDecisionCLI executes through a fresh root, the way cmd/hufu's other CLI
// tests do. Calling Execute() on the subcommand would walk up to the shared
// root and re-run whatever args another test left there.
func runDecisionCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"decision"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestDecisionListShowsUnresolvedDecisions(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "list", "-w", workspace)
	if err != nil {
		t.Fatalf("decision list = %v", err)
	}
	for _, want := range []string{"dec-1", "dec-2", "high-stakes", "unresolved"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDecisionListJSON(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "list", "-w", workspace, "--json")
	if err != nil {
		t.Fatalf("decision list --json = %v", err)
	}
	var payload struct {
		Index   string                    `json:"index"`
		Count   int                       `json:"count"`
		Entries []team.DecisionIndexEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not a single JSON object: %v\n%s", err, out)
	}
	if payload.Count != 2 || len(payload.Entries) != 2 {
		t.Fatalf("payload = %#v, want two entries", payload)
	}
	if !strings.HasSuffix(payload.Index, filepath.Join("logs", "decisions", "index.jsonl")) {
		t.Fatalf("index path = %q", payload.Index)
	}
	entry := payload.Entries[0]
	if entry.FinalizationMode != "coordinator" || entry.FinalizationIdentity != "coordinator" || entry.FinalizationOutcome != "selected" {
		t.Fatalf("finalization JSON projection = %#v", entry)
	}
	if strings.Contains(out, "stage4-cli-secret") {
		t.Fatalf("decision JSON exposed finalization secret: %s", out)
	}
	if strings.Contains(out, "stage4-cli-warning") || !strings.Contains(out, "[REDACTED]") {
		t.Fatalf("decision JSON finalization warning redaction = %s", out)
	}

	resetDecisionCLIFlags()
	show, err := runDecisionCLI(t, "show", "dec-1", "-w", workspace, "--json")
	if err != nil {
		t.Fatalf("decision show --json = %v", err)
	}
	var showEntry team.DecisionIndexEntry
	if err := json.Unmarshal([]byte(show), &showEntry); err != nil {
		t.Fatalf("show JSON is not a decision entry: %v\n%s", err, show)
	}
	if strings.Contains(show, "stage4-cli-secret") || strings.Contains(show, "stage4-cli-warning") || !strings.Contains(show, "[REDACTED]") {
		t.Fatalf("decision show JSON exposed finalization secret/warning: %s", show)
	}
}

func TestDecisionListAndShowProjectRedactedFinalization(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)
	list, err := runDecisionCLI(t, "list", "-w", workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FINALIZER", "coordinator/coordinator/selected"} {
		if !strings.Contains(list, want) {
			t.Fatalf("list missing finalization projection %q:\n%s", want, list)
		}
	}
	if strings.Contains(list, "stage4-cli-secret") {
		t.Fatalf("list exposed finalization secret: %s", list)
	}

	resetDecisionCLIFlags()
	show, err := runDecisionCLI(t, "show", "dec-1", "-w", workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Finalizer:  coordinator/coordinator/selected", "Reason:"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show missing finalization projection %q:\n%s", want, show)
		}
	}
	if strings.Contains(show, "stage4-cli-secret") {
		t.Fatalf("show exposed finalization secret: %s", show)
	}
}

// Resolving with evidence that exists in the store marks the outcome verified.
func TestDecisionResolveWithResolvableEvidenceIsVerified(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace,
		"--outcome", "succeeded", "--evidence", "deadbeef",
		"--lesson", "lag never exceeded 2m", "--resolved-by", "ops")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: true") {
		t.Fatalf("output did not report a verified outcome:\n%s", out)
	}
	if !strings.Contains(out, "The decision record was not modified.") {
		t.Fatalf("output did not state the record was untouched:\n%s", out)
	}

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	entry, found, err := index.Get("dec-1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if entry.Outcome == nil || !entry.Outcome.Verified {
		t.Fatalf("stored outcome = %#v, want verified", entry.Outcome)
	}
	if entry.Outcome.Forecast != 0.7 {
		t.Fatalf("Forecast = %v, want the decision's own probability", entry.Outcome.Forecast)
	}
	// Resolving must not change what was decided.
	if entry.FinalOption != "migrate" || entry.EvidenceHash != "abc123" {
		t.Fatalf("resolution changed the decision: %#v", entry)
	}
}

// An operator's assertion, with no resolvable evidence, is recorded but never
// counted as verified.
func TestDecisionResolveWithoutEvidenceIsUnverified(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "resolve", "dec-2", "-w", workspace,
		"--outcome", "failed", "--resolved-by", "a very confident operator")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: false") {
		t.Fatalf("an assertion was reported as verified:\n%s", out)
	}

	resetDecisionCLIFlags()
	workspace2 := buildDecisionCLIFixture(t)
	out, err = runDecisionCLI(t, "resolve", "dec-2", "-w", workspace2,
		"--outcome", "failed", "--evidence", "notinthestore")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: false") {
		t.Fatalf("an unresolvable digest was reported as verified:\n%s", out)
	}
}

func TestDecisionResolveRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing outcome",
			args:    []string{"resolve", "dec-1"},
			wantErr: "--outcome is required",
		},
		{
			name:    "undeclared outcome",
			args:    []string{"resolve", "dec-1", "--outcome", "went-fine"},
			wantErr: "is not one of",
		},
		{
			name:    "unknown decision",
			args:    []string{"resolve", "dec-99", "--outcome", "succeeded"},
			wantErr: "is not in",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDecisionCLIFlags()
			workspace := buildDecisionCLIFixture(t)
			args := append(append([]string(nil), tt.args...), "-w", workspace)
			_, err := runDecisionCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decision resolve = %v, want %q", err, tt.wantErr)
			}
			var exit *decisionExitError
			if !errorsAsDecisionExit(err, &exit) || exit.ProcessExitCode() != 2 {
				t.Fatalf("error = %#v, want a decisionExitError with code 2", err)
			}
		})
	}
}

// An outcome is recorded once; a second attempt is refused rather than
// overwriting what was already observed.
func TestDecisionResolveIsRecordedOnce(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	if _, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace, "--outcome", "succeeded"); err != nil {
		t.Fatal(err)
	}
	resetDecisionCLIFlags()
	_, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace, "--outcome", "failed")
	if err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("second resolution = %v, want a refusal", err)
	}
}

func TestDecisionShow(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "show", "dec-1", "-w", workspace)
	if err != nil {
		t.Fatalf("decision show = %v", err)
	}
	for _, want := range []string{"dec-1", "run-42", "migrate", "ingest lag exceeds 5m", "unresolved"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	resetDecisionCLIFlags()
	_, err = runDecisionCLI(t, "show", "dec-99", "-w", workspace)
	if err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("show on an unknown decision = %v, want a not-found error", err)
	}
}

// explainFixtureJudges answers judge-1 with a resolved AgentID and judge-2
// with none, so the fixture exercises both a routed and an unrouted stage.
type explainFixtureJudges struct{}

func (explainFixtureJudges) RunJudge(_ context.Context, req team.JudgeRequest) (team.DecisionOpinion, error) {
	opinion := team.DecisionOpinion{
		OptionScores: []team.OptionScore{
			{OptionID: "migrate", Criteria: map[string]float64{"cost": 8, "risk": 8}},
			{OptionID: "wait", Criteria: map[string]float64{"cost": 4, "risk": 4}},
		},
		PreferredOption: "migrate", SuccessProbability: 0.8,
	}
	if req.JudgeID == "judge-1" {
		opinion.AgentID = "judge-cand-a"
	}
	return opinion, nil
}

// explainFixtureReference answers with a resolved AgentID, standing in for a
// capability-routed reference role.
type explainFixtureReference struct{}

func (explainFixtureReference) RunReferenceEvidence(_ context.Context, _ team.ReferenceEvidenceRequest) (team.ReferenceEvidenceDraft, error) {
	return team.ReferenceEvidenceDraft{
		SchemaVersion: team.ReferenceEvidenceSchemaVersion,
		Entries: []team.ReferenceBaseRateDraft{{
			ReferenceClass: "comparable migrations", Metric: "success_rate", SampleSize: 20,
			Distribution: team.DistributionSummary{Mean: .8, Median: .8, P10: .5, P90: .95},
			Source:       team.ReferenceSourceDeclaration{Name: "internal survey"},
		}},
		AgentID: "research-worker",
	}, nil
}

// testEventJournal adapts the exported *team.EventStore (whose methods take
// no context) to team.EventJournal, exactly like the runtime's own
// unexported eventStoreJournal does, so a test outside package team can wire
// a real event store into DecisionServices.Journal.
type testEventJournal struct{ store *team.EventStore }

func (j testEventJournal) Append(_ context.Context, event team.RunEvent) (team.RunEvent, error) {
	return j.store.AppendPersistedContext(context.Background(), event)
}

func (j testEventJournal) ReadEvents(context.Context) ([]team.RunEvent, error) {
	return j.store.ReadEvents()
}

func (j testEventJournal) VerifyHashChain(context.Context) error {
	return j.store.VerifyHashChain()
}

// buildDecisionExplainFixture runs a real DecisionEngine end to end (judge
// and reference stages, one routed and one legacy-sidecar-shaped) against a
// real event log, artifact store, and decision index, exactly like a
// finalized run would leave a workspace. `decision explain` re-validates a
// row against the durable journal before showing it, so a hand-written index
// row is not enough; this must be a genuinely finalized decision.
func buildDecisionExplainFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()

	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	eventStore, err := team.NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eventStore.Close() })
	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}

	engine := team.NewDecisionEngine(team.DecisionServices{
		Judges:            explainFixtureJudges{},
		ReferenceEvidence: explainFixtureReference{},
		Journal:           testEventJournal{store: eventStore},
		Store:             store,
		Index:             index,
	})

	policy := team.DecisionPolicy{
		IndependentJudgments: 2,
		ContextIsolation:     agent.DecisionIsolationStrict,
		ScoreScale:           agent.DecisionScoreScale,
		Aggregation:          team.AggregationPolicy{Method: agent.AggregationMeanScore},
		Criteria:             []team.DecisionCriterion{{ID: "cost", Weight: 1}, {ID: "risk", Weight: 1}},
		MaxRounds:            1,
	}
	policy.OutsideView.Required = true
	policy.OutsideView.ReferenceEvidence = true

	req := team.DecisionRequest{
		DecisionID: "dec-1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Profile: "standard",
		Policy:   policy,
		Question: "Should we migrate the storage backend?",
		Options: []team.DecisionOption{
			{ID: "migrate", Kind: team.OptionExecute, Title: "Migrate now"},
			{ID: "wait", Kind: team.OptionDefer, Title: "Wait"},
		},
		Role: "You are a systems reviewer.",
	}
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}
	return workspace
}

func TestDecisionExplain(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionExplainFixture(t)

	out, err := runDecisionCLI(t, "explain", "dec-1", "-w", workspace)
	if err != nil {
		t.Fatalf("decision explain = %v\n%s", err, out)
	}
	for _, want := range []string{
		"dec-1", "reference", "research-worker",
		"judge-1", "judge-cand-a", "judge-2",
		"true", "false",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	resetDecisionCLIFlags()
	jsonOut, err := runDecisionCLI(t, "explain", "dec-1", "-w", workspace, "--json")
	if err != nil {
		t.Fatalf("decision explain --json = %v\n%s", err, jsonOut)
	}
	var payload struct {
		DecisionID string                      `json:"decision_id"`
		Bindings   []team.DecisionAgentBinding `json:"bindings"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Fatalf("decode JSON output: %v\n%s", err, jsonOut)
	}
	if payload.DecisionID != "dec-1" || len(payload.Bindings) != 3 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Bindings[0].Stage != "reference" || payload.Bindings[0].AgentID != "research-worker" || !payload.Bindings[0].Routed {
		t.Fatalf("bindings[0] = %#v", payload.Bindings[0])
	}
	if payload.Bindings[1].Stage != "judge" || payload.Bindings[1].Ordinal != "judge-1" || payload.Bindings[1].AgentID != "judge-cand-a" || !payload.Bindings[1].Routed {
		t.Fatalf("bindings[1] = %#v", payload.Bindings[1])
	}
	if payload.Bindings[2].Stage != "judge" || payload.Bindings[2].Ordinal != "judge-2" || payload.Bindings[2].Routed {
		t.Fatalf("bindings[2] = %#v, want an unrouted legacy-sidecar judge-2 opinion", payload.Bindings[2])
	}

	resetDecisionCLIFlags()
	_, err = runDecisionCLI(t, "explain", "dec-99", "-w", workspace)
	if err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("explain on an unknown decision = %v, want a not-found error", err)
	}
}

func TestDecisionListOnEmptyWorkspace(t *testing.T) {
	resetDecisionCLIFlags()
	out, err := runDecisionCLI(t, "list", "-w", t.TempDir())
	if err != nil {
		t.Fatalf("decision list on an empty workspace = %v", err)
	}
	if !strings.Contains(out, "No unresolved decisions") {
		t.Fatalf("output = %q, want an empty-state message", out)
	}
}

// errorsAsDecisionExit is a local errors.As without importing errors in the
// test's hot path assertions.
func errorsAsDecisionExit(err error, target **decisionExitError) bool {
	if exit, ok := err.(*decisionExitError); ok {
		*target = exit
		return true
	}
	return false
}

// The operator source works after the run that formed the decision has exited,
// which is the whole reason it exists (spec §18.1 source 3).
func TestDecisionAssumeRecordsAnOperatorCheck(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "assume", "dec-2", "A1", "-w", workspace,
		"--status", "contradicted", "--note", "traffic doubled in week 2")
	if err != nil {
		t.Fatalf("decision assume = %v\n%s", err, out)
	}
	if !strings.Contains(out, "critical assumption") {
		t.Fatalf("output did not flag the critical assumption:\n%s", out)
	}
	if !strings.Contains(out, "The decision record was not modified.") {
		t.Fatalf("output did not state the record was untouched:\n%s", out)
	}

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	entry, found, err := index.Get("dec-2")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if entry.CriticalAssumptionContradicted() != "A1" {
		t.Fatalf("stored assumptions = %#v", entry.Assumptions)
	}
	if entry.FinalOption != "defer" {
		t.Fatalf("the operator check changed the decision: %#v", entry)
	}
	if len(entry.AssumptionNotes) != 1 {
		t.Fatalf("notes = %v, want the reason recorded", entry.AssumptionNotes)
	}
}

func TestDecisionAssumeRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing status",
			args:    []string{"assume", "dec-2", "A1"},
			wantErr: "--status is required",
		},
		{
			name:    "unreportable status",
			args:    []string{"assume", "dec-2", "A1", "--status", "stale"},
			wantErr: "not reportable",
		},
		{
			name:    "undeclared assumption",
			args:    []string{"assume", "dec-2", "A9", "--status", "supported"},
			wantErr: "not declared on this decision",
		},
		{
			name:    "decision without assumptions",
			args:    []string{"assume", "dec-1", "A1", "--status", "supported"},
			wantErr: "declares no assumptions",
		},
		{
			name:    "unknown decision",
			args:    []string{"assume", "dec-99", "A1", "--status", "supported"},
			wantErr: "is not in",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDecisionCLIFlags()
			workspace := buildDecisionCLIFixture(t)
			args := append(append([]string(nil), tt.args...), "-w", workspace)
			_, err := runDecisionCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decision assume = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// Metrics are a projection over durable state, so a workspace with no
// decisions reports zeroes rather than failing.
func TestDecisionStatsOnEmptyWorkspace(t *testing.T) {
	resetDecisionCLIFlags()
	out, err := runDecisionCLI(t, "stats", "-w", t.TempDir())
	if err != nil {
		t.Fatalf("decision stats = %v", err)
	}
	if !strings.Contains(out, "Decisions formed:      0") {
		t.Fatalf("output = %q, want a zeroed report", out)
	}
	if !strings.Contains(out, "Phase 5 needs") {
		t.Fatalf("output did not report the Phase 5 entry condition:\n%s", out)
	}
}

func TestDecisionStatsReportsIndexMetrics(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	// Resolve one decision so the outcome sample is non-trivial.
	if _, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace,
		"--outcome", "succeeded", "--evidence", "deadbeef"); err != nil {
		t.Fatal(err)
	}

	resetDecisionCLIFlags()
	out, err := runDecisionCLI(t, "stats", "-w", workspace)
	if err != nil {
		t.Fatalf("decision stats = %v", err)
	}
	for _, want := range []string{"Decisions formed:      2", "high-stakes", "standard", "1 resolved (1 verified)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDecisionStatsJSON(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "stats", "-w", workspace, "--json")
	if err != nil {
		t.Fatalf("decision stats --json = %v", err)
	}
	var payload struct {
		Metrics     team.DecisionMetrics   `json:"metrics"`
		Phase5Entry team.Phase5EntryStatus `json:"phase5_entry"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not a single JSON object: %v\n%s", err, out)
	}
	if payload.Metrics.DecisionCount != 2 {
		t.Fatalf("DecisionCount = %d, want 2", payload.Metrics.DecisionCount)
	}
	if payload.Phase5Entry.ResolvedRequired != team.Phase5ResolvedRequired {
		t.Fatalf("phase5 thresholds not reported: %#v", payload.Phase5Entry)
	}
}
