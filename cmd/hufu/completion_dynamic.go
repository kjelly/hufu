package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/team"
)

const (
	completionQueryTimeout = 150 * time.Millisecond
	completionResultLimit  = 100
)

type completionCandidate struct {
	id, description string
}

func configureDynamicCompletions(root *cobra.Command) {
	registerPositionalCompletion(root, []string{"inspect", "run"}, completeRunIDs)
	registerPositionalCompletion(root, []string{"inspect", "evidence"}, completeRunIDs)
	registerPositionalCompletion(root, []string{"inspect", "trace"}, completeRunIDs)
	registerPositionalCompletion(root, []string{"inspect", "replay"}, completeRunIDs)
	registerPositionalCompletion(root, []string{"inspect", "task"}, completeTaskIDs)
	registerPositionalCompletion(root, []string{"inspect", "context"}, completeTaskIDs)

	for _, path := range [][]string{
		{"context", "show"}, {"context", "history"}, {"context", "confirm"},
		{"context", "reject"}, {"context", "supersede"},
	} {
		registerPositionalCompletion(root, path, completeContextIDs)
	}
	for _, name := range []string{"show", "review", "edit", "approve", "reject", "apply"} {
		registerPositionalCompletion(root, []string{"context", "promotion", name}, completePromotionIDs)
	}

	registerFlagCompletion(root, []string{"inspect"}, "branch", completeBranchIDs)
	for _, path := range [][]string{{"inspect", "overview"}, {"inspect", "task"}, {"inspect", "context"}} {
		registerFlagCompletion(root, path, "run", completeRunIDs)
	}
	for _, name := range []string{"status", "resume", "retry", "reconcile"} {
		path := []string{"session", name}
		registerFlagCompletion(root, path, "run", completeRunIDs)
		registerFlagCompletion(root, path, "branch", completeBranchIDs)
		if name == "retry" || name == "reconcile" {
			registerFlagCompletion(root, path, "task", completeTaskIDs)
		}
	}
}

func registerPositionalCompletion(root *cobra.Command, path []string, complete func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) {
	if command, _, err := root.Find(path); err == nil && command != root {
		command.ValidArgsFunction = complete
	}
}

func registerFlagCompletion(root *cobra.Command, path []string, name string, complete func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective)) {
	if command, _, err := root.Find(path); err == nil && command != root && command.Flag(name) != nil {
		_ = command.RegisterFlagCompletionFunc(name, complete)
	}
}

func completeRunIDs(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(completionContext(command), completionQueryTimeout)
	defer cancel()
	lineage, err := inspectpkg.LoadLineage(ctx, inspectpkg.InspectQuery{Workspace: completionWorkspace(command), BranchID: commandFlagValue(command, "branch")})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	candidates := make(map[string]completionCandidate)
	for _, indexed := range lineage.Events {
		if id := strings.TrimSpace(indexed.Event.RunID); id != "" {
			candidates[id] = completionCandidate{id: id, description: "run"}
		}
	}
	return renderCompletionCandidates(candidates, prefix), cobra.ShellCompDirectiveNoFileComp
}

func completeTaskIDs(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	runID := strings.TrimSpace(commandFlagValue(command, "run"))
	if runID == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(completionContext(command), completionQueryTimeout)
	defer cancel()
	lineage, err := inspectpkg.LoadLineage(ctx, inspectpkg.InspectQuery{Workspace: completionWorkspace(command), BranchID: commandFlagValue(command, "branch")})
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	candidates := make(map[string]completionCandidate)
	for _, indexed := range lineage.Events {
		event := indexed.Event
		if event.RunID == runID && strings.TrimSpace(event.TaskID) != "" {
			candidates[event.TaskID] = completionCandidate{id: event.TaskID, description: boundedCompletionDescription(event.Type)}
		}
	}
	return renderCompletionCandidates(candidates, prefix), cobra.ShellCompDirectiveNoFileComp
}

