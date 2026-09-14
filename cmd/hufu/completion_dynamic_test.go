package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func TestDynamicCompletionIsBoundedBranchAndRunScoped(t *testing.T) {
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
	for index := range 120 {
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

	command := completionTestCommand(workspace)
	runs, directive := completeRunIDs(command, nil, "run-")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(runs) != completionResultLimit {
		t.Fatalf("run completion: count=%d directive=%v", len(runs), directive)
	}
	if runs[0] != "run-000\trun" || runs[len(runs)-1] != "run-099\trun" {
		t.Fatalf("run completion is not canonical and sorted: first=%q last=%q", runs[0], runs[len(runs)-1])
	}
	_ = command.Flags().Set("run", "run-042")
	tasks, directive := completeTaskIDs(command, nil, "task-")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(tasks) != 1 || !strings.HasPrefix(tasks[0], "task-042\t") {
		t.Fatalf("task completion crossed run scope: %v (%v)", tasks, directive)
	}
	_ = command.Flags().Set("run", "run-feature")
	if siblingTasks, _ := completeTaskIDs(command, nil, "task-"); len(siblingTasks) != 0 {
		t.Fatalf("task completion crossed branch scope: %v", siblingTasks)
	}
	_ = command.Flags().Set("branch", "feature")
	featureTasks, _ := completeTaskIDs(command, nil, "task-")
	if len(featureTasks) != 1 || !strings.HasPrefix(featureTasks[0], "task-feature\t") {
		t.Fatalf("explicit feature branch completion = %v", featureTasks)
	}
	after, err := os.ReadFile(filepath.Join(workspace, "logs", "event_store.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("run/task completion mutated the event store")
	}
}

func TestDynamicContextAndPromotionCompletionIsSharedScopeOnlyAndReadOnly(t *testing.T) {
	workspace := t.TempDir()
	database := filepath.Join(workspace, "context.sqlite")
	repository, err := contextstore.OpenSQLite(database)
	if err != nil {
		t.Fatal(err)
	}
	items := []contextstore.ContextItem{
		{ID: "ctx-shared", Kind: contextstore.ContextPattern, Content: "shared secret body", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team"}, Authority: contextstore.AuthorityTool, TrustLevel: contextstore.TrustInternal},
		{ID: "ctx-private", Kind: contextstore.ContextPattern, Content: "private", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team", AgentID: "worker"}, Authority: contextstore.AuthorityTool, TrustLevel: contextstore.TrustInternal},
		{ID: "ctx-branch", Kind: contextstore.ContextPattern, Content: "branch", Scope: contextstore.Scope{ProjectID: "project", TeamID: "team", BranchID: "other-branch"}, Authority: contextstore.AuthorityTool, TrustLevel: contextstore.TrustInternal},
		{ID: "ctx-other", Kind: contextstore.ContextPattern, Content: "other", Scope: contextstore.Scope{ProjectID: "project", TeamID: "other"}, Authority: contextstore.AuthorityTool, TrustLevel: contextstore.TrustInternal},
	}
	if err = repository.Append(t.Context(), items...); err != nil {
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
	beforeNames := completionDirectoryNames(t, workspace)

	command := completionTestCommand(workspace)
	command.Flags().String("project", "project", "")
	command.Flags().String("team", "team", "")
	contexts, directive := completeContextIDs(command, nil, "ctx-")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(contexts) != 1 || !strings.HasPrefix(contexts[0], "ctx-shared\t") {
		t.Fatalf("context completion leaked another scope: %v (%v)", contexts, directive)
	}
	proposals, directive := completePromotionIDs(command, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp || len(proposals) != 1 || !strings.HasPrefix(proposals[0], proposal.ID+"\t") {
		t.Fatalf("promotion completion mismatch: %v (%v)", proposals, directive)
	}
	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !slices.Equal(beforeNames, completionDirectoryNames(t, workspace)) {
		t.Fatal("context/proposal completion mutated the workspace")
	}
	for _, value := range append(contexts, proposals...) {
		if strings.Contains(value, "secret") || strings.Contains(value, "coordinator.md") {
			t.Fatalf("completion exposed content or path: %q", value)
		}
	}
}

func TestDynamicCompletionMissingScopeOrStoreIsSilent(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "absent")
	command := completionTestCommand(workspace)
	command.Flags().String("project", "", "")
	command.Flags().String("team", "", "")
	for _, complete := range []func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective){completeRunIDs, completeTaskIDs, completeContextIDs, completePromotionIDs} {
		values, directive := complete(command, nil, "")
		if len(values) != 0 || directive != cobra.ShellCompDirectiveNoFileComp {
			t.Fatalf("missing completion result = %v, %v", values, directive)
		}
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("completion created an absent workspace: %v", err)
	}
}

func TestDynamicCompletionIsRegisteredOnCanonicalCommands(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{
		{"inspect", "run"}, {"inspect", "task"}, {"inspect", "context"},
		{"context", "show"}, {"context", "promotion", "review"},
	} {
		command, _, err := root.Find(path)
		if err != nil || command.ValidArgsFunction == nil {
			t.Fatalf("%v positional completion is not registered: %v", path, err)
		}
	}
	for _, tc := range []struct {
		path []string
		flag string
	}{
		{[]string{"inspect"}, "branch"},
		{[]string{"inspect", "overview"}, "run"},
		{[]string{"session", "retry"}, "run"},
		{[]string{"session", "retry"}, "branch"},
		{[]string{"session", "retry"}, "task"},
	} {
		command, _, err := root.Find(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := command.GetFlagCompletionFunc(tc.flag); !ok {
			t.Fatalf("%v --%s completion is not registered", tc.path, tc.flag)
		}
	}
}

func completionTestCommand(workspace string) *cobra.Command {
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.Flags().String("workspace", workspace, "")
	command.Flags().String("branch", "", "")
	command.Flags().String("run", "", "")
	return command
}

func completionDirectoryNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}
