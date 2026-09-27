package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func consolidationTestItem(id, project string, metadata map[string]string) contextstore.ContextItem {
	return contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: id, Scope: contextstore.Scope{ProjectID: project, TeamID: "team"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: metadata}
}

func manualConsolidationArgs(workspace string) []string {
	return []string{"context", "consolidate", "--apply-proposal", "--source", "src-a,src-b", "--proposal-text", "Run go test and go vet before committing.", "--workspace", workspace, "--project", "proj1", "--team", "demo"}
}

func candidateLifecycle(t *testing.T, workspace, id string) contextstore.ContextLifecycle {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	item, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return item.Lifecycle
}

func TestConsolidationReviewCommandsKeepProposalAndCandidateConsistent(t *testing.T) {
	workspace, _ := consolidationDraftFixture(t)
	if out, err := helperRunConsolidateCLI(manualConsolidationArgs(workspace)...); err != nil || !strings.Contains(out, "status=proposed") {
		t.Fatalf("manual proposal = %q err=%v", out, err)
	}
	proposal, candidate := pendingProposal(t, workspace)
	if out, err := helperRunConsolidateCLI(manualConsolidationArgs(workspace)...); err != nil || !strings.Contains(out, "already pending") {
		t.Fatalf("rerun = %q err=%v, want the pending proposal", out, err)
	}
	review := func(verb string) (string, error) {
		return helperRunConsolidateCLI("context", "consolidation", verb, proposal.ID, "--workspace", workspace, "--project", "proj1")
	}
	if _, err := review("approve"); err != nil {
		t.Fatalf("approve err = %v", err)
	}
	if got := candidateLifecycle(t, workspace, candidate.ID); got != contextstore.LifecycleConfirmed {
		t.Fatalf("approved candidate lifecycle = %s", got)
	}
	if _, err := review("reject"); err == nil || !strings.Contains(err.Error(), "supersede") {
		t.Fatalf("reject approved err = %v", err)
	}
	if got := candidateLifecycle(t, workspace, candidate.ID); got != contextstore.LifecycleConfirmed {
		t.Fatalf("reject of an approved proposal changed the candidate to %s", got)
	}
	if _, err := review("approve"); err == nil || !strings.Contains(err.Error(), "not pending") {
		t.Fatalf("second approve err = %v", err)
	}
}

func TestConsolidationReviewHidesOtherProjects(t *testing.T) {
	workspace, _ := consolidationDraftFixture(t)
	if _, err := helperRunConsolidateCLI(manualConsolidationArgs(workspace)...); err != nil {
		t.Fatal(err)
	}
	proposal, _ := pendingProposal(t, workspace)
	_, missingErr := helperRunConsolidateCLI("context", "consolidation", "show", "consolidation-missing", "--workspace", workspace, "--project", "other")
	_, otherErr := helperRunConsolidateCLI("context", "consolidation", "show", proposal.ID, "--workspace", workspace, "--project", "other")
	if missingErr == nil || otherErr == nil {
		t.Fatalf("show errors = %v / %v", missingErr, otherErr)
	}
	normalize := func(err error, id string) string { return strings.ReplaceAll(err.Error(), id, "<id>") }
	if normalize(missingErr, "consolidation-missing") != normalize(otherErr, proposal.ID) {
		t.Fatalf("missing and out-of-project errors differ: %q vs %q", missingErr, otherErr)
	}
}

func TestGenericConfirmAndRejectRefuseConsolidationCandidates(t *testing.T) {
	workspace, _ := consolidationDraftFixture(t)
	if _, err := helperRunConsolidateCLI(manualConsolidationArgs(workspace)...); err != nil {
		t.Fatal(err)
	}
	proposal, candidate := pendingProposal(t, workspace)
	for _, args := range [][]string{
		{"context", "confirm", "--workspace", workspace, "--project", "proj1", "--team", "demo", "--evidence", "bypass", candidate.ID},
		{"context", "reject", "--workspace", workspace, "--project", "proj1", "--team", "demo", "--reason", "bypass", candidate.ID},
	} {
		if _, err := helperRunConsolidateCLI(args...); err == nil || !strings.Contains(err.Error(), "hufu context consolidation approve|reject "+proposal.ID) {
			t.Fatalf("%s err = %v, want a pointer to the consolidation review commands", args[1], err)
		}
	}
	if got := candidateLifecycle(t, workspace, candidate.ID); got != contextstore.LifecycleCandidate {
		t.Fatalf("generic command changed the consolidation candidate to %s", got)
	}
}
