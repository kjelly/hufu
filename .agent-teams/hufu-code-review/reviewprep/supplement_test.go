package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/tools"
)

func frozenSupplementFixture(t *testing.T) (string, frozenSupplementRequest) {
	t.Helper()
	source, producer := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(source, "source.go"), "package sample\nfunc Target() {}\n")
	snapshot := &reviewSourceSnapshot{root: source, revision: "frozen", baseRevision: "base"}
	frozen, err := snapshot.persistFrozenSource(producer, filepath.Join(producer, "out"))
	if err != nil {
		t.Fatal(err)
	}
	return producer, frozenSupplementRequest{SourceTaskID: "3", SourceHash: strings.Repeat("a", 64), ReviewRevision: "frozen", Bindings: map[string]string{"frozen_source_manifest": frozen.Path, "frozen_source_sha256": frozen.SHA256, "frozen_source_root": snapshot.frozenRoot}, Gap: supplementGap{Kind: "missing_dependency", Description: "Caller missing", MissingEvidence: "source.go", RequestedEvidence: []supplementEvidence{{Path: "source.go", Ranges: []tools.ArtifactByteRange{}}}, Limitations: []supplementLimitation{}}}
}

func TestSupplementUsesExactFrozenSourceAndRejectsInvalidRequests(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "escape", "version", "content", "range", "semantic", "budget", "no_request"} {
		t.Run(kind, func(t *testing.T) {
			producer, request := frozenSupplementFixture(t)
			remaining := maxSupplementBytes
			switch kind {
			case "missing":
				request.Gap.RequestedEvidence[0].Path = "missing.go"
			case "escape":
				request.Gap.RequestedEvidence[0].Path = "../source.go"
			case "version":
				request.ReviewRevision = "newer"
			case "content":
				writeFile(t, filepath.Join(producer, request.Bindings["frozen_source_root"], "source.go"), "changed")
			case "range":
				request.Gap.RequestedEvidence[0].Ranges = []tools.ArtifactByteRange{{Start: 0, End: 999}}
			case "semantic":
				request.Gap.Kind = "semantic_uncertainty"
			case "budget":
				remaining = 1
			case "no_request":
				request.Gap.RequestedEvidence = nil
			}
			candidate, _, limitation := prepareSupplementBatch(producer, request, remaining)
			if kind == "valid" {
				if limitation != nil || candidate == nil || !strings.Contains(string(candidate.sourceBundle), "func Target()") {
					t.Fatalf("frozen evidence missing: %v", limitation)
				}
			} else if candidate != nil || limitation == nil || limitation.Kind == "" {
				t.Fatalf("invalid request materialized: %v", limitation)
			}
		})
	}
}

func TestSupplementRoundIsBoundedAndPreservesGapIdentity(t *testing.T) {
	producer, request := frozenSupplementFixture(t)
	requests := frozenSupplementRequests{Round: 1, SnapshotID: "inputs", Requests: []frozenSupplementRequest{}}
	for i := 0; i < maxSupplementItems+2; i++ {
		copy := request
		copy.GapIndex = i
		requests.Requests = append(requests.Requests, copy)
	}
	root := t.TempDir()
	result, err := materializeReviewSupplement(t.Context(), root, producer, scopeAttestation{}, requests, maxSupplementItems)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := result.Outputs["supplement_outcomes"].([]supplementOutcome)
	if len(outcomes) != len(requests.Requests) || result.Outputs["item_count"] != maxSupplementItems {
		t.Fatal("single round exceeded its item bound or lost gaps")
	}
	for index, outcome := range outcomes {
		if outcome.SourceTaskID != request.SourceTaskID || outcome.GapIndex != index || outcome.Kind != request.Gap.Kind {
			t.Fatal("original gap identity changed")
		}
		if index >= maxSupplementItems && (outcome.WorksetKey != "" || len(outcome.Limitations) != 1 || outcome.Limitations[0].Kind != "budget_exhausted") {
			t.Fatal("over-budget gaps lost typed limitations")
		}
	}
	m := readManifest(t, filepath.Join(root, "supplement", "workset-manifest.json"))
	if len(m.Items) != maxSupplementItems {
		t.Fatal("manifest exceeded round bound")
	}
	for _, entry := range m.Items {
		assertRequiredArtifactInputs(t, root, entry)
	}
	if _, err := materializeReviewSupplement(t.Context(), root, producer, scopeAttestation{}, requests, maxSupplementItems); err == nil {
		t.Fatal("second publication reused nonempty output")
	}
}

