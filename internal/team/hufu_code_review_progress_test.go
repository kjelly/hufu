package team

import (
	"encoding/json/v2"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func loadReviewProgressTeam(t *testing.T) *TeamSession {
	t.Helper()
	session, err := LoadTeam(filepath.Join(bundledTeamsDir(t), "hufu-code-review"), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	session.Workspace = t.TempDir()
	return session
}

func TestReviewProgressContractsAndBudgets(t *testing.T) {
	session := loadReviewProgressTeam(t)
	c := &Coordinator{session: session, acceptanceSpec: session.Config.AcceptanceSpec}
	if err := ValidateAcceptanceSpec(c.acceptanceSpec, session.Config.GoalMode); err != nil {
		t.Fatal(err)
	}
	for _, criterion := range c.acceptanceSpec.Criteria {
		if !criterion.Required {
			t.Fatalf("milestone %q must remain required", criterion.ID)
		}
		if !slices.ContainsFunc(c.acceptanceSpec.Verifications, func(spec VerificationSpec) bool {
			return reflect.DeepEqual(spec, criterion.Verify)
		}) {
			t.Fatalf("milestone %q no longer uses a blocking acceptance check", criterion.ID)
		}
	}
	for _, contract := range session.ContractTasks {
		bound, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: contract.Agent, ContractID: contract.ID}})
		if err != nil {
			t.Fatal(err)
		}
		if bound[0].Kind != TaskKindOutcome || len(bound[0].Advances) == 0 {
			t.Fatalf("contract %q lost its static progress links", contract.ID)
		}
		if err := c.validateTaskCriterionLinks(bound); err != nil {
			t.Fatalf("contract %q: %v", contract.ID, err)
		}
		// An unfinished aggregate workset must not replay accepted children.
		// Worker-attempt recovery still uses the team/agent retry policy.
		if contract.FanOut != nil && contract.MaxRetries != 0 {
			t.Fatalf("aggregate milestone could replay completed children of %q", contract.ID)
		}
	}
	rc := c.reliabilityConfig()
	if !rc.HardEnforcement || rc.MaxTokensPerAttempt <= 0 {
		t.Fatal("review circuit breakers must stay enabled")
	}
	// The producer permits 32 reviews; every one can require a separate critic.
	// The longest interval is 32 guarded attempts plus coordinator overhead,
	// synthesis, and the final native audit after the inventory milestone.
	interval := NoProgressCounters{Tokens: 33*int64(rc.MaxTokensPerAttempt) + 1_000_000, Tasks: 34, Turns: 1}
	if got, reason := decideNoProgress(interval, c.noProgressLimits(), rc.HardEnforcement); got != NoProgressContinue {
		t.Fatalf("maximum declared review interval trips the budget: %s (%s)", got, reason)
	}
	if got, _ := decideNoProgress(NoProgressCounters{Tokens: int64(rc.MaxTokensWithoutProgress)}, c.noProgressLimits(), true); got != NoProgressReplan {
		t.Fatalf("token ceiling no longer requests replan: %s", got)
	}
	if got, _ := decideNoProgress(NoProgressCounters{Tasks: rc.MaxTasksWithoutProgress}, c.noProgressLimits(), true); got != NoProgressReplan {
		t.Fatalf("task ceiling no longer requests replan: %s", got)
	}
}

func TestReviewFirstAttestationAdvancesOnceAndRejectsUntrustedEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		passed     bool
		trusted    bool
		wantPasses int
	}{
		{name: "first_pass", passed: true, trusted: true, wantPasses: 2},
		{name: "failed_scope", trusted: true},
		{name: "stale_receipt", passed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			session := loadReviewProgressTeam(t)
			definitions := session.RunInputDefinitions
			snapshot := mustRunInputSnapshot(t, definitions, []RunInputAssignment{{
				Name: "review.scope", RawValue: []byte(`{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}`), Source: RunInputSourceCLI,
			}})
			outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{
				"scope": map[string]any{
					"requested":            map[string]any{"kind": "last_n", "count": 10, "history": "first_parent", "head": "HEAD"},
					"requested_input_hash": snapshot.Inputs[0].ValueHash,
					"satisfied":            test.passed, "observed_budget": map[string]any{"changed_paths": 1},
				},
				"documentation_verification": map[string]any{"passed": test.passed},
			})
			if err != nil {
				t.Fatal(err)
			}
			contract := session.ContractTasks[slices.IndexFunc(session.ContractTasks, func(task TaskDef) bool { return task.ID == "produce-workset" })]
			tracker := NewTaskTracker()
			item := tracker.TodoList().AddBatch([]TodoSpec{{
				PlanTaskID: contract.ID, ContractID: contract.ID, Agent: contract.Agent,
				Kind: contract.Kind, Advances: slices.Clone(contract.Advances),
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
				MaterializedActionPayloadHash: "sha256:payload", BoundInputs: map[string]string{"review.scope": snapshot.Inputs[0].ValueHash},
			}})[0]
			item.Status = TaskDone
			item.TypedResult = &TaskResult{
				TaskID: item.ID, Agent: contract.Agent, Attempt: 1, Status: TaskResultStatusSuccess, Summary: "prepared", Source: "runtime",
				RuntimeOutputs: outputs, RuntimeOutputsHash: digest,
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
				MaterializedActionPayloadHash: item.MaterializedActionPayloadHash, BoundInputs: item.BoundInputs,
			}
			receiptRun := "run-review-progress"
			if !test.trusted {
				receiptRun = "run-stale"
			}
			item.ExecutionReceipt = &ExecutionReceipt{
				RunID: receiptRun, TaskID: item.ID, Attempt: 1, StartedAt: time.Now(), FinishedAt: time.Now(), ExitCode: new(0),
				ProducerID: contract.Agent, ModelExecutionID: "runtime-1", TranscriptRef: "sha256:transcript", ActionInvocationID: "action-1",
				RuntimeOutputsHash: digest, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
				MaterializedActionPayloadHash: item.MaterializedActionPayloadHash, BoundInputs: item.BoundInputs,
			}
			tracker.TodoList().Restore([]*TodoItem{item})
			c := &Coordinator{
				session: session, taskTracker: tracker, executionRunID: "run-review-progress", acceptanceSpec: session.Config.AcceptanceSpec,
				sessionData: &SessionData{RunInputSnapshots: []RunInputSnapshot{*snapshot}, ActiveRunInputSnapshotID: snapshot.ID},
			}
			c.recordNoProgressTokens(2_071_682)
			c.recordNoProgressTasks(8)
			c.reEvaluateAffectedCriteria(t.Context(), item)
			if len(item.ProgressCriteria) != test.wantPasses {
				t.Fatalf("objective milestones advanced = %v, want %d", item.ProgressCriteria, test.wantPasses)
			}
			if test.wantPasses > 0 {
				if got := c.noProgressCounters(); got.Tokens != 0 || got.Tasks != 0 {
					t.Fatalf("first objective pass did not reset counters: %+v", got)
				}
			} else if got := c.noProgressCounters(); got.Tokens != 2_071_682 || got.Tasks != 8 {
				t.Fatalf("unproven result reset counters: %+v", got)
			}
			// Rechecking identical accepted evidence cannot extend the budget.
			c.recordNoProgressTokens(123)
			before := c.noProgressCounters()
			c.reEvaluateAffectedCriteria(t.Context(), item)
			if after := c.noProgressCounters(); after != before {
				t.Fatalf("repeated attestation reset counters: before=%+v after=%+v", before, after)
			}
			if acceptance, err := c.runAcceptance(t.Context()); err == nil || acceptance.Passed {
				t.Fatal("PREPARE milestones incorrectly accepted an unfinished review")
			}
		})
	}
}

