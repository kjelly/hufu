package team

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestTaskResultReducerPreservesTypedSharedMemoryKinds(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{
		contextRepo: repo, projectDir: "project", executionRunID: "run-1",
		session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}},
	}
	c.reduceTaskResultToSharedMemory(context.Background(), TaskResultMemoryInput{
		TodoID: "task-1", Attempt: 1, Output: "fallback must not be used",
		Result: &TaskResult{
			Findings:      []Finding{{Summary: "adapter requires transaction", Detail: "commit after receipt"}},
			Decisions:     []Decision{{Topic: "storage", Choice: "SQLite", Reason: "canonical lifecycle"}},
			OpenQuestions: OpenQuestions{"which migration version?"},
		},
	})
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{
		Scope: contextstore.Scope{ProjectID: "project", TeamID: "team", SessionID: filepath.Base(workspace)}, Visibility: contextstore.VisibilityExact,
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[contextstore.ContextKind]bool)
	for _, item := range items {
		kinds[item.Kind] = true
		if item.Metadata["memory_lifetime"] != "session" || item.Metadata["run_id"] != "run-1" {
			t.Fatalf("shared working-memory provenance missing: %#v", item)
		}
	}
	for _, want := range []contextstore.ContextKind{contextstore.ContextObservation, contextstore.ContextDecision, contextstore.ContextOpenQuestion} {
		if !kinds[want] {
			t.Errorf("missing reduced kind %q: %#v", want, items)
		}
	}
	if kinds[contextstore.ContextProgress] {
		t.Fatalf("typed result should not be collapsed into progress: %#v", items)
	}
}

func TestTaskResultReducerUsesProgressOnlyForUntypedOutput(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "project", executionRunID: "run-1", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	c.reduceTaskResultToSharedMemory(context.Background(), TaskResultMemoryInput{TodoID: "task-1", Output: "worker completed README update", Attempt: 1})
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact})
	if err != nil || len(items) != 1 || items[0].Kind != contextstore.ContextProgress {
		t.Fatalf("untyped output progress = %#v, err=%v", items, err)
	}
}

func TestTaskResultReducerKeepsDistinctProvenanceForSameContentAcrossTasks(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "project", executionRunID: "run-1", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	// Two distinct tasks in one run report the same finding. They must not
	// collapse into one item with overwritten provenance.
	for _, todoID := range []string{"task-a", "task-b"} {
		c.reduceTaskResultToSharedMemory(context.Background(), TaskResultMemoryInput{
			TodoID: todoID, Attempt: 1,
			Result: &TaskResult{Findings: []Finding{{Summary: "adapter requires transaction"}}},
		})
	}
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("same-content findings from distinct tasks collapsed: %#v", items)
	}
	tasks := map[string]bool{}
	for _, item := range items {
		tasks[item.Metadata["task_id"]] = true
	}
	if !tasks["task-a"] || !tasks["task-b"] {
		t.Fatalf("reducer lost per-task provenance: %#v", items)
	}
}

func TestTaskResultReducerPopulatesArtifactAndVerificationEvidence(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "project", executionRunID: "run-1", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	c.reduceTaskResultToSharedMemory(context.Background(), TaskResultMemoryInput{
		TodoID: "task-1", Attempt: 1,
		Result: &TaskResult{
			Artifacts: []ArtifactRef{{ID: "art-1", Path: "out/report.md"}},
			Verification: []VerificationResult{{Command: "test -f out/report.md", ExitCode: 0, Fingerprint: "fp-1"}},
		},
	})
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[contextstore.ContextKind]contextstore.ContextItem{}
	for _, item := range items {
		kinds[item.Kind] = item
	}
	artifact, ok := kinds[contextstore.ContextArtifact]
	if !ok {
		t.Fatalf("reducer did not produce an artifact item: %#v", items)
	}
	if !hasEvidenceType(artifact.Evidence, "artifact", "art-1") {
		t.Fatalf("artifact item lost artifact-ID evidence: %#v", artifact.Evidence)
	}
	verification, ok := kinds[contextstore.ContextVerification]
	if !ok {
		t.Fatalf("reducer did not produce a verification item: %#v", items)
	}
	if !hasEvidenceType(verification.Evidence, "verification", "fp-1") {
		t.Fatalf("verification item lost fingerprint evidence: %#v", verification.Evidence)
	}
}

func hasEvidenceType(evidence []contextstore.EvidenceRef, typ, ref string) bool {
	for _, ev := range evidence {
		if ev.Type == typ && ev.Ref == ref {
			return true
		}
	}
	return false
}

func TestRecordVerificationFailureWritesContextError(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := &Coordinator{contextRepo: repo, projectDir: "project", executionRunID: "run-1", session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "team"}}}
	c.recordVerificationFailure(context.Background(), VerificationFailureInput{
		TodoID:      "task-1",
		Attempt:     2,
		Err:         errors.New("verification failed: artifact missing"),
		Verify:      &VerificationResult{Command: "test -f out/report.md", ExitCode: 1, Fingerprint: "fp-fail"},
		ReceiptIDs:  []string{"receipt-1"},
		ArtifactIDs: []string{"art-1"},
	})
	items, err := repo.Query(context.Background(), contextstore.RepositoryQuery{Scope: c.contextScope(), Visibility: contextstore.VisibilityExact})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("verification-failure reduction = %#v, want exactly one error item", items)
	}
	item := items[0]
	if item.Kind != contextstore.ContextError {
		t.Fatalf("failed verification stored as %q, want ContextError (not progress)", item.Kind)
	}
	if !strings.Contains(item.Content, "verification failed: artifact missing") {
		t.Fatalf("error item lost diagnostic: %q", item.Content)
	}
	if !strings.Contains(item.Content, "test -f out/report.md") {
		t.Fatalf("error item lost verification command: %q", item.Content)
	}
	if !hasEvidenceType(item.Evidence, "task", "task-1") {
		t.Fatalf("error item lost task evidence: %#v", item.Evidence)
	}
	if !hasEvidenceType(item.Evidence, "verification", "fp-fail") {
		t.Fatalf("error item lost verification-fingerprint evidence: %#v", item.Evidence)
	}
	if !hasEvidenceType(item.Evidence, "receipt", "receipt-1") {
		t.Fatalf("error item lost receipt evidence: %#v", item.Evidence)
	}
	if !hasEvidenceType(item.Evidence, "artifact", "art-1") {
		t.Fatalf("error item lost artifact evidence: %#v", item.Evidence)
	}
	if item.Metadata["verified"] != "false" || item.Metadata["task_id"] != "task-1" || item.Metadata["attempt"] != "2" {
		t.Fatalf("error item provenance metadata = %#v", item.Metadata)
	}
}
