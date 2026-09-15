package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

// These tests cover the Nushell dynamic-ID completion gap: Bash/Zsh/Fish/
// PowerShell get run/task/branch/proposal completion through Cobra's
// ValidArgsFunction/RegisterFlagCompletionFunc, but Nushell's completer
// protocol needs a plain external command instead. completionHelper*IDs
// reuse the exact same complete*IDs logic already covered by
// TestDynamicCompletionIsBoundedBranchAndRunScoped and
// TestDynamicContextAndPromotionCompletionIsSharedScopeOnlyAndReadOnly, so
// these tests focus on the new plumbing: the plain-ID rendering, the
// completion-helper CLI subcommand, and the generated Nushell script.

func TestCompletionHelperRunTaskBranchIDsAreBoundedAndReadOnly(t *testing.T) {
	workspace := t.TempDir()
	tree := team.NewSessionTree()
	tree.Branches["feature"] = &team.SessionBranch{ID: "feature", Name: "feature", CreatedAt: "2026-09-14T00:00:00Z"}
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	store, err := team.NewEventStore(workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	store.SetBranchID("main")
	for index := range 5 {
		runID := fmt.Sprintf("run-%03d", index)
		taskID := fmt.Sprintf("task-%03d", index)
		if _, err = store.AppendPersisted(team.RunEvent{RunID: runID, SessionID: "session", TaskID: taskID, Type: "task_created", Actor: "coordinator", Payload: []byte(fmt.Sprintf(`{"id":%q,"status":"pending"}`, taskID))}); err != nil {
			t.Fatal(err)
		}
	}
	store.SetBranchID("feature")
	if _, err = store.AppendPersisted(team.RunEvent{RunID: "run-feature", SessionID: "session-feature", TaskID: "task-feature", Type: "task_created", Actor: "coordinator", Payload: []byte(`{"id":"task-feature","status":"pending"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	runs := completionHelperRunIDs(workspace, "")
	if len(runs) != 5 || runs[0] != "run-000" || runs[4] != "run-004" {
		t.Fatalf("completionHelperRunIDs main branch = %v", runs)
	}
	for _, id := range runs {
		if strings.Contains(id, "\t") {
			t.Fatalf("completionHelperRunIDs leaked a description suffix: %q", id)
		}
	}

	featureRuns := completionHelperRunIDs(workspace, "feature")
	if len(featureRuns) != 1 || featureRuns[0] != "run-feature" {
		t.Fatalf("completionHelperRunIDs feature branch = %v", featureRuns)
	}

	tasks := completionHelperTaskIDs(workspace, "run-002", "")
	if len(tasks) != 1 || tasks[0] != "task-002" {
		t.Fatalf("completionHelperTaskIDs run scope = %v", tasks)
	}
	if crossed := completionHelperTaskIDs(workspace, "run-feature", ""); len(crossed) != 0 {
		t.Fatalf("completionHelperTaskIDs crossed branch scope: %v", crossed)
	}
	if scoped := completionHelperTaskIDs(workspace, "run-feature", "feature"); len(scoped) != 1 || scoped[0] != "task-feature" {
		t.Fatalf("completionHelperTaskIDs explicit feature branch = %v", scoped)
	}

	branches := completionHelperBranchIDs(workspace)
	found := false
	for _, id := range branches {
		if id == "feature" {
			found = true
		}
		if strings.Contains(id, "\t") {
			t.Fatalf("completionHelperBranchIDs leaked a description suffix: %q", id)
		}
	}
	if !found {
		t.Fatalf("completionHelperBranchIDs missing feature branch: %v", branches)
	}

	after, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("completion-helper IDs mutated the event store")
	}
}

func TestCompletionHelperProposalIDsAreScopedAndReadOnly(t *testing.T) {
	workspace := t.TempDir()
	database := filepath.Join(workspace, "context.sqlite")
	repository, err := contextstore.OpenSQLite(database)
	if err != nil {
		t.Fatal(err)
	}
	draft := "## Policy\n- Safe."
	proposal := contextstore.PromotionProposal{ProjectID: "project", TeamID: "team", Type: contextstore.PromotionTypeTeamPolicy, TargetPath: "coordinator.md", Draft: draft, DraftHash: contextstore.HashPromotionContent(draft), PolicyVersion: "policy", Status: contextstore.PromotionStatusProposed, Sources: []contextstore.PromotionSourceSnapshot{{ContextItemID: "ctx-shared", ContentHash: "hash", AggregateRevision: 1}}}
	proposal.ID = contextstore.PromotionProposalID(proposal)
	if _, _, err = repository.CreatePromotion(t.Context(), proposal, contextstore.PromotionOutboxEvent{IdempotencyKey: proposal.ID + ":proposed", EventType: "memory_promotion_proposed", Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = repository.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}

	proposals := completionHelperProposalIDs(workspace, "project", "team")
	if len(proposals) != 1 || proposals[0] != proposal.ID {
		t.Fatalf("completionHelperProposalIDs = %v, want [%s]", proposals, proposal.ID)
	}
	if strings.Contains(proposals[0], "coordinator.md") {
		t.Fatalf("completionHelperProposalIDs exposed target path: %q", proposals[0])
	}
	if other := completionHelperProposalIDs(workspace, "project", "other-team"); len(other) != 0 {
		t.Fatalf("completionHelperProposalIDs crossed team scope: %v", other)
	}
	if missing := completionHelperProposalIDs(workspace, "", ""); len(missing) != 0 {
		t.Fatalf("completionHelperProposalIDs without scope = %v, want empty", missing)
	}

	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("completion-helper proposal IDs mutated the promotion database")
	}
}

func TestCompletionHelperCmdIsHiddenAndPrintsPlainIDLines(t *testing.T) {
	root := newRootCommand()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendPersisted(team.RunEvent{RunID: "run-cli", SessionID: "session", TaskID: "task-cli", Type: "task_created", Actor: "coordinator", Payload: []byte(`{"id":"task-cli","status":"pending"}`)}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}

	command, _, err := root.Find([]string{"completion-helper"})
	if err != nil {
		t.Fatal(err)
	}
	if !command.Hidden {
		t.Fatal("completion-helper must stay Hidden from normal help output")
	}

	root.SetArgs([]string{"completion-helper", "runs", "--workspace", workspace, "--run", "", "--branch", "", "--project", "", "--team", ""})

	oldStdout := os.Stdout
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("os.Pipe failed: %v", pipeErr)
	}
	os.Stdout = w
	execErr := root.Execute()
	w.Close()
	os.Stdout = oldStdout
	if execErr != nil {
		t.Fatalf("completion-helper runs failed: %v", execErr)
	}

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	lines := strings.Fields(buf.String())
	if len(lines) != 1 || lines[0] != "run-cli" {
		t.Fatalf("completion-helper runs stdout = %q, want a single bare %q line", buf.String(), "run-cli")
	}
}

func TestNushellCompletionWiresRunTaskBranchProposalCompleters(t *testing.T) {
	var output bytes.Buffer
	if err := generateNushellCompletion(&output); err != nil {
		t.Fatal(err)
	}
	script := output.String()
	for _, expected := range []string{
		`def "nu-complete hufu runs"`,
		`def "nu-complete hufu tasks"`,
		`def "nu-complete hufu branches"`,
		`def "nu-complete hufu proposals"`,
		`run_id: string@'nu-complete hufu runs'`,
		`task_id: string@'nu-complete hufu tasks'`,
		`--branch: string@'nu-complete hufu branches'`,
		`proposal_id: string@'nu-complete hufu proposals'`,
		`proposal_id?: string@'nu-complete hufu proposals'`,
		`^hufu completion-helper`,
	} {
		if !strings.Contains(script, expected) {
			t.Fatalf("Nushell completion does not contain %q", expected)
		}
	}
	// Missing project/team must short-circuit before ever invoking hufu, the
	// same no-cross-scope-guess rule completeContextIDs/completePromotionIDs
	// already enforce for the other shells.
	if !strings.Contains(script, `if $project == "" or $team == "" { return [] }`) {
		t.Fatal("Nushell proposals completer must return early without a project/team scope")
	}
}