func TestReviewWorksetProgressRequiresAllVerifiedChildren(t *testing.T) {
	session := loadReviewProgressTeam(t)
	workspace, c, producer, store := newWorksetArtifactFixture(t)
	session.Workspace = workspace
	c.session = session
	c.sessionData = NewSession()
	// Exercise the route's actual aggregate verifier independently of PREPARE.
	for _, criterion := range session.Config.AcceptanceSpec.Criteria {
		if criterion.ID == "primary-reviews-complete" {
			criterion.DependsOn = nil
			c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{criterion}}
		}
	}
	if c.acceptanceSpec == nil {
		t.Fatal("primary review milestone is missing")
	}
	manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion}
	for _, key := range []string{"one", "two"} {
		manifest.Items = append(manifest.Items, WorksetItem{
			Key: key, Bindings: map[string]string{"key": key, "review_revision": "frozen-head", "lens": "general"},
		})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(t.Context(), PutArtifactRequest{
		Content: data, Path: "manifest.json", Kind: "workset_manifest", RunID: c.executionRunID,
		TaskID: producer.ID, Attempt: producer.TypedResult.Attempt, Agent: producer.TypedResult.Agent,
	})
	if err != nil {
		t.Fatal(err)
	}
	producer.TypedResult.Artifacts = []ArtifactRef{artifact.ArtifactRef}
	contract := session.ContractTasks[slices.IndexFunc(session.ContractTasks, func(task TaskDef) bool { return task.ID == "review-primary-workset" })]
	contract.FanOut.SourceArtifact = FactRef{TaskID: producer.ID, Artifact: artifact.ID}
	children, err := c.expandFanOutTasks([]TaskDef{contract})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 {
		t.Fatalf("expanded %d children, want 2", len(children))
	}
	var specs []TodoSpec
	for _, child := range children {
		specs = append(specs, TodoSpec{
			Agent: child.Agent, PlanTaskID: contract.ID, Kind: child.Kind,
			Advances: slices.Clone(child.Advances), WorksetBinding: child.WorksetBinding,
		})
	}
	items := c.taskTracker.TodoList().AddBatch(specs)
	receipts, err := buildWorksetReceipts(children, []string{items[0].ID, items[1].ID}, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		item.WorksetReceipt = receipts[item.WorksetBinding.WorksetID]
		item.TypedResult = &TaskResult{TaskID: item.ID, Status: TaskResultStatusSuccess, Summary: "reviewed"}
		item.VerifyResult = &VerificationResult{ExitCode: 0}
	}
	items[0].Status = TaskDone
	c.recordNoProgressTokens(2_071_682)
	c.reEvaluateAffectedCriteria(t.Context(), items[0])
	if got := c.noProgressCounters().Tokens; got != 2_071_682 {
		t.Fatalf("partial workset reset tokens: %d", got)
	}
	items[1].Status = TaskDone
	items[1].VerifyResult.ExitCode = 1
	c.reEvaluateAffectedCriteria(t.Context(), items[1])
	if got := c.noProgressCounters().Tokens; got != 2_071_682 {
		t.Fatalf("unverified workset reset tokens: %d", got)
	}
	items[1].VerifyResult.ExitCode = 0
	c.reEvaluateAffectedCriteria(t.Context(), items[1])
	if got := c.noProgressCounters().Tokens; got != 0 || items[1].Progress != ProgressAdvanced {
		t.Fatalf("complete verified workset did not advance: tokens=%d progress=%s results=%+v", got, items[1].Progress, c.sessionData.CriterionResults)
	}
}
