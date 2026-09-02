package team

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/kjelly/hufu/internal/agent"
)

// CompatSnapshot is a deterministic, environment-independent projection of a
// loaded team's effective semantics. It exists so behavior-preserving work on
// the team-authoring/compiler pipeline (see spec.md) can be checked against a
// golden fixture instead of relying on ad hoc diffing across the dozens of
// fields on agent.TeamConfig/AgentDef/TaskDef.
//
// It deliberately omits fields that are absolute-path or environment
// dependent (WorkspaceDir, ProviderRegistry, credentials, ...); those are
// exercised by other tests. Only fields relevant to authored/resolved
// semantics — the things a team author configures or relies on defaults for
// — are included.
type CompatSnapshot struct {
	Team   TeamSnapshot    `json:"team"`
	Agents []AgentSnapshot `json:"agents"`
	Tasks  []TaskSnapshot  `json:"tasks,omitempty"`
}

// TeamSnapshot captures the resolved team-level configuration.
type TeamSnapshot struct {
	Name                     string                   `json:"name"`
	Description              string                   `json:"description,omitempty"`
	MaxRounds                int                      `json:"max_rounds"`
	MinimumCoordinatorRounds int                      `json:"minimum_coordinator_rounds,omitempty"`
	MaxSteps                 int                      `json:"max_steps,omitempty"`
	Timeout                  int64                    `json:"timeout"`
	VerifyTimeout            int64                    `json:"verify_timeout"`
	MaxRetries               int                      `json:"max_retries"`
	MaxConcurrent            int                      `json:"max_concurrent,omitempty"`
	AutoReport               bool                     `json:"auto_report,omitempty"`
	AllowFreeTextResults     bool                     `json:"allow_free_text_results,omitempty"`
	NoNet                    bool                     `json:"no_net,omitempty"`
	ForceMCP                 bool                     `json:"force_mcp,omitempty"`
	Unattended               bool                     `json:"unattended,omitempty"`
	AutoApprove              bool                     `json:"auto_approve,omitempty"`
	MaxWallClock             int64                    `json:"max_wall_clock,omitempty"`
	MaxTotalTokens           int64                    `json:"max_total_tokens,omitempty"`
	Acceptance               string                   `json:"acceptance,omitempty"`
	AcceptanceSpec           *agent.AcceptanceSpec    `json:"acceptance_spec,omitempty"`
	Rollback                 string                   `json:"rollback,omitempty"`
	ExecutionProfile         string                   `json:"execution_profile,omitempty"`
	GoalMode                 string                   `json:"goal_mode,omitempty"`
	ToolsAllowed             []string                 `json:"tools_allowed,omitempty"`
	ToolsDenied              []string                 `json:"tools_denied,omitempty"`
	Workflow                 agent.WorkflowConfig     `json:"workflow,omitzero"`
	Policies                 agent.WorkflowPolicies   `json:"policies,omitzero"`
	Capabilities             agent.CapabilityConfig   `json:"capabilities,omitzero"`
	Verification             agent.VerificationConfig `json:"verification,omitzero"`
	Retry                    agent.RetryConfig        `json:"retry,omitzero"`
	Delegation               agent.DelegationPolicy   `json:"delegation,omitzero"`
}

// AgentSnapshot captures one agent's resolved (post-inference, post-expansion)
// configuration — the effective tool list, side-effect class, and policy
// restrictions a coordinator/runtime would actually see.
type AgentSnapshot struct {
	Name           string   `json:"name"`
	Role           string   `json:"role"`
	Tools          string   `json:"tools,omitempty"`
	SideEffect     string   `json:"side_effect,omitempty"`
	Recovery       string   `json:"recovery,omitempty"`
	MaxRetries     int      `json:"max_retries,omitempty"`
	MaxSteps       int      `json:"max_steps,omitempty"`
	NoNet          bool     `json:"no_net,omitempty"`
	ForceMCP       bool     `json:"force_mcp,omitempty"`
	AllowedPaths   []string `json:"allowed_paths,omitempty"`
	RestrictedPath string   `json:"restricted_path,omitempty"`
}

