package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

// End-to-end V1 decision proof (plan Stage 7.2).
//
// The chain under test is: load team → resolve profile → preflight gates →
// evidence sealing → independent judgments → aggregate → challenge/revision →
// finalization → index → durable record. It runs against the fixture team on
// disk and the deterministic judge, so a break anywhere in the chain fails
// here rather than only in a unit test of one stage.

type decisionE2E struct {
	coordinator *Coordinator
	judge       *fakeJudge
	session     *TeamSession
	journal     *memoryJournal
	// evidence holds the published artifact references, including the digests
	// the store assigned. A task must cite the digest that exists, not one it
	// invented, which is the property the sealed packet depends on.
	evidence map[string]ArtifactRef
}

// newDecisionE2E wires a coordinator over the fixture team and the fake judge.
func newDecisionE2E(t *testing.T) *decisionE2E {
	t.Helper()
	const judgeModel = "decision-v1-judge"
	GlobalModelSpecRegistry().RegisterSpec(ModelContextSpec{
		ModelID: judgeModel, ContextWindow: 32768, MaxOutputTokens: 2048, SafetyMarginTokens: 128,
	})

	judge := newFakeJudge("execute")
	server := newIPv4TestServer(t, judge)
	t.Cleanup(server.Close)

	dir := writeDecisionFixture(t, judgeModel)
	session, err := LoadTeam(dir, nil, nil, NewProviderRegistry())
	if err != nil {
		t.Fatalf("LoadTeam: %v", err)
	}
	manager, err := agent.NewProviderManager(server.URL+"/v1", "judge-secret", map[string]config.ProviderConfig{
		"ollama": {ProviderURL: server.URL + "/v1"},
	})
	if err != nil {
		t.Fatalf("NewProviderManager: %v", err)
	}

	// The fixture's evidence must exist in the artifact store: decision
	// evidence is resolved and integrity-checked, so a declared reference that
	// nobody published is correctly rejected.
	session.Workspace = dir
	session.Config.WorkspaceDir = dir
	evidence := publishFixtureEvidence(t, dir)

	// Provider invocations commit a durable profile projection, so an
	// end-to-end run needs the same event store a real run has.
	store, err := NewEventStore(dir, "run-decision-v1", "session-decision-v1")
	if err != nil {
		t.Fatalf("NewEventStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	journal := &memoryJournal{}
	c := &Coordinator{
		session:                 session,
		projectDir:              dir,
		sessionTime:             time.Now(),
		providerManager:         manager,
		modelProfileRuntime:     NewModelProfileRuntime(manager, false),
		taskTracker:             NewTaskTracker(),
		eventJournal:            journal,
		eventStore:              store,
		executionRunID:          "run-decision-v1",
		judgeModel:              judgeModel,
		sidecarModel:            judgeModel,
		reportStatus:            func(StatusEvent) {},
		providerBoundaryStarted: true,
	}
	c.SetSessionData(NewSession())
	return &decisionE2E{coordinator: c, judge: judge, session: session, journal: journal, evidence: evidence}
}

// publishFixtureEvidence writes the artifacts the fixture task references.
// Their bytes are fixed, so the sealed evidence hash is reproducible.
func publishFixtureEvidence(t *testing.T, workspace string) map[string]ArtifactRef {
	t.Helper()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("NewFileArtifactStore: %v", err)
	}
	published := map[string]ArtifactRef{}
	for _, artifact := range []struct {
		id, path string
		content  string
	}{
		{"evidence-1", "evidence/target.json", `{"bridge":"br0","accepts_change":true}`},
		{"base-rate-1", "evidence/base-rate.json", `{"reference_class":"bridge changes","success":0.62}`},
	} {
		result, err := store.Put(context.Background(), PutArtifactRequest{
			ID: artifact.id, Path: artifact.path, MediaType: "application/json",
			Kind: "evidence", Content: []byte(artifact.content),
			RunID: "run-decision-v1", TaskID: "bridge-change", Agent: "deployer", Attempt: 1,
		})
		if err != nil {
			t.Fatalf("publish fixture artifact %q: %v", artifact.id, err)
		}
		published[artifact.id] = result.ArtifactRef
	}
	return published
}

// decisionE2ETask is the fixture's decision task. Its options, assumptions and
// evidence are declared in configuration, never in model-supplied payload.
func (e *decisionE2E) task(profile string) TaskDef {
	task := decisionE2ETask(profile)
	task.DecisionArtifacts = []ArtifactRef{e.evidence["evidence-1"]}
	task.DecisionAssumptions[0].EvidenceRefs = []ArtifactRef{e.evidence["evidence-1"]}
	task.DecisionBaseRates[0].Source = e.evidence["base-rate-1"]
	return task
}

func decisionE2ETask(profile string) TaskDef {
	return TaskDef{
		ID:              "bridge-change",
		Agent:           "deployer",
		Goal:            "change br0 without dropping the tunnel",
		Constraints:     "no interruption longer than five seconds",
		DecisionProfile: profile,
		SideEffect:      SideEffectInfraMutation,
		Recovery:        RecoveryReconcile,
		ReconcileTool:   "probe-bridge",
		DecisionOptions: []DecisionOption{
			{ID: "execute", Kind: OptionExecute, Title: "Change the bridge in place"},
			{ID: "reduce", Kind: OptionReduceScope, Title: "Change one port first"},
			{ID: "defer", Kind: OptionDefer, Title: "Do nothing for now"},
			{ID: "gather", Kind: OptionRequestInfo, Title: "Measure the tunnel first"},
		},
		DecisionAssumptions: []DecisionAssumption{{
			ID: "service-accepts", Statement: "the target service accepts the change", Critical: true,
			EvidenceRefs: []ArtifactRef{{ID: "evidence-1", Path: "evidence/target.json"}},
		}},
		DecisionFacts:     map[string]any{"bridge": map[string]any{"name": "br0"}},
		DecisionArtifacts: []ArtifactRef{{ID: "evidence-1", Path: "evidence/target.json"}},
		DecisionBaseRates: []BaseRateEvidence{{
			ReferenceClass: "bridge changes", Metric: "success", SampleSize: 12,
			Source:      ArtifactRef{ID: "base-rate-1", Path: "evidence/base-rate.json", SHA256: strings.Repeat("b", 64)},
			Limitations: []string{"one operator"},
		}},
	}
}

// createDecisionTask puts the task through the durable admission boundary the
// runtime uses, so the occurrence a decision is formed against is the one an
// operator's run would produce.
func (e *decisionE2E) createDecisionTask(t *testing.T, task TaskDef) *TodoItem {
	t.Helper()
	spec := todoSpecForTestTask(task)
	ids := e.coordinator.taskTracker.TodoList().ReserveIDs(1)
	projection, err := taskOccurrenceProjectionFromSpec(spec, ids[0])
	if err != nil {
		t.Fatalf("taskOccurrenceProjectionFromSpec: %v", err)
	}
	if _, err := e.coordinator.admitTaskOccurrence(context.Background(), projection, ids[0], 1); err != nil {
		t.Fatalf("admitTaskOccurrence: %v", err)
	}
	items, err := e.coordinator.CommitTaskCreationResolved(context.Background(), []TodoSpec{spec}, ids)
	if err != nil {
		t.Fatalf("create decision task: %v", err)
	}
	return items[0]
}

// formDecision runs the production dispatch entry point and returns the index
// entry the decision became addressable as. Going through prepareTaskDecision
// rather than the engine directly is the point: it is the path a real run
// takes, including admission, arming, and indexing.
func (e *decisionE2E) formDecision(t *testing.T, profile string) DecisionIndexEntry {
	t.Helper()
	task := e.task(profile)
	item := e.createDecisionTask(t, task)
	disarm, err := e.coordinator.prepareTaskDecision(context.Background(), task, item.ID)
	if err != nil {
		t.Fatalf("form decision under %q: %v", profile, err)
	}
	t.Cleanup(disarm)

	discipline := e.coordinator.disciplineFor(item.ID)
	if discipline == nil {
		t.Fatalf("profile %q armed no execution discipline", profile)
	}
	if discipline.decisionID == "" {
		t.Fatalf("profile %q armed a discipline with no decision", profile)
	}
	index, err := e.coordinator.decisionIndex()
	if err != nil {
		t.Fatalf("decision index: %v", err)
	}
	entry, found, err := index.Get(discipline.decisionID)
	if err != nil || !found {
		t.Fatalf("decision %s is not addressable in the index: found=%t err=%v", discipline.decisionID, found, err)
	}
	return entry
}

// tryFormDecision is formDecision for the blocked cases: it returns the error
// instead of failing.
func (e *decisionE2E) tryFormDecision(t *testing.T, task TaskDef) error {
	t.Helper()
	item := e.createDecisionTask(t, task)
	disarm, err := e.coordinator.prepareTaskDecision(context.Background(), task, item.ID)
	if err == nil {
		t.Cleanup(disarm)
	}
	return err
}

// The standard profile forms a complete decision through every stage the
// profile enables, and the record it produces is durable and addressable.
func TestDecisionV1StandardProfileFormsCompleteRecord(t *testing.T) {
	e := newDecisionE2E(t)
	entry := e.formDecision(t, fixtureProfileStandard)

	if entry.DecisionID == "" {
		t.Fatalf("index entry = %#v", entry)
	}
	if entry.Profile != fixtureProfileStandard {
		t.Fatalf("entry profile = %q", entry.Profile)
	}
	if entry.EvidenceHash == "" {
		t.Fatal("entry carries no sealed evidence hash")
	}
	if entry.FinalOption != "execute" {
		t.Fatalf("selected option = %q, want the option every judge preferred", entry.FinalOption)
	}
	// The profile declares three independent judgments; each one is a real
	// judge dispatch.
	if got := e.judge.count(stageJudge); got != 3 {
		t.Fatalf("judge dispatches = %d, want 3 independent judgments", got)
	}
	// The task declared its options, so the proposer must not have run.
	if got := e.judge.count(stageOptions); got != 0 {
		t.Fatalf("option proposals = %d, want none for a task that declared options", got)
	}
	// The decision is durable: its finalization is in the journal, not only in
	// the in-memory engine.
	if e.journal.count(string(agent.EventDecisionFinalized)) == 0 {
		t.Fatalf("no finalization event was written: %v", e.journal.typesOf())
	}
}

// Isolation is a property of the prompts, not a claim: no judge may see
// another judge's opinion (spec §46 row B).
func TestDecisionV1JudgePromptsAreIsolated(t *testing.T) {
	e := newDecisionE2E(t)
	e.formDecision(t, fixtureProfileStandard)

	prompts := e.judge.promptsFor(stageJudge)
	if len(prompts) != 3 {
		t.Fatalf("judge prompts = %d, want 3", len(prompts))
	}
	for i, prompt := range prompts {
		// The judge's own output contract names its response fields, and the
		// sealed packet legitimately renders base-rate distributions. What
		// must never appear is another judge's answer, the runtime's
		// aggregate of round 1, or a challenge derived from them.
		for _, leaked := range []string{
			"Aggregate (computed by the runtime",
			"Aggregate of round 1",
			"strongest_countercase",
			"Leading option:",
			"revising your own judgment",
		} {
			if strings.Contains(prompt, leaked) {
				t.Fatalf("judge prompt %d leaked %q", i, leaked)
			}
		}
		if !strings.Contains(prompt, "No other judge's opinion") {
			t.Fatalf("judge prompt %d lost its isolation statement", i)
		}
	}
}

// The high-stakes profile blocks before any judge is dispatched when a
// required pre-judge gate is unsatisfied (§46 rows F, G, H).
func TestDecisionV1HighStakesGatesBlockBeforeAnyJudge(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*TaskDef)
		wantErr string
	}{
		{
			// Three options remain, so the minimum-options rule is satisfied
			// and the no-go requirement is the gate that fires.
			name: "no-go alternative missing",
			mutate: func(task *TaskDef) {
				kept := task.DecisionOptions[:0]
				for _, option := range task.DecisionOptions {
					if !option.Kind.IsNoGo() {
						kept = append(kept, option)
					}
				}
				task.DecisionOptions = kept
			},
			wantErr: ReasonDecisionNoNoGoOption,
		},
		{
			name:    "outside view missing",
			mutate:  func(task *TaskDef) { task.DecisionBaseRates = nil },
			wantErr: ReasonDecisionOutsideViewMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newDecisionE2E(t)
			task := e.task(fixtureProfileHighStakes)
			tc.mutate(&task)
			err := e.tryFormDecision(t, task)
			if err == nil {
				t.Fatal("a missing required gate formed a decision anyway")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want reason %s", err, tc.wantErr)
			}
			if got := e.judge.count(stageJudge); got != 0 {
				t.Fatalf("judge dispatches before a blocked gate = %d, want 0", got)
			}
		})
	}
}

