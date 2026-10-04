package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// TestContextOutcomesShowsStrongEvidenceSeparately keeps the time an item was
// last verified visible next to the time it was last seen, so an operator can
// tell a memory that is merely retrieved from one that is still verified.
func TestContextOutcomesShowsStrongEvidenceSeparately(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	verified := time.Date(2026, 5, 1, 8, 0, 0, 0, time.UTC)
	retrieved := verified.Add(40 * 24 * time.Hour)
	for _, id := range []string{"verified-memory", "retrieved-memory"} {
		if err := repo.Append(t.Context(), contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: "procedure " + id, Scope: contextstore.Scope{ProjectID: "project-1"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
			t.Fatal(err)
		}
	}
	for _, observation := range []contextstore.ExperienceObservation{
		{IdempotencyKey: "a", ContextItemID: "verified-memory", PolicyVersion: "memory-policy-v1", TaskID: "run-1/1", PositiveWeight: 1, VerifiedSupportDelta: 1, StrongEvidence: true, ObservedAt: verified},
		{IdempotencyKey: "b", ContextItemID: "verified-memory", PolicyVersion: "memory-policy-v1", ExposureDelta: 1, ObservedAt: retrieved},
		{IdempotencyKey: "c", ContextItemID: "retrieved-memory", PolicyVersion: "memory-policy-v1", ExposureDelta: 1, ObservedAt: retrieved},
	} {
		if _, err := repo.ApplyExperienceObservation(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		root := newRootCommand()
		out := new(bytes.Buffer)
		root.SetOut(out)
		root.SetArgs(append([]string{"context", "outcomes", "--workspace", workspace, "--policy-version", "memory-policy-v1"}, args...))
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	text := run("verified-memory", "--json=false")
	for _, want := range []string{"last_observed_at: " + retrieved.Format(time.RFC3339) + "\n", "last_strong_evidence_at: " + verified.Format(time.RFC3339) + "\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("outcomes output lacks %q:\n%s", want, text)
		}
	}
	if text := run("retrieved-memory", "--json=false"); !strings.Contains(text, "last_strong_evidence_at: none\n") {
		t.Fatalf("an item never verified must show no strong evidence:\n%s", text)
	}

	var aggregate map[string]any
	if err := json.Unmarshal([]byte(run("retrieved-memory", "--json")), &aggregate); err != nil {
		t.Fatal(err)
	}
	if _, present := aggregate["last_strong_evidence_at"]; present {
		t.Fatalf("JSON for an item never verified carries a strong evidence time: %v", aggregate)
	}
}
