package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
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
	return producer, frozenSupplementRequest{SourceTaskID: "3", SourceHash: strings.Repeat("a", 64), ReviewRevision: "frozen", Bindings: map[string]string{"frozen_source_manifest": frozen.Path, "frozen_source_sha256": frozen.SHA256, "frozen_source_root": snapshot.frozenRoot}, Kind: "missing_dependency", Description: "Caller missing", MissingEvidence: "source.go", RequestedEvidence: []supplementEvidence{{Path: "source.go", Ranges: []tools.ArtifactByteRange{}}}, Limitations: []supplementLimitation{}}
}

func TestSupplementUsesExactFrozenSourceAndRejectsInvalidRequests(t *testing.T) {
	for _, kind := range []string{"valid", "missing", "directory", "escape", "version", "content", "range", "semantic", "budget", "no_request"} {
		t.Run(kind, func(t *testing.T) {
			producer, request := frozenSupplementFixture(t)
			remaining := maxSupplementBytes
			switch kind {
			case "missing":
				request.RequestedEvidence[0].Path = "missing.go"
			case "directory":
				request.RequestedEvidence[0].Path = "internal/inspect/"
			case "escape":
				request.RequestedEvidence[0].Path = "../source.go"
			case "version":
				request.ReviewRevision = "newer"
			case "content":
				writeFile(t, filepath.Join(producer, request.Bindings["frozen_source_root"], "source.go"), "changed")
			case "range":
				request.RequestedEvidence[0].Ranges = []tools.ArtifactByteRange{{Start: 0, End: 999}}
			case "semantic":
				request.Kind = "semantic_uncertainty"
			case "budget":
				remaining = 1
			case "no_request":
				request.RequestedEvidence = nil
			}
			candidate, _, limitation := prepareSupplementBatch(producer, request, remaining)
			if kind == "valid" {
				if limitation != nil || candidate == nil || !strings.Contains(string(candidate.sourceBundle), "func Target()") {
					t.Fatalf("frozen evidence missing: %v", limitation)
				}
			} else if candidate != nil || limitation == nil || limitation.Kind == "" {
				t.Fatalf("invalid request materialized: %v", limitation)
			} else if (kind == "directory" || kind == "escape" || kind == "range") && limitation.Kind != "invalid_request" {
				t.Fatalf("unsafe request lacked its typed rejection: %v", limitation)
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
		if outcome.SourceTaskID != request.SourceTaskID || outcome.GapIndex != index || outcome.Kind != request.Kind {
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
	assertSupplementDeclarations(t, root, result, m)
	if _, err := materializeReviewSupplement(t.Context(), root, producer, scopeAttestation{}, requests, maxSupplementItems); err == nil {
		t.Fatal("second publication reused nonempty output")
	}
}

func assertSupplementDeclarations(t *testing.T, root string, result actionResult, m manifest) {
	t.Helper()
	declarations := map[string]artifact{}
	for _, ref := range result.Artifacts {
		if _, exists := declarations[ref.ID]; exists {
			t.Fatal("repeated content has multiple producer declarations")
		}
		declarations[ref.ID] = ref
	}
	for _, entry := range m.Items {
		assertRequiredArtifactInputs(t, root, entry)
		for _, ref := range entry.Inputs {
			if ref != declarations[ref.ID] {
				t.Fatal("workset input differs from its canonical producer declaration")
			}
			if ref.Kind == "review_evidence_inventory" && entry.Bindings["evidence_inventory"] != ref.Path {
				t.Fatal("workset evidence binding retained a noncanonical alias")
			}
		}
	}
}

func TestSupplementWithoutItemsKeepsLimitationsWithoutManifest(t *testing.T) {
	for _, kind := range []string{"no_requests", "no_paths", "unavailable", "budget"} {
		t.Run(kind, func(t *testing.T) {
			producer, request := frozenSupplementFixture(t)
			requests := []frozenSupplementRequest{request}
			wantOutcomes, wantLimit := 1, "unavailable_path"
			limit := maxSupplementItems
			switch kind {
			case "no_requests":
				requests = nil
				wantOutcomes = 0
			case "no_paths":
				requests[0].RequestedEvidence = nil
				wantLimit = "invalid_request"
			case "unavailable":
				requests[0].RequestedEvidence[0].Path = "not-retained.go"
			case "budget":
				limit, wantLimit = 0, "budget_exhausted"
			}
			root := t.TempDir()
			result, err := materializeReviewSupplement(t.Context(), root, producer, scopeAttestation{}, frozenSupplementRequests{Round: 1, Requests: requests}, limit)
			if err != nil {
				t.Fatal(err)
			}
			outcomes := result.Outputs["supplement_outcomes"].([]supplementOutcome)
			if result.Outputs["item_count"] != 0 || result.Outputs["supplement_prepared"] != true || len(outcomes) != wantOutcomes || len(result.Artifacts) != 0 {
				t.Fatalf("zero-item output = %+v", result)
			}
			if wantOutcomes > 0 && (outcomes[0].SourceTaskID != request.SourceTaskID || outcomes[0].GapIndex != request.GapIndex || outcomes[0].WorksetKey != "" || len(outcomes[0].Limitations) != 1 || outcomes[0].Limitations[0].Kind != wantLimit) {
				t.Fatal("original request identity or typed limitation was lost")
			}
			if _, err := os.Stat(filepath.Join(root, "supplement", "workset-manifest.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("empty executable manifest was retained: %v", err)
			}
		})
	}
}

func TestEmbeddedSupplementUsesReceiptBoundFrozenRequests(t *testing.T) {
	for _, kind := range []string{"valid", "directory", "escape", "range"} {
		t.Run(kind, func(t *testing.T) { testEmbeddedSupplementUsesReceiptBoundFrozenRequests(t, kind) })
	}
}

func testEmbeddedSupplementUsesReceiptBoundFrozenRequests(t *testing.T, kind string) {
	t.Helper()
	root := t.TempDir()
	producer, request := frozenSupplementFixture(t)
	request.RequestedEvidence[0].Ranges = []tools.ArtifactByteRange{{Start: 0, End: 5}}
	switch kind {
	case "directory":
		request.RequestedEvidence[0].Path = "internal/inspect/"
	case "escape":
		request.RequestedEvidence[0].Path = "../source.go"
	case "range":
		request.RequestedEvidence[0].Ranges[0].End = 999
	}
	producerRoot := filepath.Join(root, "runtime", "runs", "run-1", "actions", "original")
	if err := os.MkdirAll(filepath.Dir(producerRoot), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(producer, producerRoot); err != nil {
		t.Fatal(err)
	}
	requestedScope := lastNScope(1)
	scope := scopeAttestation{Requested: requestedScope, RequestedInputHash: scopeInputDigest(requestedScope), Satisfied: true, Resolved: resolvedScope{Base: "base", Head: "frozen"}}
	sourcePayload := map[string]any{"record": map[string]any{"review_revision": "frozen", "gaps": []supplementGap{request.gap()}}}
	if kind == "valid" {
		sourcePayload["record"].(map[string]any)["gaps"] = []supplementGap{request.gap(), request.gap()}
	}
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
	if kind == "valid" {
		duplicate := request
		duplicate.GapIndex = 1
		requests.Requests = append(requests.Requests, duplicate)
	}
	actionTask := func(id, contract, action string, outputs map[string]any) map[string]any {
		data, err := json.Marshal(outputs)
		if err != nil {
			t.Fatal(err)
		}
		var values map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			t.Fatal(err)
		}
		// Generate the receipt at the real runtime boundary, independently of
		// the adapter verifier. Its canonical format differs from payload JSON.
		canonical, digest, err := team.CanonicalizeRuntimeOutputs(values)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"ID": id, "Status": "done", "contract_id": contract, "run_input_snapshot_id": "inputs", "execution_receipt": map[string]any{"run_id": "run-1", "task_id": id, "attempt": 1, "action_invocation_id": action, "runtime_outputs_hash": digest, "producer_id": "reviewer", "model_execution_id": "model-execution", "transcript_ref": "transcript"}, "VerifyResult": map[string]any{"exit_code": 0, "fingerprint": "verified"}, "typed_result": map[string]any{"status": "success", "runtime_outputs": canonical, "runtime_outputs_hash": digest}}
	}
	original := actionTask("1", "produce-workset", "original", map[string]any{"scope": scope, "wide_counter": json.Number("9007199254740993")})
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
	wantItems := float64(2)
	if kind != "valid" {
		wantItems = 0
		var outcomes []supplementOutcome
		encoded, err := json.Marshal(output.Outputs["supplement_outcomes"])
		if err != nil || json.Unmarshal(encoded, &outcomes) != nil || len(outcomes) != 1 || outcomes[0].WorksetKey != "" || len(outcomes[0].Limitations) != 1 || outcomes[0].Limitations[0].Kind != "invalid_request" {
			t.Fatalf("unsafe request lost its typed limitation: %s", result.Stdout)
		}
	}
	if output.Outputs["item_count"] != wantItems {
		t.Fatalf("native supplement failed: %s", result.Stdout)
	}
	if wantItems == 0 && len(output.Artifacts) != 0 {
		t.Fatal("interpreted producer declared an empty executable workset")
	}
	if kind == "valid" {
		m := readManifest(t, filepath.Join(staging, "supplement", "workset-manifest.json"))
		assertSupplementDeclarations(t, staging, output, m)
		if len(m.Items) != 2 || m.Items[0].Bindings["source_gap_index"] != "0" || m.Items[1].Bindings["source_gap_index"] != "1" {
			t.Fatal("canonical content reuse merged distinct gap identities")
		}
	}
	// Preserve exact runtime output numbers, and reject changed bytes even when
	// the task and receipt still claim the original hash.
	originalOutputs := original["typed_result"].(map[string]any)["runtime_outputs"].(map[string]any)
	originalOutputs["wide_counter"] = json.Number("9007199254740992")
	save()
	result, err = golangruntime.Execute(t.Context(), "", program, envelope, env, 1<<20, 16<<10)
	if err == nil || !strings.Contains(string(result.Stderr), "runtime outputs differ from its receipt") {
		t.Fatalf("changed runtime output escaped receipt validation: %v %s", err, result.Stderr)
	}
	originalOutputs["wide_counter"] = json.Number("9007199254740993")
	// Reusing a request after its accepted source identity changes must fail.
	sourceTask["typed_result"].(map[string]any)["structured_payload"].(map[string]any)["sha256"] = strings.Repeat("b", 64)
	save()
	result, err = golangruntime.Execute(t.Context(), "", program, envelope, env, 1<<20, 16<<10)
	if err == nil || !strings.Contains(string(result.Stderr), "supplement source receipt mismatch") {
		t.Fatalf("substituted source did not fail at receipt binding: %v %s", err, result.Stderr)
	}
}

// TestSupplementRuntimeActionFixture is invoked only by the runtime integration
// test. It uses the real materializer and writes its ordinary action envelope
// inside the runtime-assigned action root; no model or production service runs.
func TestSupplementRuntimeActionFixture(t *testing.T) {
	root := os.Getenv("HUFU_TEST_SUPPLEMENT_OUTPUT")
	if root == "" {
		t.Skip("runtime subprocess fixture")
	}
	producer, request := frozenSupplementFixture(t)
	requests := []frozenSupplementRequest{request}
	limit := maxSupplementItems
	switch os.Getenv("HUFU_TEST_SUPPLEMENT_CASE") {
	case "no_requests":
		requests = nil
	case "no_paths":
		requests[0].RequestedEvidence = nil
		requests[0].Limitations = []supplementLimitation{{Kind: "unavailable_path", Detail: "Exact file unavailable in the assigned workset"}}
	case "unavailable":
		requests[0].RequestedEvidence[0].Path = "not-retained.go"
	case "mixed":
		missing := request
		missing.GapIndex = 1
		missing.RequestedEvidence = nil
		requests = append(requests, missing)
	case "budget":
		limit = 0
	case "valid":
	default:
		t.Fatal("unknown runtime fixture case")
	}
	scope := scopeAttestation{Requested: lastNScope(1), RequestedInputHash: scopeInputDigest(lastNScope(1)), Satisfied: true, Resolved: resolvedScope{Base: "base", Head: "frozen"}}
	result, err := materializeReviewSupplement(t.Context(), root, producer, scope, frozenSupplementRequests{Round: 1, Requests: requests}, limit)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixture-response.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func canonicalSupplementJSON(data []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
