package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/consolidation"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func helperRunConsolidateCLI(args ...string) (string, error) {
	contextWorkspace, contextProject, contextTeam, contextAgent, contextTier, contextLifecycle = "", "", "", "", "", ""
	contextQueryJSON, contextApplyProposal, contextConsolidateDraft = false, false, false
	contextProposalText, contextProposalSources, contextConsolidateModel, contextConsolidateSearchPath = "", "", "", ""
	contextPolicyVersion = "memory-policy-v1"
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func stubConsolidationDrafter(t *testing.T, reply string) *int {
	t.Helper()
	calls := 0
	original := consolidationDraftFactory
	consolidationDraftFactory = func(context.Context, string, string) (consolidation.TextGenerator, string, func(), error) {
		calls++
		return staticTextGenerator{reply: reply}, "draft-model", func() {}, nil
	}
	t.Cleanup(func() {
		consolidationDraftFactory = original
		contextWorkspace = ""
	})
	return &calls
}

func consolidationDraftFixture(t *testing.T) (workspace, search string) {
	t.Helper()
	workspace, search = t.TempDir(), t.TempDir()
	helperSetupTeam(t, search, "demo")
	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "src-a", "Run go test before committing.")
	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "src-b", "Run go vet before committing.")
	return workspace, search
}

func draftArgs(workspace, search string, extra ...string) []string {
	return append([]string{"context", "consolidate", "--apply-proposal", "--source", "src-a,src-b", "--draft", "--workspace", workspace, "--project", "proj1", "--team", "demo", "--team-search-path", search}, extra...)
}

const validDraftReply = `{"text":"Run go test and go vet before committing.","covered_source_ids":["src-a","src-b"]}`

func pendingProposal(t *testing.T, workspace string) (contextstore.ConsolidationProposal, contextstore.ContextItem) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	ids := []string{"src-a", "src-b"}
	sort.Strings(ids)
	proposal, found, err := repo.FindProposedConsolidation(context.Background(), "proj1", "demo", ids)
	if err != nil || !found {
		t.Fatalf("pending proposal found=%v err=%v", found, err)
	}
	candidate, err := repo.Get(context.Background(), proposal.CandidateContextItemID)
	if err != nil {
		t.Fatal(err)
	}
	return proposal, candidate
}

func TestConsolidateDraftFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "requires apply-proposal", args: []string{"context", "consolidate", "--draft", "--project", "proj1", "--team", "demo"}, want: "--draft requires --apply-proposal"},
		{name: "excludes proposal text", args: []string{"context", "consolidate", "--apply-proposal", "--draft", "--proposal-text", "x", "--source", "a,b", "--project", "proj1", "--team", "demo"}, want: "mutually exclusive"},
		{name: "requires source", args: []string{"context", "consolidate", "--apply-proposal", "--draft", "--project", "proj1", "--team", "demo"}, want: "--draft requires --source"},
		{name: "requires team", args: []string{"context", "consolidate", "--apply-proposal", "--draft", "--source", "a,b", "--project", "proj1"}, want: "--draft requires --team"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubConsolidationDrafter(t, validDraftReply)
			args := append(append([]string{}, tc.args...), "--workspace", t.TempDir())
			if _, err := helperRunConsolidateCLI(args...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if *calls != 0 {
				t.Fatalf("drafter called %d times on invalid flags", *calls)
			}
		})
	}
}