func TestSupplementUnavailableEvidenceKeepsLimitationsWithoutDispatch(t *testing.T) {
	producer, request := frozenSupplementFixture(t)
	request.Gap.RequestedEvidence[0].Path = "not-retained.go"
	root := t.TempDir()
	result, err := materializeReviewSupplement(t.Context(), root, producer, scopeAttestation{}, frozenSupplementRequests{Round: 1, Requests: []frozenSupplementRequest{request}}, maxSupplementItems)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := result.Outputs["supplement_outcomes"].([]supplementOutcome)
	if result.Outputs["item_count"] != 0 || len(outcomes) != 1 || outcomes[0].Limitations[0].Kind != "unavailable_path" {
		t.Fatal("unavailable evidence was hidden")
	}
	// Empty manifests are retained audit outputs; the coordinator must not dispatch them.
	data, err := os.ReadFile(filepath.Join(root, "supplement", "workset-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil || len(value.Items) != 0 {
		t.Fatal("unexpected work was published")
	}
}

func TestEmbeddedSupplementUsesReceiptBoundFrozenRequests(t *testing.T) {
	root := t.TempDir()
	producer, request := frozenSupplementFixture(t)
	producerRoot := filepath.Join(root, "runtime", "runs", "run-1", "actions", "original")
	if err := os.MkdirAll(filepath.Dir(producerRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(producer, producerRoot); err != nil {
		t.Fatal(err)
	}
	requestedScope := lastNScope(1)
	scope := scopeAttestation{Requested: requestedScope, RequestedInputHash: scopeInputDigest(requestedScope), Satisfied: true, Resolved: resolvedScope{Base: "base", Head: "frozen"}}
	sourcePayload := map[string]any{"record": map[string]any{"review_revision": "frozen", "gaps": []supplementGap{request.Gap}}}
	payloadBytes, err := json.Marshal(sourcePayload)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPayload, err := canonicalSupplementJSON(payloadBytes)
	if err != nil {
		t.Fatal(err)
	}
	request.SourceHash = sha256Hex(canonicalPayload)
	requests := frozenSupplementRequests{Round: 1, SnapshotID: "inputs", Requests: []frozenSupplementRequest{request}}
	actionTask := func(id, contract, action string, outputs map[string]any) map[string]any {
		data, err := json.Marshal(outputs)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := canonicalSupplementJSON(data)
		if err != nil {
			t.Fatal(err)
		}
		digest := "sha256:" + sha256Hex(canonical)
		return map[string]any{"ID": id, "Status": "done", "contract_id": contract, "run_input_snapshot_id": "inputs", "execution_receipt": map[string]any{"run_id": "run-1", "task_id": id, "attempt": 1, "action_invocation_id": action, "runtime_outputs_hash": digest, "producer_id": "reviewer", "model_execution_id": "model-execution", "transcript_ref": "transcript"}, "VerifyResult": map[string]any{"exit_code": 0, "fingerprint": "verified"}, "typed_result": map[string]any{"status": "success", "runtime_outputs": outputs, "runtime_outputs_hash": digest}}
	}
	original := actionTask("1", "produce-workset", "original", map[string]any{"scope": scope})
	inventory := actionTask("2", "inventory-review-supplement-requests", "inventory", map[string]any{"review_supplement_requests": requests})
	sourceTask := actionTask("3", "review-primary-workset", "", nil)
	sourceTask["run_input_snapshot_id"] = ""
	sourceTask["workset_binding"] = map[string]any{"bindings": request.Bindings}
	sourceTask["typed_result"] = map[string]any{"status": "completed_with_gaps", "structured_payload": map[string]any{"value": sourcePayload, "sha256": request.SourceHash}}
	active := map[string]any{"ID": "4", "Status": "in_progress", "contract_id": "prepare-review-supplement", "run_input_snapshot_id": "inputs"}
	tasks := []map[string]any{original, inventory, sourceTask, active}
	save := func() {
		data, err := json.Marshal(map[string]any{"active_run_input_snapshot_id": "inputs", "tasks": tasks})
		if err != nil {
			t.Fatal(err)
		}
		store, err := runtimestore.Open(t.Context(), root)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutDocument(t.Context(), runtimestore.DocumentSession, data); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	save()
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, "runtime", "runs", "run-1", "actions", "supplement")
	if err := os.MkdirAll(staging, 0755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HUFU_WORKSPACE="+staging, "HUFU_RUN_ID=run-1", "HUFU_ACTION_INVOCATION_ID=supplement", "HUFU_TASK_ID=4", "HUFU_RECOVERED_TASK_RUNS=")
	payload, err := json.Marshal(map[string]any{"scope": requestedScope})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(actionRequest{Type: "prepare_review_supplement", Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := golangruntime.Execute(t.Context(), "", program, envelope, env, 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("embedded supplement: %v %s", err, result.Stderr)
	}
	var output actionResult
	if err := json.Unmarshal(result.Stdout, &output); err != nil {
		t.Fatal(err)
	}
	if output.Outputs["item_count"] != float64(1) {
		t.Fatalf("native supplement failed: %s", result.Stdout)
	}
	// Reusing a request after its accepted source identity changes must fail.
	sourceTask["typed_result"].(map[string]any)["structured_payload"].(map[string]any)["sha256"] = strings.Repeat("b", 64)
	save()
	result, err = golangruntime.Execute(t.Context(), "", program, envelope, env, 1<<20, 16<<10)
	if err == nil || !strings.Contains(string(result.Stderr), "supplement source receipt mismatch") {
		t.Fatalf("substituted source did not fail at receipt binding: %v %s", err, result.Stderr)
	}
}
