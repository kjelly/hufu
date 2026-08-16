package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/promotion"
)

type promotionGeneratorFunc func(context.Context, promotion.DraftRequest) (promotion.DraftResult, error)

func (f promotionGeneratorFunc) Generate(ctx context.Context, request promotion.DraftRequest) (promotion.DraftResult, error) {
	return f(ctx, request)
}

func helperSetupTeam(t *testing.T, searchDir, teamName string) string {
	t.Helper()
	teamDir := filepath.Join(searchDir, teamName)
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	coord := "---\nname: coordinator\nrole: coordinator\ntools: ask_user\n---\nCoordinate tasks."
	if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte(coord), 0o644); err != nil {
		t.Fatal(err)
	}
	worker := "---\nname: worker\nrole: worker\ntools: view,ls\n---\nWorker task execution."
	if err := os.WriteFile(filepath.Join(teamDir, "worker.md"), []byte(worker), 0o644); err != nil {
		t.Fatal(err)
	}
	return teamDir
}

func helperSeedEligibleLTM(t *testing.T, workspace, projectID, teamID, itemID, content string) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	ctx := context.Background()
	item := contextstore.ContextItem{
		ID:        itemID,
		Kind:      contextstore.ContextDecision,
		Content:   content,
		Scope:     contextstore.Scope{ProjectID: projectID, TeamID: teamID},
		Lifecycle: contextstore.LifecycleConfirmed,
		Metadata:  map[string]string{"memory_lifetime": "persistent"},
	}
	if err := repo.Append(ctx, item); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		_, err := repo.ApplyExperienceObservation(ctx, contextstore.ExperienceObservation{
			IdempotencyKey:       itemID + string(rune('0'+i)),
			ContextItemID:        itemID,
			PolicyVersion:        "memory-policy-v1",
			ProjectID:            projectID,
			TaskID:               "task-" + string(rune('0'+i)),
			AppliedDelta:         1,
			VerifiedSupportDelta: 1,
			PositiveWeight:       1,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func helperCreateProposalInRepo(t *testing.T, workspace, searchDir, projectID, teamID, targetPath, draft string) string {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	ctx := context.Background()
	var sources []contextstore.PromotionSourceSnapshot
	srcItem, err := repo.Get(ctx, "source-1")
	if err == nil {
		agg, _ := repo.ExperienceAggregate(ctx, "source-1", "memory-policy-v1")
		sources = append(sources, contextstore.PromotionSourceSnapshot{
			ContextItemID:     srcItem.ID,
			ContentHash:       srcItem.ContentHash,
			AggregateRevision: agg.Revision,
		})
	} else {
		sources = append(sources, contextstore.PromotionSourceSnapshot{
			ContextItemID:     "source-1",
			ContentHash:       "hash-1",
			AggregateRevision: 2,
		})
	}

	targetBaseHash := ""
	if targetBytes, readErr := os.ReadFile(filepath.Join(searchDir, teamID, targetPath)); readErr == nil {
		targetBaseHash = contextstore.HashPromotionContent(string(targetBytes))
	}

	p := contextstore.PromotionProposal{
		ProjectID:      projectID,
		TeamID:         teamID,
		Type:           contextstore.PromotionTypeTeamPolicy,
		TargetPath:     targetPath,
		TargetBaseHash: targetBaseHash,
		Draft:          draft,
		DraftHash:      contextstore.HashPromotionContent(draft),
		PolicyVersion:  "memory-policy-v1",
		Sources:        sources,
		Status:         contextstore.PromotionStatusProposed,
	}
	p.ID = contextstore.PromotionProposalID(p)
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": p.ID})
	_, _, err = repo.CreatePromotion(ctx, p, contextstore.PromotionOutboxEvent{
		IdempotencyKey: p.ID + ":proposed",
		EventType:      "memory_promotion_proposed",
		Payload:        payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func helperRunCLI(args ...string) (string, error) {
	promotionWorkspace = ""
	promotionProject = ""
	promotionTeam = ""
	promotionSearchPath = ""
	promotionPolicyVersion = "memory-policy-v1"
	promotionType = ""
	promotionAgent = ""
	promotionModel = ""
	promotionDraftFile = ""
	promotionRejectReason = ""
	promotionJSON = false
	ltmPromotionDryRun = false
	promotionShowContent = false

	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestRunPromotionAnalyzeReleasesGeneratorAfterSuccess(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	original := promotionGeneratorFactory
	var releaseCalls int
	promotionGeneratorFactory = func(context.Context, string) (promotion.DraftGenerator, func(), error) {
		return promotionGeneratorFunc(func(context.Context, promotion.DraftRequest) (promotion.DraftResult, error) {
			return promotion.DraftResult{}, errors.New("generator should not run without eligible context")
		}), func() { releaseCalls++ }, nil
	}
	t.Cleanup(func() { promotionGeneratorFactory = original })

	if _, err := helperRunCLI("context", "promotion", "analyze",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search); err != nil {
		t.Fatalf("analyze error: %v", err)
	}
	if releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", releaseCalls)
	}
}

func TestRunPromotionAnalyzeReleasesGeneratorAfterFailure(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")
	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "source-1", "verified promotion source")

	want := errors.New("generator failure")
	original := promotionGeneratorFactory
	var releaseCalls int
	promotionGeneratorFactory = func(context.Context, string) (promotion.DraftGenerator, func(), error) {
		return promotionGeneratorFunc(func(context.Context, promotion.DraftRequest) (promotion.DraftResult, error) {
			return promotion.DraftResult{}, want
		}), func() { releaseCalls++ }, nil
	}
	t.Cleanup(func() { promotionGeneratorFactory = original })

	_, err := helperRunCLI("context", "promotion", "analyze",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want generator failure", err)
	}
	if releaseCalls != 1 {
		t.Fatalf("release calls = %d, want 1", releaseCalls)
	}
}

func TestCLIAcceptanceCase1_NoSuitableLTM(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	// Text mode
	out, err := helperRunCLI("context", "promotion", "analyze",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--dry-run")
	if err != nil {
		t.Fatalf("analyze dry-run error: %v", err)
	}
	if !strings.Contains(out, "No suitable LTM entries found for promotion.") {
		t.Fatalf("expected message, got: %q", out)
	}

	// JSON mode
	outJSON, err := helperRunCLI("context", "promotion", "analyze",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--dry-run",
		"--json")
	if err != nil {
		t.Fatalf("analyze dry-run json error: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(outJSON), &data); err != nil {
		t.Fatalf("invalid JSON output: %v\n%s", err, outJSON)
	}
	if msg, ok := data["message"].(string); !ok || !strings.Contains(msg, "No suitable LTM entries found for promotion.") {
		t.Fatalf("unexpected json payload: %v", data)
	}
}

func TestCLIAcceptanceCase2_ProposeAndList(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	draft := "## Promoted Policy\nAlways run tests before submission."
	id := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "coordinator.md", draft)

	// List in text mode
	outText, err := helperRunCLI("context", "promotion", "list",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("list text error: %v", err)
	}
	if !strings.Contains(outText, id) || !strings.Contains(outText, "proposed") || !strings.Contains(outText, "coordinator.md") {
		t.Fatalf("unexpected list text output: %s", outText)
	}
	// Verify draft content is not leaked in list
	if strings.Contains(outText, "Always run tests before submission") {
		t.Fatalf("list text leaked draft content: %s", outText)
	}

	// List in JSON mode
	outJSON, err := helperRunCLI("context", "promotion", "list",
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--json")
	if err != nil {
		t.Fatalf("list JSON error: %v", err)
	}
	var listPayload struct {
		Proposals []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			TargetPath string `json:"target_path"`
			Draft      string `json:"draft"`
		} `json:"proposals"`
	}
	if err := json.Unmarshal([]byte(outJSON), &listPayload); err != nil {
		t.Fatalf("invalid JSON from list: %v\n%s", err, outJSON)
	}
	if len(listPayload.Proposals) != 1 || listPayload.Proposals[0].ID != id {
		t.Fatalf("unexpected json proposals: %#v", listPayload)
	}
	if listPayload.Proposals[0].Draft != "" {
		t.Fatalf("list JSON leaked draft: %s", outJSON)
	}

	// Verify team Markdown files remain completely untouched
	coordContent, _ := os.ReadFile(filepath.Join(search, "demo", "coordinator.md"))
	if strings.Contains(string(coordContent), "hufu-promotion") {
		t.Fatal("coordinator.md was unexpectedly modified by list/propose")
	}
}

func TestCLIAcceptanceCase3_ShowProposal(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "source-1", "Decision content for show verification")
	draft := "## Promoted Policy\nVerified show policy draft."
	id := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "coordinator.md", draft)

	// 1. Show without --show-content (compact metadata) in text
	outShow, err := helperRunCLI("context", "promotion", "show", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("show error: %v", err)
	}
	if !strings.Contains(outShow, id) || !strings.Contains(outShow, "status: proposed") || !strings.Contains(outShow, "target: coordinator.md") {
		t.Fatalf("unexpected show output: %s", outShow)
	}
	if strings.Contains(outShow, "Verified show policy draft") {
		t.Fatalf("compact show leaked draft: %s", outShow)
	}

	// 2. Show with --show-content in text
	outShowContent, err := helperRunCLI("context", "promotion", "show", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--show-content")
	if err != nil {
		t.Fatalf("show --show-content error: %v", err)
	}
	if !strings.Contains(outShowContent, "draft:") || !strings.Contains(outShowContent, "Verified show policy draft") {
		t.Fatalf("show --show-content missing draft: %s", outShowContent)
	}
	if !strings.Contains(outShowContent, "source source-1") {
		t.Fatalf("show --show-content missing source summary: %s", outShowContent)
	}

	// 3. Show with --show-content in JSON
	outJSONContent, err := helperRunCLI("context", "promotion", "show", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--show-content",
		"--json")
	if err != nil {
		t.Fatalf("show --show-content --json error: %v", err)
	}
	var showPayload struct {
		Proposal struct {
			ID              string `json:"id"`
			Draft           string `json:"draft"`
			SourceSummaries []struct {
				ID      string `json:"id"`
				Summary string `json:"summary"`
			} `json:"source_summaries"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal([]byte(outJSONContent), &showPayload); err != nil {
		t.Fatalf("invalid JSON from show: %v\n%s", err, outJSONContent)
	}
	if showPayload.Proposal.ID != id || !strings.Contains(showPayload.Proposal.Draft, "Verified show policy draft") {
		t.Fatalf("unexpected proposal payload: %#v", showPayload)
	}
	if len(showPayload.Proposal.SourceSummaries) != 1 || showPayload.Proposal.SourceSummaries[0].ID != "source-1" {
		t.Fatalf("unexpected source summaries: %#v", showPayload.Proposal.SourceSummaries)
	}
}

func TestCLIAcceptanceCase4_EditAndReject(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "source-1", "Original source decision")
	initialDraft := "## Initial Draft\nInitial policy draft."
	id := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "coordinator.md", initialDraft)

	// 1. Edit the proposal draft via file
	draftFile := filepath.Join(t.TempDir(), "new_draft.md")
	newDraft := "## Edited Policy\nUpdated policy draft with refined steps."
	if err := os.WriteFile(draftFile, []byte(newDraft), 0o644); err != nil {
		t.Fatal(err)
	}

	outEdit, err := helperRunCLI("context", "promotion", "edit", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--draft-file", draftFile)
	if err != nil {
		t.Fatalf("edit error: %v", err)
	}
	if !strings.Contains(outEdit, id) || !strings.Contains(outEdit, "proposed") {
		t.Fatalf("unexpected edit output: %s", outEdit)
	}

	// Verify draft was updated in show
	outShowAfterEdit, err := helperRunCLI("context", "promotion", "show", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--show-content")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outShowAfterEdit, "Updated policy draft with refined steps") {
		t.Fatalf("draft was not updated after edit: %s", outShowAfterEdit)
	}

	// 2. Reject the proposal
	outReject, err := helperRunCLI("context", "promotion", "reject", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search,
		"--reason", "Does not meet architectural conventions")
	if err != nil {
		t.Fatalf("reject error: %v", err)
	}
	if !strings.Contains(outReject, id) || !strings.Contains(outReject, "rejected") {
		t.Fatalf("unexpected reject output: %s", outReject)
	}

	// Verify proposal status is rejected
	outShowAfterReject, err := helperRunCLI("context", "promotion", "show", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(outShowAfterReject, "status: rejected") {
		t.Fatalf("status not rejected: %s", outShowAfterReject)
	}

	// Verify target file is untouched
	coordContent, _ := os.ReadFile(filepath.Join(search, "demo", "coordinator.md"))
	if strings.Contains(string(coordContent), "hufu-promotion") {
		t.Fatal("coordinator.md was modified after reject")
	}

	// Verify source LTM item is still confirmed
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	item, err := repo.Get(context.Background(), "source-1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Lifecycle != contextstore.LifecycleConfirmed {
		t.Fatalf("source item lifecycle changed: %s", item.Lifecycle)
	}
}

func TestCLIAcceptanceCase5_ApproveAndApply(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "source-1", "Crucial operational convention")
	draft := "## Promoted Policy: Safety\nAlways acquire lock before mutation."
	id := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "coordinator.md", draft)

	// 1. Attempt apply before approve (must fail)
	_, err := helperRunCLI("context", "promotion", "apply", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err == nil || (!strings.Contains(err.Error(), "must be approved") && !strings.Contains(err.Error(), "approval is required")) {
		t.Fatalf("expected apply error before approve, got: %v", err)
	}

	// 2. Approve the proposal
	outApprove, err := helperRunCLI("context", "promotion", "approve", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("approve error: %v", err)
	}
	if !strings.Contains(outApprove, id) || !strings.Contains(outApprove, "approved") {
		t.Fatalf("unexpected approve output: %s", outApprove)
	}

	// Target file must still be unmodified after approve alone
	coordBeforeApply, _ := os.ReadFile(filepath.Join(search, "demo", "coordinator.md"))
	if strings.Contains(string(coordBeforeApply), "hufu-promotion") {
		t.Fatal("target file modified after approve before apply")
	}

	// 3. Apply the approved proposal
	outApply, err := helperRunCLI("context", "promotion", "apply", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("apply error: %v", err)
	}
	if !strings.Contains(outApply, id) || !strings.Contains(outApply, "applied") {
		t.Fatalf("unexpected apply output: %s", outApply)
	}

	// Target file must now contain the promotion marker and draft
	coordAfterApply, _ := os.ReadFile(filepath.Join(search, "demo", "coordinator.md"))
	if !strings.Contains(string(coordAfterApply), "<!-- hufu-promotion:"+id+":start -->") {
		t.Fatalf("target missing promotion marker: %s", coordAfterApply)
	}
	if !strings.Contains(string(coordAfterApply), "Always acquire lock before mutation.") {
		t.Fatalf("target missing draft content: %s", coordAfterApply)
	}

	// Audit event store must contain memory_promotion_applied
	events, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatalf("read event_store: %v", err)
	}
	if !strings.Contains(string(events), "memory_promotion_applied") {
		t.Fatalf("missing memory_promotion_applied event in audit log: %s", events)
	}
}

func TestCLIAcceptanceCase6_IdempotentReapplyAndStale(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	helperSetupTeam(t, search, "demo")

	helperSeedEligibleLTM(t, workspace, "proj1", "demo", "source-1", "Stale and reapply testing")
	draft := "## Policy\nStandard operational procedure."
	id := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "coordinator.md", draft)

	// Approve & Apply
	_, _ = helperRunCLI("context", "promotion", "approve", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	outApply1, err := helperRunCLI("context", "promotion", "apply", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("apply 1: %v", err)
	}
	if !strings.Contains(outApply1, "applied") {
		t.Fatalf("apply 1 output: %s", outApply1)
	}

	// Re-apply (idempotent)
	outReapply, err := helperRunCLI("context", "promotion", "apply", id,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("reapply error: %v", err)
	}
	if !strings.Contains(outReapply, "already applied") {
		t.Fatalf("expected already applied, got: %s", outReapply)
	}

	// Verify no duplicated promotion blocks in file
	content, _ := os.ReadFile(filepath.Join(search, "demo", "coordinator.md"))
	if strings.Count(string(content), "hufu-promotion:"+id+":start") != 1 {
		t.Fatalf("duplicate block detected after re-apply: %s", content)
	}

	// Stale evidence test on a second proposal
	id2 := helperCreateProposalInRepo(t, workspace, search, "proj1", "demo", "worker.md", "## Worker Policy\nDo not drift.")
	_, _ = helperRunCLI("context", "promotion", "approve", id2,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)

	// Mutate source LTM observation to invalidate snapshot revision
	repo, _ := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	_, _ = repo.ApplyExperienceObservation(context.Background(), contextstore.ExperienceObservation{
		IdempotencyKey:       "source-1-mutated",
		ContextItemID:        "source-1",
		PolicyVersion:        "memory-policy-v1",
		ProjectID:            "proj1",
		TaskID:               "task-extra",
		AppliedDelta:         1,
		VerifiedSupportDelta: 1,
	})
	_ = repo.Close()

	// Apply should detect stale source and fail
	_, err = helperRunCLI("context", "promotion", "apply", id2,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("expected stale error on apply, got: %v", err)
	}

	// Verify proposal status is updated to stale
	outShowStale, _ := helperRunCLI("context", "promotion", "show", id2,
		"--workspace", workspace,
		"--project", "proj1",
		"--team", "demo",
		"--team-search-path", search)
	if !strings.Contains(outShowStale, "status: stale") {
		t.Fatalf("expected proposal status stale, got: %s", outShowStale)
	}
}

func TestContextPromotionNoEligibleAndDefaultOutputIsContentFree(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	teamDir := filepath.Join(search, "demo")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte("---\nname: coordinator\nrole: coordinator\n---\nCoordinate."), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := helperRunCLI("context", "promotion", "analyze", "--workspace", workspace, "--project", "p", "--team", "demo", "--team-search-path", search, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No suitable LTM entries found for promotion.") {
		t.Fatalf("output=%q", out)
	}

	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	draft := "## Promoted policy\nDistinctive draft material must stay hidden."
	p := contextstore.PromotionProposal{
		ProjectID:      "p",
		TeamID:         "demo",
		Type:           contextstore.PromotionTypeTeamPolicy,
		TargetPath:     "coordinator.md",
		TargetBaseHash: contextstore.HashPromotionContent("---\nname: coordinator\nrole: coordinator\n---\nCoordinate."),
		Draft:          draft,
		DraftHash:      contextstore.HashPromotionContent(draft),
		PolicyVersion:  "memory-policy-v1",
		Sources:        []contextstore.PromotionSourceSnapshot{{ContextItemID: "source-id", ContentHash: "source-hash", AggregateRevision: 2}},
		Status:         contextstore.PromotionStatusProposed,
	}
	p.ID = contextstore.PromotionProposalID(p)
	payload, _ := json.Marshal(map[string]any{"schema_version": 1, "proposal_id": p.ID})
	if _, _, err = repo.CreatePromotion(t.Context(), p, contextstore.PromotionOutboxEvent{IdempotencyKey: p.ID + ":proposed", EventType: "memory_promotion_proposed", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}

	outList, err := helperRunCLI("context", "promotion", "list", "--workspace", workspace, "--project", "p", "--team", "demo", "--team-search-path", search, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(outList, "Distinctive draft material") {
		t.Fatalf("default JSON leaked draft: %s", outList)
	}

	events, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(events), "Distinctive draft material") {
		t.Fatal("audit event leaked draft")
	}
	if !strings.Contains(string(events), "memory_promotion_proposed") {
		t.Fatalf("missing audit event: %s", events)
	}
}

func TestCLIAcceptanceTemplatedTeam(t *testing.T) {
	workspace := t.TempDir()
	search := t.TempDir()
	teamDir := filepath.Join(search, "tmpl-team")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	teamYAML := `name: tmpl-team
vars:
  env: production
  app: backend
`
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte(teamYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	coord := "---\nname: coordinator\nrole: coordinator\ntools: ask_user\n---\nCoordinator for {@ .app @} in {@ .env @}."
	if err := os.WriteFile(filepath.Join(teamDir, "coordinator.md"), []byte(coord), 0o644); err != nil {
		t.Fatal(err)
	}

	helperSeedEligibleLTM(t, workspace, "proj-tmpl", "tmpl-team", "source-1", "Deploy safely")
	draft := "## Policy\nAlways check readiness probe."
	id := helperCreateProposalInRepo(t, workspace, search, "proj-tmpl", "tmpl-team", "coordinator.md", draft)

	// Approve via CLI
	outApprove, err := helperRunCLI("context", "promotion", "approve", id,
		"--workspace", workspace,
		"--project", "proj-tmpl",
		"--team", "tmpl-team",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if !strings.Contains(outApprove, "approved") {
		t.Fatalf("expected approved, got: %s", outApprove)
	}

	// Apply via CLI
	outApply, err := helperRunCLI("context", "promotion", "apply", id,
		"--workspace", workspace,
		"--project", "proj-tmpl",
		"--team", "tmpl-team",
		"--team-search-path", search)
	if err != nil {
		t.Fatalf("apply failed on templated team: %v", err)
	}
	if !strings.Contains(outApply, "applied") {
		t.Fatalf("expected applied, got: %s", outApply)
	}

	// Target file must contain promotion marker
	appliedContent, err := os.ReadFile(filepath.Join(teamDir, "coordinator.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(appliedContent), "hufu-promotion:"+id+":start") {
		t.Fatalf("missing promotion block: %s", appliedContent)
	}
	if !strings.Contains(string(appliedContent), "Coordinator for {@ .app @} in {@ .env @}.") {
		t.Fatalf("templated content corrupted: %s", appliedContent)
	}
}
