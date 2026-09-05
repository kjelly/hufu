package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// resetDecisionCLIFlags clears every decision subcommand's package-level flag
// variable. pflag only assigns a flag's variable when that flag appears in
// argv, so a value left by an earlier case would otherwise leak into the next
// one. Call this at the start of every case.
func resetDecisionCLIFlags() {
	decisionWorkspace, decisionJSON, decisionAll = "", false, false
	decisionResolveOutcome, decisionResolveBy, decisionResolveNotes = "", "", ""
	decisionResolveEvidence, decisionResolveLessons = nil, nil
	decisionResolveForecast = 0
}

// buildDecisionCLIFixture writes a workspace holding two unresolved decisions
// and one artifact, through the same exported primitives the runtime uses.
func buildDecisionCLIFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	for i, entry := range []team.DecisionIndexEntry{
		{
			DecisionID: "dec-1", RunID: "run-42", TaskID: "t1", Profile: "high-stakes",
			EvidenceHash: "abc123", Question: "Should we migrate now?",
			FinalOption: "migrate", Probability: 0.7, ForecastRequired: true,
			FalsificationConditions: []string{"ingest lag exceeds 5m"},
			RecordPath:              "decisions/dec-1.json", CreatedAt: base,
		},
		{
			DecisionID: "dec-2", RunID: "run-43", Profile: "standard",
			Question: "Roll back?", FinalOption: "defer", Probability: 0.4,
			CreatedAt: base.Add(time.Hour),
		},
	} {
		if err := index.Append(entry); err != nil {
			t.Fatalf("seeding entry %d: %v", i, err)
		}
	}

	metaDir := filepath.Join(workspace, "logs", "artifacts", "meta")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"art-9","path":"reports/lag.json","sha256":"deadbeef","media_type":"application/json"}`
	if err := os.WriteFile(filepath.Join(metaDir, "art-9.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

// runDecisionCLI executes through a fresh root, the way cmd/hufu's other CLI
// tests do. Calling Execute() on the subcommand would walk up to the shared
// root and re-run whatever args another test left there.
func runDecisionCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"decision"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestDecisionListShowsUnresolvedDecisions(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "list", "-w", workspace)
	if err != nil {
		t.Fatalf("decision list = %v", err)
	}
	for _, want := range []string{"dec-1", "dec-2", "high-stakes", "unresolved"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDecisionListJSON(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "list", "-w", workspace, "--json")
	if err != nil {
		t.Fatalf("decision list --json = %v", err)
	}
	var payload struct {
		Index   string                    `json:"index"`
		Count   int                       `json:"count"`
		Entries []team.DecisionIndexEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not a single JSON object: %v\n%s", err, out)
	}
	if payload.Count != 2 || len(payload.Entries) != 2 {
		t.Fatalf("payload = %#v, want two entries", payload)
	}
	if !strings.HasSuffix(payload.Index, filepath.Join("logs", "decisions", "index.jsonl")) {
		t.Fatalf("index path = %q", payload.Index)
	}
}

// Resolving with evidence that exists in the store marks the outcome verified.
func TestDecisionResolveWithResolvableEvidenceIsVerified(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace,
		"--outcome", "succeeded", "--evidence", "deadbeef",
		"--lesson", "lag never exceeded 2m", "--resolved-by", "ops")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: true") {
		t.Fatalf("output did not report a verified outcome:\n%s", out)
	}
	if !strings.Contains(out, "The decision record was not modified.") {
		t.Fatalf("output did not state the record was untouched:\n%s", out)
	}

	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	entry, found, err := index.Get("dec-1")
	if err != nil || !found {
		t.Fatalf("Get = %v, %v", found, err)
	}
	if entry.Outcome == nil || !entry.Outcome.Verified {
		t.Fatalf("stored outcome = %#v, want verified", entry.Outcome)
	}
	if entry.Outcome.Forecast != 0.7 {
		t.Fatalf("Forecast = %v, want the decision's own probability", entry.Outcome.Forecast)
	}
	// Resolving must not change what was decided.
	if entry.FinalOption != "migrate" || entry.EvidenceHash != "abc123" {
		t.Fatalf("resolution changed the decision: %#v", entry)
	}
}

// An operator's assertion, with no resolvable evidence, is recorded but never
// counted as verified.
func TestDecisionResolveWithoutEvidenceIsUnverified(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "resolve", "dec-2", "-w", workspace,
		"--outcome", "failed", "--resolved-by", "a very confident operator")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: false") {
		t.Fatalf("an assertion was reported as verified:\n%s", out)
	}

	resetDecisionCLIFlags()
	workspace2 := buildDecisionCLIFixture(t)
	out, err = runDecisionCLI(t, "resolve", "dec-2", "-w", workspace2,
		"--outcome", "failed", "--evidence", "notinthestore")
	if err != nil {
		t.Fatalf("decision resolve = %v\n%s", err, out)
	}
	if !strings.Contains(out, "Verified: false") {
		t.Fatalf("an unresolvable digest was reported as verified:\n%s", out)
	}
}

func TestDecisionResolveRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "missing outcome",
			args:    []string{"resolve", "dec-1"},
			wantErr: "--outcome is required",
		},
		{
			name:    "undeclared outcome",
			args:    []string{"resolve", "dec-1", "--outcome", "went-fine"},
			wantErr: "is not one of",
		},
		{
			name:    "unknown decision",
			args:    []string{"resolve", "dec-99", "--outcome", "succeeded"},
			wantErr: "is not in",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetDecisionCLIFlags()
			workspace := buildDecisionCLIFixture(t)
			args := append(append([]string(nil), tt.args...), "-w", workspace)
			_, err := runDecisionCLI(t, args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decision resolve = %v, want %q", err, tt.wantErr)
			}
			var exit *decisionExitError
			if !errorsAsDecisionExit(err, &exit) || exit.ProcessExitCode() != 2 {
				t.Fatalf("error = %#v, want a decisionExitError with code 2", err)
			}
		})
	}
}

// An outcome is recorded once; a second attempt is refused rather than
// overwriting what was already observed.
func TestDecisionResolveIsRecordedOnce(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	if _, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace, "--outcome", "succeeded"); err != nil {
		t.Fatal(err)
	}
	resetDecisionCLIFlags()
	_, err := runDecisionCLI(t, "resolve", "dec-1", "-w", workspace, "--outcome", "failed")
	if err == nil || !strings.Contains(err.Error(), "already resolved") {
		t.Fatalf("second resolution = %v, want a refusal", err)
	}
}

func TestDecisionShow(t *testing.T) {
	resetDecisionCLIFlags()
	workspace := buildDecisionCLIFixture(t)

	out, err := runDecisionCLI(t, "show", "dec-1", "-w", workspace)
	if err != nil {
		t.Fatalf("decision show = %v", err)
	}
	for _, want := range []string{"dec-1", "run-42", "migrate", "ingest lag exceeds 5m", "unresolved"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	resetDecisionCLIFlags()
	_, err = runDecisionCLI(t, "show", "dec-99", "-w", workspace)
	if err == nil || !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("show on an unknown decision = %v, want a not-found error", err)
	}
}

func TestDecisionListOnEmptyWorkspace(t *testing.T) {
	resetDecisionCLIFlags()
	out, err := runDecisionCLI(t, "list", "-w", t.TempDir())
	if err != nil {
		t.Fatalf("decision list on an empty workspace = %v", err)
	}
	if !strings.Contains(out, "No unresolved decisions") {
		t.Fatalf("output = %q, want an empty-state message", out)
	}
}

// errorsAsDecisionExit is a local errors.As without importing errors in the
// test's hot path assertions.
func errorsAsDecisionExit(err error, target **decisionExitError) bool {
	if exit, ok := err.(*decisionExitError); ok {
		*target = exit
		return true
	}
	return false
}