func TestConsolidateDraftPersistsProposalAndIsIdempotent(t *testing.T) {
	workspace, search := consolidationDraftFixture(t)
	calls := stubConsolidationDrafter(t, validDraftReply)
	out, err := helperRunConsolidateCLI(draftArgs(workspace, search)...)
	if err != nil || !strings.Contains(out, "status=proposed") {
		t.Fatalf("draft = %q err=%v", out, err)
	}
	proposal, candidate := pendingProposal(t, workspace)
	if candidate.Content != "Run go test and go vet before committing." || candidate.Lifecycle != contextstore.LifecycleCandidate {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.Metadata["proposal_origin"] != "model" || candidate.Metadata["draft_model"] != "draft-model" || candidate.Metadata["derived_from"] != "src-a,src-b" {
		t.Fatalf("candidate metadata = %v", candidate.Metadata)
	}
	events, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil || !strings.Contains(string(events), "memory_consolidation_proposed") || !strings.Contains(string(events), "proposal_origin") {
		t.Fatalf("event store = %s err=%v", events, err)
	}

	out, err = helperRunConsolidateCLI(draftArgs(workspace, search)...)
	if err != nil || !strings.Contains(out, "already pending") || !strings.Contains(out, proposal.ID) || *calls != 1 {
		t.Fatalf("second draft = %q err=%v calls=%d, want reuse without a model call", out, err, *calls)
	}
}

func TestConsolidateDraftValidatesBeforeModelCall(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, workspace string)
		want  string
	}{
		{name: "unsupported source", setup: func(t *testing.T, workspace string) {
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			if err = repo.MarkSuperseded(context.Background(), []string{"src-b"}, "src-a"); err != nil {
				t.Fatal(err)
			}
		}, want: "not current confirmed knowledge"},
		{name: "conflicted source", setup: func(t *testing.T, workspace string) {
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			a, _ := repo.Get(context.Background(), "src-a")
			b, _ := repo.Get(context.Background(), "src-b")
			j := contextstore.PairJudgment{ProjectID: "proj1", TeamID: "demo", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: contextstore.PairVerdictContradicts, JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
			contextstore.NormalizePairJudgment(&j)
			if _, _, err = repo.SavePairJudgment(context.Background(), j, "operator", false); err != nil {
				t.Fatal(err)
			}
		}, want: "unresolved memory conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace, search := consolidationDraftFixture(t)
			tc.setup(t, workspace)
			calls := stubConsolidationDrafter(t, validDraftReply)
			if _, err := helperRunConsolidateCLI(draftArgs(workspace, search)...); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if *calls != 0 {
				t.Fatalf("drafter called %d times before validation passed", *calls)
			}
		})
	}
}

func TestConsolidateDraftRejectsInvalidModelOutput(t *testing.T) {
	workspace, search := consolidationDraftFixture(t)
	stubConsolidationDrafter(t, `{"text":"Run go test before committing.","covered_source_ids":["src-a","src-b"]}`)
	if _, err := helperRunConsolidateCLI(draftArgs(workspace, search)...); err == nil || !strings.Contains(err.Error(), "verbatim") {
		t.Fatalf("err = %v, want verbatim-copy rejection", err)
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if _, found, err := repo.FindProposedConsolidation(context.Background(), "proj1", "demo", []string{"src-a", "src-b"}); err != nil || found {
		t.Fatalf("invalid draft persisted a proposal: found=%v err=%v", found, err)
	}
}

func TestConsolidateManualProposalRecordsOperatorOrigin(t *testing.T) {
	workspace, _ := consolidationDraftFixture(t)
	out, err := helperRunConsolidateCLI("context", "consolidate", "--apply-proposal", "--source", "src-a,src-b", "--proposal-text", "Run go test and go vet before committing.", "--workspace", workspace, "--project", "proj1", "--team", "demo")
	if err != nil || !strings.Contains(out, "status=proposed") {
		t.Fatalf("manual proposal = %q err=%v", out, err)
	}
	_, candidate := pendingProposal(t, workspace)
	if candidate.Metadata["proposal_origin"] != "operator" || candidate.Metadata["draft_model"] != "" {
		t.Fatalf("manual candidate metadata = %v", candidate.Metadata)
	}
}

func TestConsolidateDraftRejectsOversizedSources(t *testing.T) {
	workspace, search := t.TempDir(), t.TempDir()
	helperSetupTeam(t, search, "demo")
	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "src-a", strings.Repeat("alpha ", 1400))
	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "src-b", strings.Repeat("bravo ", 1400))
	calls := stubConsolidationDrafter(t, validDraftReply)
	if _, err := helperRunConsolidateCLI(draftArgs(workspace, search)...); err == nil || !strings.Contains(err.Error(), "too large for drafting") {
		t.Fatalf("err = %v, want oversized-source rejection", err)
	}
	if *calls != 0 {
		t.Fatalf("drafter called %d times for oversized sources", *calls)
	}
}
