package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/improve"
	"github.com/kjelly/hufu/internal/promotion"
	"github.com/spf13/cobra"
)

func TestRunImproveHandoffPrepareDispatchesEveryKind(t *testing.T) {
	t.Run("memory policy", testPrepareMemoryPolicyHandoff)
	t.Run("consolidation", testPrepareConsolidationHandoff)
	t.Run("skill", testPrepareSkillHandoff)
}

func TestConsolidationHandoffLifecycleRejectsWrongEvidenceAndRecommendsRollback(t *testing.T) {
	fixture := prepareConsolidationHandoffFixture(t)
	wrongRef := improve.ArtifactRef{Kind: "context_item", ID: "wrong-candidate", Revision: fixture.candidate.Revision}
	wrongReport := writeConsolidationExperiment(t, fixture, "wrong-experiment", wrongRef)
	setHandoffTestGlobals(fixture.workspace, fixture.scope)
	improveHandoffExperiment = wrongReport.ID
	if err := runImproveHandoffEvaluate(handoffTestCommand(t), []string{fixture.handoffID}); err == nil || !strings.Contains(err.Error(), "context candidate") {
		t.Fatalf("wrong candidate experiment error = %v", err)
	}
	assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffCandidateReady)

	report := writeConsolidationExperiment(t, fixture, "matching-experiment", fixture.candidate)
	improveHandoffExperiment = report.ID
	if err := runImproveHandoffEvaluate(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatal(err)
	}
	handoff := assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffEligibleForReview)
	improveHandoffExpected = handoff.Revision
	if err := runImproveHandoffApprove(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatal(err)
	}
	handoff = assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffApproved)
	if err := runImproveHandoffApprove(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatalf("idempotent approval retry: %v", err)
	}

	repo := openHandoffTestRepo(t, fixture.workspace)
	if err := repo.ConfirmCandidates(t.Context(), []string{fixture.candidate.ID}, contextstore.CandidateBinding{Evidence: contextstore.EvidenceRef{Type: "operator_approval", Ref: fixture.proposalID}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateConsolidationProposal(t.Context(), fixture.proposalID, "approved", "explicit operator approval"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	improveHandoffExpected = handoff.Revision
	improveHandoffAdoption = "context_consolidation_approval:" + fixture.proposalID + ":" + fixture.candidate.Revision
	if err := runImproveHandoffAdopt(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatal(err)
	}
	handoff = assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffAdopted)
	if err := runImproveHandoffAdopt(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatalf("idempotent adoption retry: %v", err)
	}

	monitoring, err := improve.EvaluateMonitoring(improve.Adoption{
		ID: fixture.proposalID, Team: fixture.scope.TeamID, CandidateRevision: fixture.candidate.Revision,
		BaselineSnapshotID: "baseline-context", RollbackRevision: "baseline-context-revision", BaselineMetrics: improve.Metrics{TotalTasks: 1, Done: 1},
	}, &improve.Report{Team: fixture.scope.TeamID, RunIDs: []string{"production-run"}, TeamRevisions: []string{fixture.candidate.Revision}, Metrics: improve.Metrics{TotalTasks: 1, Done: 1}}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	monitoring.ID = "degraded-monitoring"
	if _, err := improve.WriteMonitoringReport(fixture.workspace, monitoring); err != nil {
		t.Fatal(err)
	}
	improveHandoffExpected = handoff.Revision
	improveHandoffMonitoring = "monitoring_report:" + monitoring.ID + ":" + improve.MonitoringReportRevision(monitoring)
	if err := runImproveHandoffMonitor(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatal(err)
	}
	assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffRollbackRecommended)
	if err := runImproveHandoffMonitor(handoffTestCommand(t), []string{fixture.handoffID}); err != nil {
		t.Fatalf("idempotent monitoring retry: %v", err)
	}
}

func TestHandoffCommandsEnforceScopeAndMarkChangedEvidenceStale(t *testing.T) {
	fixture := prepareConsolidationHandoffFixture(t)
	setHandoffTestGlobals(fixture.workspace, fixture.scope)
	improveTeam = "other-team"
	if err := runImproveHandoffShow(handoffTestCommand(t), []string{fixture.handoffID}); err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("scope mismatch error = %v", err)
	}
	setHandoffTestGlobals(fixture.workspace, fixture.scope)
	repo := openHandoffTestRepo(t, fixture.workspace)
	if err := repo.UpdateLifecycle(t.Context(), []string{"source-a"}, contextstore.LifecycleRejected); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	improveHandoffExperiment = "missing-experiment"
	if err := runImproveHandoffEvaluate(handoffTestCommand(t), []string{fixture.handoffID}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("changed evidence error = %v", err)
	}
	assertHandoffStatus(t, fixture.store, fixture.handoffID, improve.HandoffStale)
}

