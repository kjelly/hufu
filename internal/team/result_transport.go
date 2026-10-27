package team

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

type ExecutionResultTransportAgentPolicy struct {
	Agent     string                `json:"agent"`
	Transport agent.ResultTransport `json:"transport"`
}

// An absent policy preserves historical tool transport and serialization.
// The policy and immutable task contract jointly determine the protocol before
// work begins; callers cannot select it through dynamic task arguments.
type ExecutionResultCollectionPolicySnapshot struct {
	Version                 int                                   `json:"version"`
	Default                 agent.ResultTransport                 `json:"default"`
	DefaultExplicit         bool                                  `json:"default_explicit,omitzero"`
	Agents                  []ExecutionResultTransportAgentPolicy `json:"agents,omitempty"`
	CollectorSchemaRevision int                                   `json:"collector_schema_revision"`
	TurnLimit               int                                   `json:"turn_limit"`
	GenerationSHA256        string                                `json:"generation_sha256"`
}

func cloneExecutionResultCollectionPolicy(value *ExecutionResultCollectionPolicySnapshot) *ExecutionResultCollectionPolicySnapshot {
	if value == nil {
		return nil
	}
	clone := *value
	clone.Agents = slices.Clone(value.Agents)
	return &clone
}

func executionPolicyResultCollection(session *TeamSession, version int) (*ExecutionResultCollectionPolicySnapshot, error) {
	if session == nil {
		return nil, nil
	}
	policy := &ExecutionResultCollectionPolicySnapshot{Version: 1, Default: agent.ResultTransportTool,
		CollectorSchemaRevision: resultCollectionVersion, TurnLimit: resultCollectionDefaultTurns}
	configured := false
	if mode := session.Config.ResultTransport; mode != "" {
		if !mode.Valid() {
			return nil, fmt.Errorf("invalid team result transport %q", mode)
		}
		policy.Default, policy.DefaultExplicit, configured = mode, true, true
	}
	for name, def := range session.Agents {
		if def == nil || def.ResultTransport == "" {
			continue
		}
		if !def.ResultTransport.Valid() {
			return nil, fmt.Errorf("agent %s: invalid result transport %q", name, def.ResultTransport)
		}
		policy.Agents = append(policy.Agents, ExecutionResultTransportAgentPolicy{Agent: strings.ToLower(strings.TrimSpace(name)), Transport: def.ResultTransport})
		configured = true
	}
	if !configured {
		return nil, nil
	}
	type collectorGeneration struct {
		Generation agent.GenerationParams
		Timeout    int64
	}
	generation := make(map[string]collectorGeneration, len(session.Agents))
	for name, def := range session.Agents {
		if def != nil {
			generation[name] = collectorGeneration{def.Generation, def.Timeout}
		}
	}
	var err error
	policy.GenerationSHA256, err = collectionDigest(struct {
		Team        agent.GenerationParams
		TeamTimeout int64
		Agents      map[string]collectorGeneration
	}{session.Config.Generation, session.Config.Timeout, generation})
	if err != nil {
		return nil, err
	}
	if version < executionPolicySnapshotVersion {
		return nil, fmt.Errorf("legacy execution policy cannot pin result transport; start a new session")
	}
	slices.SortFunc(policy.Agents, func(a, b ExecutionResultTransportAgentPolicy) int { return strings.Compare(a.Agent, b.Agent) })
	return policy, validateExecutionResultCollectionPolicy(policy)
}

func validateExecutionResultCollectionPolicy(policy *ExecutionResultCollectionPolicySnapshot) error {
	if policy == nil {
		return nil
	}
	if policy.Version != 1 || !policy.Default.Valid() || policy.CollectorSchemaRevision != resultCollectionVersion || policy.TurnLimit != resultCollectionDefaultTurns || !validCollectionDigest(policy.GenerationSHA256) {
		return fmt.Errorf("invalid result collection policy")
	}
	last := ""
	for _, entry := range policy.Agents {
		if entry.Agent == "" || entry.Agent != strings.ToLower(strings.TrimSpace(entry.Agent)) || entry.Agent <= last || !entry.Transport.Valid() {
			return fmt.Errorf("invalid result transport agent policy")
		}
		last = entry.Agent
	}
	return nil
}

func resultTransportFromPolicy(policy *ExecutionResultCollectionPolicySnapshot, agentName string, task TaskDef) (agent.ResultTransport, error) {
	if task.Sidecar || task.Action != nil {
		return agent.ResultTransportTool, nil
	}
	if err := validateExecutionResultCollectionPolicy(policy); err != nil {
		return "", err
	}
	if policy == nil {
		return agent.ResultTransportTool, nil
	}
	mode, explicit := policy.Default, policy.DefaultExplicit
	for _, entry := range policy.Agents {
		if entry.Agent == strings.ToLower(strings.TrimSpace(agentName)) {
			mode, explicit = entry.Transport, true
			break
		}
	}
	if mode == agent.ResultTransportRuntime && slices.Contains(task.Execution.ToolSequence, submitResultToolName) {
		if explicit {
			return "", fmt.Errorf("runtime result transport conflicts with the closed submit_result sequence")
		}
		return agent.ResultTransportTool, nil
	}
	return mode, nil
}

type workerResultTargetKey struct{}

func (c *Coordinator) resultTransportForTask(def *agent.AgentDef, task TaskDef) (agent.ResultTransport, error) {
	return c.resultTransportForTaskContext(context.Background(), def, task)
}

func (c *Coordinator) resultTransportForTaskContext(ctx context.Context, def *agent.AgentDef, task TaskDef) (agent.ResultTransport, error) {
	if c == nil || def == nil {
		return agent.ResultTransportTool, nil
	}
	switch strings.ToLower(strings.ReplaceAll(def.Role, "-", "_")) {
	case "coordinator", "orchestrator", "auxiliary", "plan_reviewer":
		return agent.ResultTransportTool, nil
	}
	var frozen *ExecutionPolicySnapshot
	c.viewSessionData(func(sd *SessionData) { frozen = cloneExecutionPolicySnapshot(sd.ExecutionPolicySnapshot) })
	if frozen == nil {
		frozen = c.ExecutionPolicySnapshot()
	}
	if frozen == nil || frozen.ResultCollection == nil {
		return agent.ResultTransportTool, nil
	}
	// Result transport applies only to local LLM candidates. External proposals,
	// runtime actions, and plan/sidecar protocols retain their existing adapters.
	target := task.ResolvedExecutionTarget
	if actual, ok := ctx.Value(workerResultTargetKey{}).(execution.ExecutionTarget); ok {
		target = actual
	}
	if target.IsZero() {
		return agent.ResultTransportTool, nil
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
	if err != nil {
		return "", err
	}
	if backend.Kind() != execution.BackendKindLLM {
		return agent.ResultTransportTool, nil
	}
	return resultTransportFromPolicy(frozen.ResultCollection, def.Name, task)
}
