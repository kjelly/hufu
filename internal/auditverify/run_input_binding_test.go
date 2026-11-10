package auditverify

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestVerifyRunInputBindingDimensionReplaysAssertionsAndDetectsTampering(t *testing.T) {
	definitions := []team.RunInputDefinition{{Name: "review.scope", Schema: team.RunInputSchema{Type: "object"}, Required: true}}
	snapshot, err := team.ResolveRunInputSnapshot(definitions, []team.RunInputAssignment{{
		Name: "review.scope", RawValue: []byte(`{"count":1,"kind":"last_n"}`), Source: team.RunInputSourceCLI,
	}}, "run-input-audit", "invocation-1", "review")
	if err != nil {
		t.Fatal(err)
	}
	outputs, outputHash, err := team.CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{
		"requested":            map[string]any{"count": 1, "kind": "last_n"},
		"requested_input_hash": snapshot.Inputs[0].ValueHash,
		"satisfied":            true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bound := map[string]string{"review.scope": snapshot.Inputs[0].ValueHash}
	exitCode := 0
	item := &team.TodoItem{
		ID: "producer-1", PlanTaskID: "produce-workset", Status: team.TaskDone, OccurrenceRevision: 1,
		RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
		MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound,
		TypedResult: &team.TaskResult{
			TaskID: "producer-1", Status: team.TaskResultStatusSuccess, Summary: "produced", Source: "runtime",
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound,
			RuntimeOutputs: outputs, RuntimeOutputsHash: outputHash,
		},
		ExecutionReceipts: []team.ExecutionReceipt{{
			RunID: "run-input-audit", TaskID: "producer-1", Attempt: 1, ExitCode: &exitCode,
			StartedAt: time.Now().Add(-time.Second), FinishedAt: time.Now(), ActionInvocationID: "action-1", TranscriptRef: "artifact-transcript",
			RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
			MaterializedActionPayloadHash: "sha256:materialized", BoundInputs: bound, RuntimeOutputsHash: outputHash,
		}},
	}
	spec := team.VerificationSpec{
		Type: team.VerifyTaskOutputAssert, WorksetSourceTask: "produce-workset", TaskOutputName: "scope",
		Assertions: []team.TaskOutputAssertion{
			{Pointer: "/requested", Op: "equals_input", Input: "review.scope"},
			{Pointer: "/requested_input_hash", Op: "equals_input_hash", Input: "review.scope"},
			{Pointer: "/satisfied", Op: "equals", Value: true},
		},
	}
	evidence, err := team.ReplayTaskOutputAssertions([]*team.TodoItem{item}, "run-input-audit", snapshot, spec)
	if err != nil {
		t.Fatal(err)
	}
	verification := &team.VerificationResult{ExitCode: 0, Spec: &spec, TaskOutputAssertions: evidence}
	acceptance := &team.AcceptanceResult{State: team.AcceptancePassed, VerificationEvidence: []*team.VerificationResult{verification}}
	result := &team.RunResult{
		RunID: "run-input-audit", RunInputs: snapshot,
		Acceptance: acceptance, InputBoundAssertions: team.SummarizeInputBoundAssertions(acceptance),
	}
	audit := &AuditVerificationResult{RunID: result.RunID}
	if dimension := verifyRunInputBindingDimension(result.RunID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit); dimension.Status != AuditDimensionPass {
		t.Fatalf("dimension = %#v findings=%#v", dimension, audit.Findings)
	}

	item.TypedResult.RuntimeOutputs["scope"].(map[string]any)["satisfied"] = false
	audit = &AuditVerificationResult{RunID: result.RunID}
	dimension := verifyRunInputBindingDimension(result.RunID, result, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit)
	if dimension.Status != AuditDimensionFail || len(audit.Findings) != 1 || audit.Findings[0].Code != CodeRunInputBindingInvalid {
		encoded, _ := json.Marshal(audit)
		t.Fatalf("tampered audit dimension = %#v audit=%s", dimension, encoded)
	}
}

func TestReplayInputBoundAssertionsKeepsVerifiedRecoveryOrigin(t *testing.T) {
	for _, mode := range []string{"accepted", "missing_manifest", "missing_binding", "foreign_binding", "foreign_manifest", "changed_snapshot", "changed_output", "changed_receipt"} {
		t.Run(mode, func(t *testing.T) {
			snapshot, err := team.ResolveRunInputSnapshot(nil, nil, "source-run", "source-invocation", "generic")
			if err != nil {
				t.Fatal(err)
			}
			outputs, digest, err := team.CanonicalizeRuntimeOutputs(map[string]any{"scope": map[string]any{"satisfied": true}})
			if err != nil {
				t.Fatal(err)
			}
			ref := team.ArtifactRef{ID: "sha256-" + strings.Repeat("a", 64), RunID: "source-run", TaskID: "1", Attempt: 1, Agent: "worker"}
			receipt := &team.ExecutionReceipt{RunID: "source-run", TaskID: "1", Attempt: 1, ProducerID: "worker", ModelExecutionID: "native-source", ExitCode: new(0), TranscriptRef: ref.ID,
				RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ActionInvocationID: "native-source", RuntimeOutputsHash: digest}
			item := &team.TodoItem{ID: "1", PlanTaskID: "native-source", Status: team.TaskDone, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ExecutionReceipt: receipt,
				TypedResult: &team.TaskResult{TaskID: "1", Status: team.TaskResultStatusSuccess, Source: "runtime", RuntimeOutputs: outputs, RuntimeOutputsHash: digest, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}}
			binding := &team.EvidenceBinding{RunID: receipt.RunID, TaskID: item.ID, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID, ProducerID: receipt.ProducerID,
				TranscriptRef: ref.ID, ArtifactIDs: []string{ref.ID}, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash, ActionInvocationID: receipt.ActionInvocationID, RuntimeOutputsHash: digest}
			manifest := &team.EvidenceManifest{RunID: "recovery-run", RecoverySnapshotID: snapshot.ID, RecoverySnapshotHash: snapshot.SnapshotHash, ArtifactRefs: []team.ArtifactRef{ref},
				EvidenceResults: []team.EvidenceResult{{RequirementID: "task:1", Status: "passed", Binding: binding, ArtifactRefs: []team.ArtifactRef{ref}}}}
			if err := manifest.Seal(); err != nil {
				t.Fatal(err)
			}
			if _, ok := manifest.VerifiedTaskBinding("1"); !ok {
				t.Fatal("fixture has no verified source binding")
			}
			spec := team.VerificationSpec{Type: team.VerifyTaskOutputAssert, WorksetSourceTask: "native-source", TaskOutputName: "scope", Assertions: []team.TaskOutputAssertion{{Pointer: "/satisfied", Op: "equals", Value: true}}}
			evidence, err := team.ReplayTaskOutputAssertions([]*team.TodoItem{item}, "source-run", snapshot, spec)
			if err != nil {
				t.Fatal(err)
			}
			acceptance := &team.AcceptanceResult{State: team.AcceptancePassed, VerificationEvidence: []*team.VerificationResult{{ExitCode: 0, Spec: &spec, TaskOutputAssertions: evidence}}}
			run := &team.RunResult{RunID: "recovery-run", RunInputs: snapshot, RunInputOriginRunID: "source-run", EvidenceManifest: manifest, Acceptance: acceptance, InputBoundAssertions: team.SummarizeInputBoundAssertions(acceptance)}
			switch mode {
			case "missing_manifest":
				run.EvidenceManifest = nil
			case "missing_binding":
				manifest.EvidenceResults = nil
			case "foreign_binding":
				binding.RunID = "foreign"
			case "foreign_manifest":
				manifest.RunID = "foreign"
			case "changed_snapshot":
				manifest.RecoverySnapshotHash = "changed"
			case "changed_output":
				item.TypedResult.RuntimeOutputs["scope"] = false
			case "changed_receipt":
				receipt.RunID = "foreign"
			}
			audit := &AuditVerificationResult{RunID: run.RunID}
			result := verifyRunInputBindingDimension(run.RunID, run, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit)
			if (result.Status == AuditDimensionPass) != (mode == "accepted") {
				t.Fatalf("dimension=%+v findings=%+v", result, audit.Findings)
			}
		})
	}
}

func TestVerifyRunInputBindingDimensionAcceptsInvocationBoundWorkers(t *testing.T) {
	for _, typed := range []bool{false, true} {
		for _, mutation := range []string{"none", "task_id", "task_hash", "receipt_hash", "missing_receipt", "failed_receipt"} {
			t.Run(fmt.Sprintf("typed_%t/%s", typed, mutation), func(t *testing.T) {
				var definitions []team.RunInputDefinition
				if typed {
					definitions = []team.RunInputDefinition{{Name: "scope", Schema: team.RunInputSchema{Type: "string"}, Default: []byte(`"original"`)}}
				}
				snapshot, err := team.ResolveRunInputSnapshot(definitions, nil, "run", "invocation", "generic")
				if err != nil {
					t.Fatal(err)
				}
				item := &team.TodoItem{ID: "1", Status: team.TaskDone, RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash,
					ExecutionReceipt: &team.ExecutionReceipt{RunID: "run", TaskID: "1", Attempt: 1, ExitCode: new(0),
						RunInputSnapshotID: snapshot.ID, RunInputSnapshotHash: snapshot.SnapshotHash}}
				switch mutation {
				case "task_id":
					item.RunInputSnapshotID = ""
				case "task_hash":
					item.RunInputSnapshotHash = "foreign"
				case "receipt_hash":
					item.ExecutionReceipt.RunInputSnapshotHash = "foreign"
				case "missing_receipt":
					item.ExecutionReceipt = nil
				case "failed_receipt":
					item.ExecutionReceipt.ExitCode = new(1)
				}
				run := &team.RunResult{RunID: "run", RunInputs: snapshot}
				audit := &AuditVerificationResult{RunID: run.RunID}
				got := verifyRunInputBindingDimension(run.RunID, run, []*team.TodoItem{item}, []team.RunInputSnapshot{*snapshot}, audit)
				want := AuditDimensionFail
				if mutation == "none" {
					want = AuditDimensionPass
				}
				if got.Status != want {
					t.Fatalf("binding=%+v findings=%+v", got, audit.Findings)
				}
			})
		}
	}
}
