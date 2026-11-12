package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kjelly/hufu/internal/golangruntime"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/tools"
)

func TestReviewBudgetFailureExplainsFixedEvidenceOverhead(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "primary.go"), "package sample\nfunc Context() {}\n")
	for i := range 30 {
		writeFile(t, filepath.Join(root, "related", fmt.Sprintf("related%d.go", i)), "package related\nfunc Caller() { Context() }\n")
	}
	snapshot := &reviewSourceSnapshot{root: root, revision: "frozen", baseRevision: "base"}
	source, inventory, err := snapshot.bundleDataWithScopes("workset/batches/unit-0000", []string{"primary.go"}, map[string][]tools.ArtifactByteRange{"primary.go": {{Start: 0, End: 1}}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	current := &batch{paths: []string{"primary.go"}}
	config := Config{MaxSourceSnapshotBytes: 1000, MaxReviewInputTokens: 200000}
	failure := oversizedReviewInputError(current, source, inventory, estimateReviewInputTokens(len(source)+len(inventory)), config)
	var typed *reviewBudgetError
	if !errors.As(failure, &typed) || typed.diagnosis.CauseStatus != "confirmed" || !strings.Contains(typed.diagnosis.Cause, "without primary content") {
		t.Fatalf("diagnosis=%#v err=%v", typed, failure)
	}
	if typed.diagnosis.Code != "workset_item_too_large" || !strings.Contains(failure.Error(), "source_snapshot_bytes=") {
		t.Fatal("lost stable code or legacy evidence")
	}
	var out bytes.Buffer
	if err = writeReviewFailure(&out, failure); err == nil {
		t.Fatal("reporting a failure returned success")
	}
	var response struct {
		Error struct {
			Diagnosis reviewBudgetDiagnosis `json:"diagnosis"`
		} `json:"error"`
	}
	if err = json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	facts := map[string]string{}
	for _, fact := range response.Error.Diagnosis.Evidence {
		facts[fact.Name] = fact.Value
	}
	if facts["primary_content_bytes"] != "1" || facts["related_paths"] != "30" || facts["max_source_snapshot_bytes"] != "1000" {
		t.Fatalf("inexact evidence=%v", facts)
	}
}

func TestRunEmitsStructuredBudgetFailureWithoutPublishingWorksets(t *testing.T) {
	repo := newLargeSourceBudgetRepo(t)
	payload, err := json.Marshal(wireConfig{Repository: repo, OutputDir: "out", Scope: scopePointer(lastNScope(1)), MaxSourceSnapshotBytes: 400, MaxReviewInputTokens: 200000})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(actionRequest{Type: "prepare_review_workset", Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Run(t.Context(), bytes.NewReader(request), &out); err == nil {
		t.Fatal("budget failure succeeded")
	}
	var envelope struct {
		Error struct {
			Class     string                `json:"failure_class"`
			Diagnosis reviewBudgetDiagnosis `json:"diagnosis"`
		} `json:"error"`
	}
	if err = json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid response: %v %s", err, out.String())
	}
	if envelope.Error.Class != "execution" || envelope.Error.Diagnosis.Code != "workset_item_too_large" {
		t.Fatalf("response=%s", out.String())
	}
	if _, err = os.Stat(filepath.Join(repo, "out")); !os.IsNotExist(err) {
		t.Fatalf("failure published workset: %v", err)
	}
}

func scopePointer(scope resolverScope) *resolverScope { return &scope }

func TestEmbeddedRuntimeEmitsReviewBudgetDiagnosis(t *testing.T) {
	repo := newLargeSourceBudgetRepo(t)
	scope := lastNScope(1)
	payload := wireConfig{Repository: repo, OutputDir: "out", Scope: &scope, MaxSourceSnapshotBytes: 400, MaxReviewInputTokens: 200000}
	request := mustJSON(t, actionRequest{Type: "prepare_review_workset", Payload: string(mustJSON(t, payload))})
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	result, err := golangruntime.Execute(t.Context(), "", program, request, os.Environ(), 1<<20, 16<<10)
	if err == nil {
		t.Fatal("embedded action succeeded")
	}
	var envelope struct {
		Error struct {
			Diagnosis reviewBudgetDiagnosis `json:"diagnosis"`
		} `json:"error"`
	}
	if decodeErr := json.Unmarshal(result.Stdout, &envelope); decodeErr != nil {
		t.Fatalf("missing failure envelope: %v stderr=%s stdout=%s", err, result.Stderr, result.Stdout)
	}
	if envelope.Error.Diagnosis.Code != "workset_item_too_large" {
		t.Fatalf("missing team diagnosis: %s stderr=%s", result.Stdout, result.Stderr)
	}
}