type consolidationHandoffFixture struct {
	workspace  string
	scope      improve.HandoffScope
	store      *improve.HandoffStore
	handoffID  string
	proposalID string
	candidate  improve.ArtifactRef
}

func prepareConsolidationHandoffFixture(t *testing.T) consolidationHandoffFixture {
	t.Helper()
	workspace := t.TempDir()
	scope := improve.HandoffScope{ProjectID: "project", TeamID: "team", PolicyVersion: "memory-policy-v1"}
	repo := openHandoffTestRepo(t, workspace)
	sources := appendConfirmedHandoffSources(t, repo, scope, 2)
	item, err := repo.UpsertCandidate(t.Context(), contextstore.ContextItem{
		ID: "context-candidate", Kind: contextstore.ContextPattern, Content: "consolidated guidance",
		Scope: contextstore.Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID}, Lifecycle: contextstore.LifecycleCandidate,
		Source: contextstore.SourceRef{Type: "consolidation_proposal", Ref: "consolidation-proposal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := contextstore.ConsolidationProposal{
		ID: "consolidation-proposal", ProjectID: scope.ProjectID, TeamID: scope.TeamID, CandidateContextItemID: item.ID,
		SourceIDs: []string{sources[0].ID, sources[1].ID}, SourceRevisions: map[string]string{sources[0].ID: sources[0].ContentHash, sources[1].ID: sources[1].ContentHash},
		AggregateRevisions: map[string]int64{sources[0].ID: 1, sources[1].ID: 1}, Status: "proposed",
	}
	if err := repo.SaveConsolidationProposal(t.Context(), proposal); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	handoff, err := createConsolidationHandoff(t.Context(), workspace, scope, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := improve.NewHandoffStore(workspace)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	setHandoffTestGlobals(workspace, scope)
	if err := runImproveHandoffPrepare(handoffTestCommand(t), []string{handoff.ID}); err != nil {
		t.Fatal(err)
	}
	prepared := assertHandoffStatus(t, store, handoff.ID, improve.HandoffCandidateReady)
	return consolidationHandoffFixture{workspace: workspace, scope: scope, store: store, handoffID: handoff.ID, proposalID: proposal.ID, candidate: *prepared.Candidate}
}

func writeConsolidationExperiment(t *testing.T, handoff consolidationHandoffFixture, id string, candidateRef improve.ArtifactRef) improve.ExperimentReport {
	t.Helper()
	benchmark := improve.BenchmarkFixture{Name: "context-benchmark-" + id, Team: handoff.scope.TeamID, Category: "context", Cases: []improve.BenchmarkCase{{ID: "case", Type: "happy", Prompt: "Use context."}}}
	if _, _, err := improve.CreateBenchmark(handoff.workspace, benchmark); err != nil {
		t.Fatal(err)
	}
	baseline := improve.TeamSnapshot{Version: 1, ID: "baseline-" + id, Kind: "baseline", Team: handoff.scope.TeamID, DefinitionRevision: "baseline-revision", ContentRevision: "baseline-content"}
	candidate := improve.TeamSnapshot{Version: 1, ID: "candidate-" + id, Kind: "candidate", Team: handoff.scope.TeamID, DefinitionRevision: "candidate-revision", ContentRevision: "candidate-content", BaselineID: baseline.ID}
	report, err := improve.EvaluateExperiment(id, benchmark,
		improve.ExperimentInput{Snapshot: baseline, Report: &improve.Report{Team: handoff.scope.TeamID, RunIDs: []string{"baseline-run"}, TeamRevisions: []string{baseline.DefinitionRevision}, Metrics: improve.Metrics{TotalTasks: 1, Done: 1}}, AcceptancePassed: true},
		improve.ExperimentInput{Snapshot: candidate, Report: &improve.Report{Team: handoff.scope.TeamID, RunIDs: []string{"candidate-run"}, TeamRevisions: []string{candidate.DefinitionRevision}, AppliedContextRefs: []improve.ArtifactRef{candidateRef}, Metrics: improve.Metrics{TotalTasks: 1, Done: 1}}, ContextCandidate: &candidateRef, AcceptancePassed: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := improve.WriteExperimentReport(handoff.workspace, report); err != nil {
		t.Fatal(err)
	}
	return report
}

func assertHandoffStatus(t *testing.T, store *improve.HandoffStore, id string, status improve.HandoffStatus) improve.ImprovementHandoff {
	t.Helper()
	handoff, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Status != status {
		t.Fatalf("handoff status = %s, want %s", handoff.Status, status)
	}
	return handoff
}

func testPrepareMemoryPolicyHandoff(t *testing.T) {
	workspace := t.TempDir()
	base := improve.DefaultMemoryPolicySnapshot("base-policy")
	if _, err := improve.WriteMemoryPolicySnapshot(workspace, base); err != nil {
		t.Fatal(err)
	}
	optimizer, err := improve.ProposeMemoryPolicyOptimization("candidate-policy", base, improve.Metrics{MemoryHarmfulUseRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := improve.NewMemoryPolicyOptimizationProposal(workspace, "policy-proposal", optimizer, improve.Metrics{MemoryHarmfulUseRate: 1})
	if err != nil {
		t.Fatal(err)
	}
	scope := improve.HandoffScope{ProjectID: "project", TeamID: "team"}
	handoff, err := createMemoryPolicyHandoff(t.Context(), workspace, scope, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := improve.NewHandoffStore(workspace)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	setHandoffTestGlobals(workspace, scope)
	improveHandoffBaselinePolicy = base.ID
	if err := runImproveHandoffPrepare(handoffTestCommand(t), []string{handoff.ID}); err != nil {
		t.Fatal(err)
	}
	assertHandoffCandidateReady(t, store, handoff.ID, "memory_policy_snapshot")
}

func testPrepareConsolidationHandoff(t *testing.T) {
	workspace := t.TempDir()
	scope := improve.HandoffScope{ProjectID: "project", TeamID: "team", PolicyVersion: "memory-policy-v1"}
	repo := openHandoffTestRepo(t, workspace)
	sources := appendConfirmedHandoffSources(t, repo, scope, 2)
	candidate, err := repo.UpsertCandidate(t.Context(), contextstore.ContextItem{
		ID: "context-candidate", Kind: contextstore.ContextPattern, Content: "consolidated guidance",
		Scope: contextstore.Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID}, Lifecycle: contextstore.LifecycleCandidate,
		Source: contextstore.SourceRef{Type: "consolidation_proposal", Ref: "consolidation-proposal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := contextstore.ConsolidationProposal{
		ID: "consolidation-proposal", ProjectID: scope.ProjectID, TeamID: scope.TeamID, CandidateContextItemID: candidate.ID,
		SourceIDs:          []string{sources[0].ID, sources[1].ID},
		SourceRevisions:    map[string]string{sources[0].ID: sources[0].ContentHash, sources[1].ID: sources[1].ContentHash},
		AggregateRevisions: map[string]int64{sources[0].ID: 1, sources[1].ID: 1}, Status: "proposed",
	}
	if err := repo.SaveConsolidationProposal(t.Context(), proposal); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	handoff, err := createConsolidationHandoff(t.Context(), workspace, scope, proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := improve.NewHandoffStore(workspace)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	setHandoffTestGlobals(workspace, scope)
	if err := runImproveHandoffPrepare(handoffTestCommand(t), []string{handoff.ID}); err != nil {
		t.Fatal(err)
	}
	assertHandoffCandidateReady(t, store, handoff.ID, "context_item")
}

func testPrepareSkillHandoff(t *testing.T) {
	workspace := t.TempDir()
	scope := improve.HandoffScope{ProjectID: "project", TeamID: "team", PolicyVersion: "memory-policy-v1"}
	repo := openHandoffTestRepo(t, workspace)
	source := appendConfirmedHandoffSources(t, repo, scope, 1)[0]
	draft := "---\nname: prepared-skill\ndescription: Prepared only in an isolated candidate.\n---\n1. Verify the input.\n2. Record objective evidence.\n"
	proposal := contextstore.PromotionProposal{
		ProjectID: scope.ProjectID, TeamID: scope.TeamID, Type: contextstore.PromotionTypeSkill,
		TargetPath: promotion.TargetPathForSkill("prepared-skill"), Draft: draft, PolicyVersion: scope.PolicyVersion,
		Sources: []contextstore.PromotionSourceSnapshot{{ContextItemID: source.ID, ContentHash: source.ContentHash, AggregateRevision: 1}},
	}
	stored, _, err := repo.CreatePromotion(t.Context(), proposal, contextstore.PromotionOutboxEvent{IdempotencyKey: "promotion-created", EventType: "memory_promotion_proposed", Payload: json.RawMessage(`{"schema_version":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	teamDir := filepath.Join(t.TempDir(), "team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: team\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "helper.md"), []byte("---\nname: helper\nrole: worker\n---\nHelp.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseline, _, err := improve.CreateBaselineSnapshot(workspace, "baseline-team", teamDir)
	if err != nil {
		t.Fatal(err)
	}
	handoff, err := createSkillHandoff(t.Context(), workspace, scope, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := improve.NewHandoffStore(workspace)
	if _, err := store.Create(t.Context(), handoff); err != nil {
		t.Fatal(err)
	}
	setHandoffTestGlobals(workspace, scope)
	improveHandoffBaselineTeam = baseline.ID
	if err := runImproveHandoffPrepare(handoffTestCommand(t), []string{handoff.ID}); err != nil {
		t.Fatal(err)
	}
	assertHandoffCandidateReady(t, store, handoff.ID, "team_snapshot")
}

func openHandoffTestRepo(t *testing.T, workspace string) *contextstore.SQLiteRepository {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func appendConfirmedHandoffSources(t *testing.T, repo *contextstore.SQLiteRepository, scope improve.HandoffScope, count int) []contextstore.ContextItem {
	t.Helper()
	items := make([]contextstore.ContextItem, count)
	for i := range count {
		items[i] = contextstore.ContextItem{
			ID: "source-" + string(rune('a'+i)), Kind: contextstore.ContextPattern, Content: "source guidance " + string(rune('a'+i)),
			Scope: contextstore.Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID}, Lifecycle: contextstore.LifecycleConfirmed,
			Metadata: map[string]string{"memory_lifetime": "persistent"},
		}
	}
	if err := repo.Append(t.Context(), items...); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(items))
	for i := range items {
		ids[i] = items[i].ID
		if _, err := repo.ApplyExperienceObservation(t.Context(), contextstore.ExperienceObservation{IdempotencyKey: "observation-" + items[i].ID, ContextItemID: items[i].ID, PolicyVersion: scope.PolicyVersion, ProjectID: scope.ProjectID, TaskID: "task-" + items[i].ID, ExposureDelta: 1}); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := repo.GetMany(t.Context(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func setHandoffTestGlobals(workspace string, scope improve.HandoffScope) {
	improveWorkspace = workspace
	improveTeam = scope.TeamID
	improveHandoffProject = scope.ProjectID
	improveHandoffPolicy = scope.PolicyVersion
	improveHandoffBaselineTeam = ""
	improveHandoffBaselinePolicy = ""
	improveHandoffExperiment = ""
	improveHandoffExpected = 0
	improveHandoffAdoption = ""
	improveHandoffMonitoring = ""
}

func assertHandoffCandidateReady(t *testing.T, store *improve.HandoffStore, id, kind string) {
	t.Helper()
	handoff, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Status != improve.HandoffCandidateReady || handoff.Candidate == nil || handoff.Candidate.Kind != kind {
		t.Fatalf("prepared handoff = %#v", handoff)
	}
}

func handoffTestCommand(t *testing.T) *cobra.Command {
	t.Helper()
	command := &cobra.Command{}
	command.SetContext(t.Context())
	return command
}