// Two runs of the same fixture produce the same sealed evidence hash and the
// same selected option: aggregation is deterministic and order-independent
// (§46 rows C, E).
func TestDecisionV1FormationIsDeterministic(t *testing.T) {
	first := newDecisionE2E(t)
	firstEntry := first.formDecision(t, fixtureProfileStandard)

	second := newDecisionE2E(t)
	secondEntry := second.formDecision(t, fixtureProfileStandard)

	if firstEntry.EvidenceHash != secondEntry.EvidenceHash {
		t.Fatalf("evidence hash differed between identical runs: %q vs %q",
			firstEntry.EvidenceHash, secondEntry.EvidenceHash)
	}
	if firstEntry.FinalOption != secondEntry.FinalOption {
		t.Fatalf("selected option differed: %q vs %q", firstEntry.FinalOption, secondEntry.FinalOption)
	}
	if firstEntry.Probability != secondEntry.Probability {
		t.Fatalf("probability differed: %v vs %v", firstEntry.Probability, secondEntry.Probability)
	}
}

// A task under the reserved off profile forms no decision and dispatches no
// judge: the compatibility guarantee for every team that has not adopted
// decision profiles (§46 row A).
func TestDecisionV1OffProfileIsInert(t *testing.T) {
	e := newDecisionE2E(t)
	task := e.task(DecisionProfileOff)
	item := e.createDecisionTask(t, task)
	disarm, err := e.coordinator.prepareTaskDecision(context.Background(), task, item.ID)
	if err != nil {
		t.Fatalf("off-profile task failed: %v", err)
	}
	disarm()
	if got := e.judge.count(stageJudge); got != 0 {
		t.Fatalf("off-profile task dispatched %d judges, want 0", got)
	}
	if e.coordinator.disciplineFor(item.ID) != nil {
		t.Fatal("off-profile task armed an execution discipline")
	}
	for _, event := range []string{string(agent.EventDecisionStarted), string(agent.EventDecisionFinalized)} {
		if e.journal.count(event) != 0 {
			t.Fatalf("off-profile task wrote %s events", event)
		}
	}
}
