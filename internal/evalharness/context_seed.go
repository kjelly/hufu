package evalharness

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

var contextFixtureKinds = []string{
	"decision", "progress", "open_question", "tool_call", "tool_result", "error",
	"verification", "artifact", "convention", "architecture", "pattern", "summary", "observation",
}

// validateFixtureBranch proves the seed alias against a checked event lineage.
// Names are resolved to canonical branch IDs before writing a context row.
func validateFixtureBranch(workspace string, fixture ContextItemFixture) (string, error) {
	if _, err := os.Stat(filepath.Join(workspace, "session_tree.json")); err != nil {
		return "", fmt.Errorf("context item %q requires an explicit session tree", fixture.ID)
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return "", err
	}
	branch := tree.GetBranch(fixture.Branch)
	if branch == nil {
		return "", fmt.Errorf("context item %q refers to an unseeded branch", fixture.ID)
	}
	store, err := team.OpenEventStoreReadOnly(workspace)
	if err != nil {
		return "", err
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return "", err
	}
	visible, err := team.ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
	if err != nil {
		return "", err
	}
	hasItem := func(events []team.RunEvent) bool {
		return slices.ContainsFunc(events, func(event team.RunEvent) bool {
			if event.BranchID != branch.ID || (event.Type != "worker_memory_confirmed" && event.Type != "worker_memory_candidate_saved") {
				return false
			}
			var payload struct {
				ItemID string `json:"item_id"`
			}
			return json.Unmarshal(event.Payload, &payload) == nil && payload.ItemID == fixture.ID
		})
	}
	ancestor := false
	for current := tree.GetBranch(tree.ActiveBranch); current != nil && current.ParentID != ""; {
		current = tree.GetBranch(current.ParentID)
		if current != nil && current.ID == branch.ID {
			ancestor = true
			break
		}
	}
	valid := hasItem(events)
	switch fixture.Scope {
	case "active_branch":
		valid = valid && branch.ID == tree.ActiveBranch && hasItem(visible)
	case "ancestor_before_fork":
		valid = valid && ancestor && hasItem(visible)
	case "ancestor_after_fork":
		valid = valid && ancestor && !hasItem(visible)
	case "sibling_branch":
		valid = valid && !ancestor && branch.ID != tree.ActiveBranch && !hasItem(visible)
	default:
		return "", fmt.Errorf("context item %q cannot attach a branch to scope %q", fixture.ID, fixture.Scope)
	}
	if !valid {
		return "", fmt.Errorf("context item %q scope does not match its durable fork lineage", fixture.ID)
	}
	return branch.ID, nil
}

var contextFixtureScopes = []string{
	"same_team_shared", "same_worker_private", "other_worker", "other_team",
	"active_branch", "ancestor_before_fork", "ancestor_after_fork", "sibling_branch",
}

func activationFixtureMetadata(activation *ContextActivationFixture) map[string]string {
	if activation == nil {
		return nil
	}
	metadata := make(map[string]string)
	for key, values := range map[string][]string{
		"phases": activation.Phases, "triggers": activation.Triggers, "roles": activation.Roles,
		"capabilities": activation.Capabilities, "tools": activation.Tools,
		"error_classes": activation.ErrorClasses, "environment": activation.Environment,
	} {
		if len(values) > 0 {
			metadata["activation."+key] = strings.Join(values, ",")
		}
	}
	return metadata
}

func validateContextFixtures(items []ContextItemFixture) error {
	ids := make(map[string]bool, len(items))
	for index, item := range items {
		if strings.TrimSpace(item.ID) == "" || strings.ContainsAny(item.ID, " \t\n\r<>") || ids[item.ID] || strings.TrimSpace(item.Content) == "" {
			return fmt.Errorf("context-items[%d] requires a unique id and non-empty content", index)
		}
		ids[item.ID] = true
		if err := validateContextFixture(item); err != nil {
			return fmt.Errorf("context-items[%d]: %w", index, err)
		}
	}
	for _, item := range items {
		if item.SupersededBy != "" && (!ids[item.SupersededBy] || item.SupersededBy == item.ID) {
			return fmt.Errorf("context item %q superseded-by must name a different seeded item", item.ID)
		}
	}
	return nil
}

