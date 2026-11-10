package team

import (
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/kjelly/hufu/internal/agent"
)

// LaunchOptions is an explicit allowlist of command options needed to resume.
// Never add argv, environment values, provider URLs/keys, prompt text or shell
// commands here. Secrets remain supplied by the current host environment.
type LaunchOptions struct {
	ExecutionProfile string   `json:"execution_profile"`
	NoNet            bool     `json:"no_net"`
	ForceMCP         bool     `json:"force_mcp"`
	RestrictedBash   bool     `json:"restricted_bash"`
	PlanFirst        bool     `json:"plan_first"`
	AutoSkills       bool     `json:"auto_skills"`
	ForcedSkills     []string `json:"forced_skills,omitempty"`
	Unattended       bool     `json:"unattended"`
	AutoApprove      bool     `json:"auto_approve"`
	NoJournal        bool     `json:"no_journal"`
	DecisionProfile  string   `json:"decision_profile"`
	MaxDuration      int64    `json:"max_duration"`
	MaxTotalTokens   int64    `json:"generation_budget"`
}

// LaunchGeneration keeps generation values typed and separately named from
// credential-like keys (notably token). It intentionally has no provider data.
type LaunchGeneration struct {
	Model           string `json:"model"`
	ContextWindow   int    `json:"context_window"`
	Temperature     string `json:"temperature"`
	MaxOutput       string `json:"max_output"`
	TopP            string `json:"top_p"`
	TopK            string `json:"top_k"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type LaunchAgentSettings struct {
	Generation     LaunchGeneration `json:"generation"`
	Timeout        int64            `json:"timeout"`
	MaxSteps       int              `json:"max_steps"`
	ExecutionRoute string           `json:"execution_route"`
}

// LaunchSettings is stored inside the canonical execution-policy event in
// runtime.sqlite before provider preflight. Its values are covered by the
// policy hash and event chain and follow the selected branch's event lineage.
// It restores startup inputs, never prompts, tools, contracts or credentials.
type LaunchSettings struct {
	Version           int                            `json:"version"`
	Options           LaunchOptions                  `json:"options"`
	Generation        LaunchGeneration               `json:"generation"`
	Timeout           int64                          `json:"timeout"`
	VerifyTimeout     int64                          `json:"verify_timeout"`
	WorkerModel       string                         `json:"worker_model"`
	CoordinatorModel  string                         `json:"coordinator_model"`
	SidecarModel      string                         `json:"sidecar_model"`
	GuardModel        string                         `json:"guard_model"`
	JudgeModel        string                         `json:"judge_model"`
	PlanReviewerModel string                         `json:"plan_reviewer_model"`
	DefaultLLMBackend string                         `json:"default_llm_backend"`
	ExecutionRoute    string                         `json:"execution_route"`
	MaxConcurrent     int                            `json:"max_concurrent"`
	MaxRounds         int                            `json:"max_rounds"`
	MaxSteps          int                            `json:"max_steps"`
	GoalMode          string                         `json:"goal_mode"`
	RequestAcceptance bool                           `json:"request_acceptance"`
	Agents            map[string]LaunchAgentSettings `json:"agents"`
}

func launchGeneration(g agent.GenerationParams) LaunchGeneration {
	return LaunchGeneration{g.Model, g.ContextWindow, g.Temperature, g.MaxTokens, g.TopP, g.TopK, g.ReasoningEffort}
}

func (g LaunchGeneration) params() agent.GenerationParams {
	return agent.GenerationParams{Model: g.Model, ContextWindow: g.ContextWindow, Temperature: g.Temperature,
		MaxTokens: g.MaxOutput, TopP: g.TopP, TopK: g.TopK, ReasoningEffort: g.ReasoningEffort}
}

func captureLaunchSettings(c *Coordinator, version int) (*LaunchSettings, error) {
	if version < executionPolicySnapshotVersion || c.session.LaunchOptions == nil {
		return nil, nil
	}
	s := c.session
	cfg := s.Config
	settings := &LaunchSettings{
		Version: 1, Options: *s.LaunchOptions, Generation: launchGeneration(cfg.Generation),
		Timeout: cfg.Timeout, VerifyTimeout: cfg.VerifyTimeout,
		WorkerModel: cfg.WorkerModel, CoordinatorModel: cfg.CoordinatorModel,
		SidecarModel: c.sidecarModel, GuardModel: c.guardModel, JudgeModel: c.judgeModel, PlanReviewerModel: c.planReviewerModel,
		DefaultLLMBackend: cfg.DefaultLLMBackend, ExecutionRoute: cfg.ExecutionRoute,
		MaxConcurrent: c.maxConcurrent, MaxRounds: cfg.MaxRounds, MaxSteps: cfg.MaxSteps,
		GoalMode: cfg.GoalMode, RequestAcceptance: cfg.RequestAcceptance,
		Agents: make(map[string]LaunchAgentSettings, len(s.Agents)),
	}
	settings.Options.ForcedSkills = slices.Clone(settings.Options.ForcedSkills)
	for name, def := range s.Agents {
		if def != nil {
			settings.Agents[name] = LaunchAgentSettings{launchGeneration(def.Generation), def.Timeout, def.MaxSteps, def.ExecutionRoute}
		}
	}
	return settings, settings.validate()
}

func cloneLaunchSettings(settings *LaunchSettings) *LaunchSettings {
	if settings == nil {
		return nil
	}
	clone := *settings
	clone.Options.ForcedSkills = slices.Clone(settings.Options.ForcedSkills)
	clone.Agents = maps.Clone(settings.Agents)
	return &clone
}

func (g LaunchGeneration) validate() error {
	if g.ContextWindow < 0 {
		return fmt.Errorf("negative context window")
	}
	for _, value := range []string{g.Temperature, g.MaxOutput, g.TopP, g.TopK} {
		if value != "" {
			if _, err := strconv.ParseFloat(value, 64); err != nil {
				return fmt.Errorf("non-numeric generation parameter")
			}
		}
	}
	return nil
}

func (s *LaunchSettings) validate() error {
	if s.Version != 1 || s.Agents == nil || s.Timeout < 0 || s.VerifyTimeout < 0 || s.MaxConcurrent < 0 || s.MaxRounds < 0 || s.MaxSteps < 0 {
		return fmt.Errorf("invalid launch settings")
	}
	if err := s.Generation.validate(); err != nil {
		return fmt.Errorf("invalid team launch generation: %w", err)
	}
	for _, a := range s.Agents {
		if a.Timeout < 0 || a.MaxSteps < 0 {
			return fmt.Errorf("invalid agent launch settings")
		}
		if err := a.Generation.validate(); err != nil {
			return fmt.Errorf("invalid agent launch generation: %w", err)
		}
	}
	return nil
}

// RestoreLaunchSettings rehydrates only startup inputs from the selected
// event-authoritative policy. Call before applying explicit CLI overrides and
// before model resolution, MCP startup or provider preflight. Ordinary policy
// admission still verifies the reconstructed configuration afterwards.
// Legacy snapshots have no launch settings and retain their original checks.
func RestoreLaunchSettings(session *TeamSession, policy *ExecutionPolicySnapshot) error {
	if policy == nil {
		return nil
	}
	if err := validateExecutionPolicySnapshot(policy); err != nil {
		return err
	}
	if policy.Launch == nil {
		return nil
	}
	if session == nil {
		return fmt.Errorf("launch restoration requires a team session")
	}
	s := policy.Launch
	// Never add/remove agents or infer a replacement for a missing identity.
	count := 0
	for name, def := range session.Agents {
		if def != nil {
			count++
			if _, ok := s.Agents[name]; !ok {
				return fmt.Errorf("launch agent set changed; restore the original team definitions")
			}
		}
	}
	if count != len(s.Agents) {
		return fmt.Errorf("launch agent set changed; restore the original team definitions")
	}
	cfg := &session.Config
	cfg.Generation, cfg.Timeout, cfg.VerifyTimeout = s.Generation.params(), s.Timeout, s.VerifyTimeout
	cfg.WorkerModel, cfg.CoordinatorModel = s.WorkerModel, s.CoordinatorModel
	cfg.SidecarModel, cfg.GuardModel, cfg.JudgeModel, cfg.PlanReviewerModel = s.SidecarModel, s.GuardModel, s.JudgeModel, s.PlanReviewerModel
	cfg.DefaultLLMBackend, cfg.ExecutionRoute = s.DefaultLLMBackend, s.ExecutionRoute
	cfg.MaxConcurrent, cfg.MaxRounds, cfg.MaxSteps = s.MaxConcurrent, s.MaxRounds, s.MaxSteps
	cfg.GoalMode, cfg.RequestAcceptance = s.GoalMode, s.RequestAcceptance
	cfg.ExecutionProfile = s.Options.ExecutionProfile
	session.LaunchOptions = new(s.Options)
	session.LaunchOptions.ForcedSkills = slices.Clone(s.Options.ForcedSkills)
	session.RestoredLaunchSettings = true
	for name, a := range s.Agents {
		def := session.Agents[name]
		def.Generation, def.Timeout, def.MaxSteps, def.ExecutionRoute = a.Generation.params(), a.Timeout, a.MaxSteps, a.ExecutionRoute
	}
	return nil
}