func completeBranchIDs(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	ctx, cancel := context.WithTimeout(completionContext(command), completionQueryTimeout)
	defer cancel()
	workspace := completionWorkspace(command)
	if _, err := os.Stat(filepath.Join(workspace, "session_tree.json")); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	candidates := make(map[string]completionCandidate, len(tree.Branches)+len(tree.Labels))
	for id, branch := range tree.Branches {
		description := "branch"
		if id == tree.ActiveBranch {
			description = "active branch"
		}
		candidates[id] = completionCandidate{id: id, description: description}
		if branch != nil && branch.Name != "" && branch.Name != id {
			candidates[branch.Name] = completionCandidate{id: branch.Name, description: "branch name"}
		}
	}
	for label, target := range tree.Labels {
		if _, ok := tree.Branches[target]; ok {
			candidates[label] = completionCandidate{id: label, description: "branch label"}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return renderCompletionCandidates(candidates, prefix), cobra.ShellCompDirectiveNoFileComp
}

func completeContextIDs(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	projectID := strings.TrimSpace(commandFlagValue(command, "project"))
	if projectID == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(completionContext(command), completionQueryTimeout)
	defer cancel()
	repository, err := contextstore.OpenSQLiteReadOnly(filepath.Join(completionWorkspace(command), "context.sqlite"))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer func() { _ = repository.Close() }()
	items, err := repository.ListContextMetadataForScope(ctx, contextstore.Scope{ProjectID: projectID, TeamID: commandFlagValue(command, "team")}, completionResultLimit)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	candidates := make(map[string]completionCandidate, len(items))
	for _, item := range items {
		candidates[item.ID] = completionCandidate{id: item.ID, description: boundedCompletionDescription(fmt.Sprintf("%s %s", item.Kind, item.Lifecycle))}
	}
	return renderCompletionCandidates(candidates, prefix), cobra.ShellCompDirectiveNoFileComp
}

func completePromotionIDs(command *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
	projectID := strings.TrimSpace(commandFlagValue(command, "project"))
	teamID := strings.TrimSpace(commandFlagValue(command, "team"))
	if projectID == "" || teamID == "" {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(completionContext(command), completionQueryTimeout)
	defer cancel()
	repository, err := contextstore.OpenSQLiteReadOnly(filepath.Join(completionWorkspace(command), "context.sqlite"))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer func() { _ = repository.Close() }()
	proposals, err := repository.ListPromotionMetadataForScope(ctx, projectID, teamID, completionResultLimit)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	candidates := make(map[string]completionCandidate, len(proposals))
	for _, proposal := range proposals {
		candidates[proposal.ID] = completionCandidate{id: proposal.ID, description: boundedCompletionDescription(fmt.Sprintf("%s %s", proposal.Type, proposal.Status))}
	}
	return renderCompletionCandidates(candidates, prefix), cobra.ShellCompDirectiveNoFileComp
}

func completionWorkspace(command *cobra.Command) string {
	if value := strings.TrimSpace(commandFlagValue(command, "workspace")); value != "" {
		return value
	}
	return getWorkspace()
}

func completionContext(command *cobra.Command) context.Context {
	if command != nil && command.Context() != nil {
		return command.Context()
	}
	return context.Background()
}

func commandFlagValue(command *cobra.Command, name string) string {
	if command == nil {
		return ""
	}
	if flag := command.Flag(name); flag != nil {
		return flag.Value.String()
	}
	return ""
}

func renderCompletionCandidates(candidates map[string]completionCandidate, prefix string) []string {
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	if len(ids) > completionResultLimit {
		ids = ids[:completionResultLimit]
	}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		candidate := candidates[id]
		if candidate.description == "" {
			values = append(values, id)
		} else {
			values = append(values, id+"\t"+candidate.description)
		}
	}
	return values
}

func boundedCompletionDescription(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	return operatorpkg.SafeDisplayText(value, 64)
}