func validateContextFixture(item ContextItemFixture) error {
	if item.Kind != "" && !slices.Contains(contextFixtureKinds, item.Kind) {
		return fmt.Errorf("unsupported kind %q", item.Kind)
	}
	if item.Lifecycle != "" && !slices.Contains([]string{"candidate", "confirmed", "rejected"}, item.Lifecycle) {
		return fmt.Errorf("unsupported lifecycle %q", item.Lifecycle)
	}
	if item.Scope != "" && !slices.Contains(contextFixtureScopes, item.Scope) {
		return fmt.Errorf("unsupported scope %q", item.Scope)
	}
	if item.Priority != nil && (*item.Priority < 0 || *item.Priority > 100) {
		return fmt.Errorf("priority must be in [0,100]")
	}
	if item.Confidence != nil && (math.IsNaN(*item.Confidence) || math.IsInf(*item.Confidence, 0) || *item.Confidence < 0 || *item.Confidence > 1) {
		return fmt.Errorf("confidence must be in [0,1]")
	}
	if item.ValidFrom != nil && item.ValidUntil != nil && !item.ValidFrom.Before(*item.ValidUntil) {
		return fmt.Errorf("valid-from must precede valid-until")
	}
	if item.ValidFrom != nil && item.ExpiresAt != nil && !item.ValidFrom.Before(*item.ExpiresAt) {
		return fmt.Errorf("valid-from must precede expires-at")
	}
	if _, err := team.ParseContextActivation(activationFixtureMetadata(item.Activation)); err != nil {
		return err
	}
	return nil
}

func contextFixtureItem(ctx context.Context, fixture ContextItemFixture, scope contextstore.Scope) contextstore.ContextItem {
	item := contextstore.ContextItem{
		ID: fixture.ID, Kind: contextstore.ContextPattern, Content: fixture.Content, Scope: scope,
		Authority: contextstore.AuthorityRepository, TrustLevel: contextstore.TrustTrusted,
		Priority: contextstore.PriorityHigh, MustKeep: fixture.MustKeep, Confidence: 1,
		Source:    contextstore.SourceRef{Type: "eval_fixture", Ref: fixture.ID},
		Lifecycle: contextstore.LifecycleConfirmed, Metadata: activationFixtureMetadata(fixture.Activation),
		ValidFrom: fixture.ValidFrom, ValidUntil: fixture.ValidUntil, ExpiresAt: fixture.ExpiresAt,
		CreatedAt: contextstore.EvaluationTime(ctx),
	}
	if fixture.Kind != "" {
		item.Kind = contextstore.ContextKind(fixture.Kind)
	}
	if fixture.Lifecycle != "" {
		item.Lifecycle = contextstore.ContextLifecycle(fixture.Lifecycle)
	}
	if fixture.Priority != nil {
		item.Priority = contextstore.Priority(*fixture.Priority)
	}
	if fixture.Confidence != nil {
		item.Confidence = *fixture.Confidence
	}
	return item
}

// fixtureScope resolves aliases against the harness-owned team/workspace. The
// resolver is seed-only; it is never a runtime grant or model-facing tool.
func fixtureScope(fixture ContextItemFixture, base contextstore.Scope, workers map[string]string) (contextstore.Scope, error) {
	scope := base
	scope.SessionID, scope.BranchID, scope.AgentID, scope.TaskID, scope.AttemptID = "", "", "", "", ""
	switch fixture.Scope {
	case "", "same_team_shared":
	case "other_team":
		scope.TeamID += "-eval-other"
	case "other_worker":
		scope.AgentID = "eval-other-worker"
		for slices.Contains(slices.Collect(maps.Values(workers)), scope.AgentID) {
			scope.AgentID += "-other"
		}
	case "same_worker_private", "active_branch", "ancestor_before_fork", "ancestor_after_fork", "sibling_branch":
		worker, ok := workers[fixture.Agent]
		if !ok || worker == "" {
			return scope, fmt.Errorf("context item %q requires a known memory worker", fixture.ID)
		}
		scope.AgentID = worker
		if fixture.Scope != "same_worker_private" {
			if fixture.Branch == "" {
				return scope, fmt.Errorf("context item %q requires an explicit seeded branch", fixture.ID)
			}
			scope.SessionID, scope.BranchID = base.SessionID, fixture.Branch
		}
	default:
		return scope, fmt.Errorf("unsupported context seed scope %q", fixture.Scope)
	}
	return scope, nil
}
