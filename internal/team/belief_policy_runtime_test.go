package team

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func configureBeliefTestPolicy(t *testing.T, session *TeamSession) {
	t.Helper()
	policy, err := json.Marshal(beliefTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session.Dir, "policy.json"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	session.Config.Belief.PolicyFile = "policy.json"
	session.ActionCatalog = beliefTestCatalog(t, "executor")
}

func TestBeliefPolicyRuntimeAdvisoryAndRestart(t *testing.T) {
	c, marker := runBeliefAction(t, false, func(session *TeamSession) { configureBeliefTestPolicy(t, session) })
	assertBeliefActionState(t, c, marker)
	views := c.BeliefStates()
	if len(views[0].Suggestions) != 1 || views[0].Suggestions[0].Record.SelectedCandidateID != "check-b" || views[0].Suggestions[0].Record.DecisionGeneration != 1 || views[0].Suggestions[0].Validation != "verified" {
		t.Fatalf("runtime did not record an advisory selection: %+v", views)
	}
	want := views[0].Suggestions
	// Suggestions must not consume or reset existing execution budgets.
	c.budgetLedger.setLimits(60, 100)
	c.budgetLedger.addTokens(37)
	limits := c.budgetLedger.Limits()
	c.beliefRuntime = nil
	c.refreshBeliefs(nil)
	if c.budgetLedger.TokensUsed() != 37 || c.budgetLedger.Limits() != limits || c.budgetLedger.Reserved() != 0 {
		t.Fatal("advisory rebuild changed execution budget")
	}
	if !reflect.DeepEqual(want, c.BeliefStates()[0].Suggestions) {
		t.Fatal("cache reconstruction changed advisory generation")
	}
	c.saveCheckpoint()
	if err := c.eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	saved := LoadSession(c.session.Workspace)
	if saved == nil {
		t.Fatal("missing durable session")
	}
	restarted := &Coordinator{session: c.session, taskTracker: NewTaskTracker(), executionRunID: "run-after-restart", reportStatus: func(StatusEvent) {}}
	restarted.SetSessionData(saved)
	restarted.initEventStore()
	t.Cleanup(func() { _ = restarted.eventStore.Close() })
	if restarted.sessionData.RecoveryRequired {
		t.Fatal("advisory events caused execution projection drift")
	}
	restarted.refreshBeliefs(nil)
	if _, err := restarted.ResumeInterruptedTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range restarted.BeliefStates() {
		if view.RunID == "run-belief-action" {
			found = true
			if !reflect.DeepEqual(want, view.Suggestions) {
				t.Fatal("restart replaced the historical advice")
			}
		}
	}
	if !found {
		t.Fatal("restart lost the run's advisory history")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
		t.Fatal("advisory selection dispatched an extra action")
	}
	if len(restarted.taskTracker.TodoList().Items()) != 1 {
		t.Fatal("advisory selection scheduled another task")
	}
}

func TestBeliefPolicyRuntimeHonorsConditionalAdapterFailure(t *testing.T) {
	c, marker := runBeliefAction(t, false, func(session *TeamSession) { configureBeliefTestPolicy(t, session) })
	view := c.BeliefStates()[0]
	epoch := view.Epochs[0]
	if err := c.beliefRuntime.journal.Retract(t.Context(), epoch.Epoch.ID, epoch.Observations[0].ID, epoch.Sources[0].EventID); err != nil {
		t.Fatal(err)
	}
	adapter := &beliefPolicyFaultJournal{ConditionalEventJournal: c.EventJournal().(ConditionalEventJournal), after: true}
	c.SetEventJournal(adapter)
	c.beliefRuntime.needsRepair = true
	c.refreshBeliefs(nil)
	if adapter.after || !c.beliefRuntime.needsRepair {
		t.Fatal("runtime bypassed the injected conditional journal")
	}
	c.refreshBeliefs(nil)
	view = c.BeliefStates()[0]
	if len(view.Suggestions) != 2 || view.Suggestions[1].Record.DecisionGeneration != 2 || c.beliefRuntime.needsRepair {
		t.Fatalf("advisory repair duplicated or lost the generation: %+v", view)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
		t.Fatal("advisory repair repeated execution")
	}
}

func TestBeliefPolicyManifestUsesExistingCatalog(t *testing.T) {
	manifest := actionCatalogTestManifest("belief:\n  model-file: model.json\n  experiment: comparison\n  policy-file: policy.json\n", actionCatalogTestEntry)
	dir := writeActionCatalogTeam(t, manifest)
	writeBeliefModel(t, dir)
	policy := beliefTestPolicy()
	for i := range policy.Choices {
		policy.Choices[i].CandidateID = "collect-debug-bundle"
	}
	data, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	session, err := LoadTeam(dir, nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if session.Belief == nil || session.Belief.policy == nil || session.Belief.policy.CatalogDigest != session.ActionCatalog.Hash || len(session.Belief.policy.Candidates) != 1 {
		t.Fatal("manifest did not bind the validated catalog")
	}
	start := func() *Coordinator {
		t.Helper()
		copySession := *session
		copySession.Config.GoalMode = "exploratory"
		copySession.Workspace = session.Workspace
		c, err := newCoordinator(coordinatorParams{Session: &copySession, MaxConcurrent: 1}, RuntimeServices{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if saved := LoadSession(copySession.Workspace); saved != nil {
			c.SetSessionData(saved)
		}
		c.initEventStore()
		return c
	}
	// Use a dedicated writable workspace, then freeze the original policy.
	session.Workspace = t.TempDir()
	first := start()
	if err := first.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	policy.Version = "changed"
	data, err = json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compileTeamBelief(session); err != nil {
		t.Fatal(err)
	}
	changed := start()
	if err := changed.checkRunAdmission(); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("policy change did not block resume: %v", err)
	}
}
