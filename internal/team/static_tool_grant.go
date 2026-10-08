package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// staticToolGrantNames returns the phase-free set of built-in and
// agent-scoped MCP tool names that def's configuration grants for task. It
// applies neither the phase gate nor template grants: a grant only exempts
// the phase gate, which is a lifecycle narrowing reapplied on every
// resolution. Protocol tools, team action tools, the dynamic gateway, and
// manager MCP targets are governed elsewhere and are not included.
func (c *Coordinator) staticToolGrantNames(def *agent.AgentDef, task TaskDef) []string {
	names := []string{}
	if c == nil || def == nil {
		return names
	}
	candidate := filterImplicitArtifactPolicyDeniedTools(def, agent.SelectTools(c.coreTools, def.Tools), task.WorksetBinding != nil, c.effectiveSideEffect(task) == SideEffectNone)
	candidate = c.filterCoordinatorOnlyWorkerTools(c.filterTeamDeniedWorkerTools(c.filterLegacyMemoryMutationTools(def, candidate), nil, false))
	names = append(names, agentToolNames(candidate)...)
	for key := range def.MCPTools {
		if key = strings.TrimSpace(key); key != "" && !c.toolDeniedByTeam(key) && !isCoordinatorOnlyWorkerTool(key) {
			names = append(names, key)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// filterToolsByStaticCeiling drops tools outside the occurrence's frozen
// ceiling. A nil snapshot keeps the unfrozen behavior.
func filterToolsByStaticCeiling(candidate []fantasy.AgentTool, snapshot *DynamicToolAuthorizationSnapshot) []fantasy.AgentTool {
	if snapshot == nil {
		return candidate
	}
	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	for _, tool := range candidate {
		if tool != nil && slices.Contains(snapshot.StaticToolCeiling, tool.Info().Name) {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

// frozenBaseWorkerTools returns the live base tools bounded by the
// occurrence's frozen ceiling, and records any narrowing. Live configuration
// may narrow the surface but never widen it. A result-only lifecycle has no
// base tools.
func (c *Coordinator) frozenBaseWorkerTools(ctx context.Context, todo *TodoItem, def *agent.AgentDef, task TaskDef, snapshot *DynamicToolAuthorizationSnapshot, prospective, resultOnly bool) ([]fantasy.AgentTool, error) {
	if resultOnly {
		return nil, nil
	}
	base := filterToolsByStaticCeiling(c.selectWorkerToolsForTask(def, task), snapshot)
	if err := c.noteStaticToolGrantNarrowing(ctx, todo, def, task, snapshot, prospective); err != nil {
		return nil, err
	}
	return base, nil
}

// staticToolGrantDiff compares two sorted name sets: ignored are live grants
// the ceiling excludes, and unavailable are frozen names live configuration
// no longer grants.
func staticToolGrantDiff(ceiling, live []string) (ignored, unavailable []string) {
	for _, name := range live {
		if !slices.Contains(ceiling, name) {
			ignored = append(ignored, name)
		}
	}
	for _, name := range ceiling {
		if !slices.Contains(live, name) {
			unavailable = append(unavailable, name)
		}
	}
	return ignored, unavailable
}

// noteStaticToolGrantNarrowing records when live configuration differs from
// an occurrence's frozen ceiling. The comparison is phase-free, so a phase
// gate never looks like narrowing. Result-only lifecycles and prospective
// resolutions before task_created record nothing.
func (c *Coordinator) noteStaticToolGrantNarrowing(ctx context.Context, todo *TodoItem, def *agent.AgentDef, task TaskDef, snapshot *DynamicToolAuthorizationSnapshot, prospective bool) error {
	if c == nil || todo == nil || snapshot == nil || prospective {
		return nil
	}
	ignored, unavailable := staticToolGrantDiff(snapshot.StaticToolCeiling, c.staticToolGrantNames(def, task))
	if len(ignored) == 0 && len(unavailable) == 0 {
		return nil
	}
	return c.recordStaticToolGrantNarrowed(ctx, todo, ignored, unavailable)
}

func (c *Coordinator) recordStaticToolGrantNarrowed(ctx context.Context, todo *TodoItem, ignored, unavailable []string) error {
	revision := max(1, todo.OccurrenceRevision)
	payload := struct {
		Type               string   `json:"type"`
		TaskID             string   `json:"task_id"`
		OccurrenceRevision int      `json:"occurrence_revision"`
		Ignored            []string `json:"ignored"`
		Unavailable        []string `json:"unavailable"`
	}{
		Type: string(EventStaticToolGrantNarrowed), TaskID: todo.ID, OccurrenceRevision: revision,
		Ignored: append([]string{}, ignored...), Unavailable: append([]string{}, unavailable...),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode static tool grant narrowing: %w", err)
	}
	hasher := sha256.New()
	writeDigestRecord(hasher, append([]string{"ignored"}, ignored...)...)
	writeDigestRecord(hasher, append([]string{"unavailable"}, unavailable...)...)
	key := fmt.Sprintf("static-tool-grant-narrowed:%s:%d:%s", todo.ID, revision, hex.EncodeToString(hasher.Sum(nil))[:16])
	if c.hasDurableEventJournal() {
		if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
			Type: string(EventStaticToolGrantNarrowed), Actor: "runtime", TaskID: todo.ID,
			IdempotencyKey: key, Payload: raw,
		}); err != nil {
			return fmt.Errorf("record static tool grant narrowing: %w", err)
		}
	}
	if _, reported := c.staticToolNarrowingReported.LoadOrStore(key, true); !reported {
		c.report(c.newEvent(string(EventStaticToolGrantNarrowed)).withTodoID(todo.ID).withMessage(fmt.Sprintf(
			"frozen tool grant kept: ignored=[%s] unavailable=[%s]", strings.Join(ignored, ","), strings.Join(unavailable, ","))))
	}
	return nil
}