// TaskSnapshot captures one authored static task/contract entry. Fields that
// carry json:"-" on TaskDef itself (Phase, WhenGoalContains, Action) are
// deliberately re-exposed here: those are exactly the authoring-time fields a
// team-compiler regression is most likely to silently drop or reorder.
type TaskSnapshot struct {
	ID               string `json:"id,omitempty"`
	Agent            string `json:"agent"`
	Phase            string `json:"phase,omitempty"`
	WhenGoalContains string `json:"when_goal_contains,omitempty"`
	SideEffect       string `json:"side_effect,omitempty"`
	Recovery         string `json:"recovery,omitempty"`
	OutputMode       string `json:"output_mode,omitempty"`
	Optional         bool   `json:"optional,omitempty"`
}

// BuildCompatSnapshot projects a loaded TeamSession into a CompatSnapshot.
func BuildCompatSnapshot(session *TeamSession) (*CompatSnapshot, error) {
	if session == nil {
		return nil, fmt.Errorf("build compat snapshot: session is nil")
	}
	cfg := session.Config

	snapshot := &CompatSnapshot{
		Team: TeamSnapshot{
			Name:                     cfg.Name,
			Description:              cfg.Description,
			MaxRounds:                cfg.MaxRounds,
			MinimumCoordinatorRounds: cfg.MinimumCoordinatorRounds,
			MaxSteps:                 cfg.MaxSteps,
			Timeout:                  cfg.Timeout,
			VerifyTimeout:            cfg.VerifyTimeout,
			MaxRetries:               cfg.MaxRetries,
			MaxConcurrent:            cfg.MaxConcurrent,
			AutoReport:               cfg.AutoReport,
			AllowFreeTextResults:     cfg.AllowFreeTextResults,
			NoNet:                    cfg.NoNet,
			ForceMCP:                 cfg.ForceMCP,
			Unattended:               cfg.Unattended,
			AutoApprove:              cfg.AutoApprove,
			MaxWallClock:             cfg.MaxWallClock,
			MaxTotalTokens:           cfg.MaxTotalTokens,
			Acceptance:               cfg.Acceptance,
			AcceptanceSpec:           cfg.AcceptanceSpec,
			Rollback:                 cfg.Rollback,
			ExecutionProfile:         cfg.ExecutionProfile,
			GoalMode:                 cfg.GoalMode,
			ToolsAllowed:             append([]string(nil), cfg.ToolsAllowed...),
			ToolsDenied:              append([]string(nil), cfg.ToolsDenied...),
			Workflow:                 cfg.Workflow,
			Policies:                 cfg.Policies,
			Capabilities:             cfg.Capabilities,
			Verification:             cfg.Verification,
			Retry:                    cfg.Retry,
			Delegation:               cfg.Delegation,
		},
	}

	agentNames := make([]string, 0, len(session.Agents))
	for name := range session.Agents {
		agentNames = append(agentNames, name)
	}
	sort.Strings(agentNames)
	for _, name := range agentNames {
		def := session.Agents[name]
		if def == nil {
			continue
		}
		snapshot.Agents = append(snapshot.Agents, AgentSnapshot{
			Name:           def.Name,
			Role:           def.Role,
			Tools:          def.Tools,
			SideEffect:     def.SideEffect,
			Recovery:       def.Recovery,
			MaxRetries:     def.MaxRetries,
			MaxSteps:       def.MaxSteps,
			NoNet:          def.NoNet,
			ForceMCP:       def.ForceMCP,
			AllowedPaths:   append([]string(nil), def.AllowedPaths...),
			RestrictedPath: def.RestrictedPath,
		})
	}

	for _, task := range session.ContractTasks {
		snapshot.Tasks = append(snapshot.Tasks, TaskSnapshot{
			ID:               task.ID,
			Agent:            task.Agent,
			Phase:            string(task.Phase),
			WhenGoalContains: task.WhenGoalContains,
			SideEffect:       string(task.SideEffect),
			Recovery:         string(task.Recovery),
			OutputMode:       task.OutputMode,
			Optional:         task.Optional,
		})
	}

	return snapshot, nil
}

// JSON renders the snapshot as stable, indented JSON suitable for a golden
// fixture file.
func (s *CompatSnapshot) JSON() ([]byte, error) {
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal compat snapshot: %w", err)
	}
	return append(out, '\n'), nil
}
