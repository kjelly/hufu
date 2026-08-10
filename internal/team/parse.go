package team

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/notify"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/yamlutil"
)

type TeamSession struct {
	Config        agent.TeamConfig
	Dir           string
	Workspace     string
	Agents        map[string]*agent.AgentDef
	MCPServers    map[string]mcp.MCPServerConfig
	Skills        []*skill.SkillDef
	ContractTasks []TaskDef // Optional static task contracts used by preflight tooling.
}

type agentFrontmatter struct {
	Name            string                         `yaml:"name"`
	Description     string                         `yaml:"description"`
	Role            string                         `yaml:"role"`
	Tools           any                            `yaml:"tools"`  // string or []string (YAML list)
	Skills          any                            `yaml:"skills"` // string or []string (YAML list)
	Guard           []string                       `yaml:"guard"`
	Model           string                         `yaml:"model"`
	ExtraModels     []string                       `yaml:"extra-models"`
	Temperature     string                         `yaml:"temperature"`
	MaxTokens       string                         `yaml:"max-tokens"`
	TopP            string                         `yaml:"top-p"`
	TopK            string                         `yaml:"top-k"`
	ReasoningEffort string                         `yaml:"reasoning-effort"`
	Timeout         int64                          `yaml:"timeout"`
	MaxRetries      any                            `yaml:"max-retries"` // int or string
	MaxSteps        int                            `yaml:"max-steps"`
	ProviderURL     string                         `yaml:"provider-url"`
	AllowedPaths    any                            `yaml:"allowed-paths"` // string or []string
	RestrictedPath  string                         `yaml:"restricted-path"`
	NoNet           bool                           `yaml:"no-net"`
	ForceMCP        bool                           `yaml:"force-mcp"`
	Shell           string                         `yaml:"shell"`
	MCPTools        map[string]agent.MCPToolConfig `yaml:"mcp-tools"`
	SideEffect      string                         `yaml:"side_effect"`
	Recovery        string                         `yaml:"recovery"`
	ReconcileTool   string                         `yaml:"reconcile-tool"`
	MemoryID        string                         `yaml:"memory-id"`
	Memory          rawWorkerMemoryPolicy          `yaml:"memory"`
}

type teamConfigYAML struct {
	Name                string                           `yaml:"name"`
	Description         string                           `yaml:"description"`
	MaxRounds           int                              `yaml:"max-rounds"`
	MaxSteps            int                              `yaml:"max-steps"`
	Workspace           string                           `yaml:"workspace"`
	Timeout             int64                            `yaml:"timeout"`
	VerifyTimeout       int64                            `yaml:"verify-timeout"`
	MaxRetries          int                              `yaml:"max-retries"`
	Model               string                           `yaml:"model"`
	Temperature         string                           `yaml:"temperature"`
	MaxTokens           string                           `yaml:"max-tokens"`
	TopP                string                           `yaml:"top-p"`
	TopK                string                           `yaml:"top-k"`
	ReasoningEffort     string                           `yaml:"reasoning-effort"`
	Skills              string                           `yaml:"skills"`
	SkillsExclude       string                           `yaml:"skills-exclude"`
	ProviderURL         string                           `yaml:"provider-url"`
	ProviderAPIKey      string                           `yaml:"provider-api-key"`
	Providers           map[string]config.ProviderConfig `yaml:"providers"`
	ModelList           []config.ModelEntry              `yaml:"model-list"`
	SidecarModel        string                           `yaml:"sidecar-model"`
	GuardModel          string                           `yaml:"guard-model"`
	JudgeModel          string                           `yaml:"judge-model"`
	PlanReviewerModel   string                           `yaml:"plan-reviewer-model"`
	MaxConcurrent       int                              `yaml:"max-concurrent"`
	MaxCoordinatorTurns int                              `yaml:"max-coordinator-turns"`
	EscalateOnRetry     bool                             `yaml:"escalate-on-retry"`
	Notify              notify.NotifyConfig              `yaml:"notify"`
	AllowedPaths        interface{}                      `yaml:"allowed-paths"`
	RestrictedPath      string                           `yaml:"restricted-path"`
	NoNet               bool                             `yaml:"no-net"`
	ForceMCP            bool                             `yaml:"force-mcp"`
	ProjectContext      bool                             `yaml:"project-context"`
	Shell               string                           `yaml:"shell"`
	Vars                map[string]interface{}           `yaml:"vars"`
	// WorkerContextSize is a token budget, not a character count (spec.md
	// item 7); the YAML key is kept as-is for backward compatibility.
	WorkerContextSize int                           `yaml:"worker-context-size"`
	ToolsAllowed      interface{}                   `yaml:"tools"` // tools.allowed/tools.denied in YAML - string or []string
	Delegation        rawDelegationPolicy           `yaml:"delegation"`
	Preflight         []agent.CapabilityRequirement `yaml:"preflight"`
	Unattended        bool                          `yaml:"unattended"`
	AutoApprove       bool                          `yaml:"auto-approve"`
	MaxWallClock      int64                         `yaml:"max-duration"`
	MaxTotalTokens    int64                         `yaml:"max-total-tokens"`
	Acceptance        interface{}                   `yaml:"acceptance"`
	Rollback          string                        `yaml:"rollback"`
	ExecutionProfile  string                        `yaml:"execution-profile"`
	GoalMode          string                        `yaml:"goal-mode"`
	Reliability       rawReliabilityConfig          `yaml:"reliability"`
	WorkerMemory      rawWorkerMemoryPolicy         `yaml:"worker-memory"`
	Tasks             []TaskDef                     `yaml:"tasks"`
}

type rawDelegationPolicy struct {
	AllowedWorkers []string `yaml:"allowed-workers"`
	InitialBatch   struct {
		Agents        []string `yaml:"agents"`
		Exact         bool     `yaml:"exact"`
		FirstTool     string   `yaml:"first-tool"`
		BindContracts bool     `yaml:"bind-contracts"`
	} `yaml:"initial-batch"`
	NoRedispatchAfterSuccess []string                  `yaml:"no-redispatch-after-success"`
	ForbidContextFiles       bool                      `yaml:"forbid-context-files"`
	TaskGoalInvariants       []agent.TaskGoalInvariant `yaml:"task-goal-invariants"`
}

type rawReliabilityConfig struct {
	Rollout                           string `yaml:"rollout"`
	MaxDiagnosticTasksWithoutProgress int    `yaml:"max-diagnostic-tasks-without-progress"`
	MaxSameFailureFingerprint         int    `yaml:"max-same-failure-fingerprint"`
	MaxRepairsPerCriterion            int    `yaml:"max-repairs-per-criterion"`
	// MaxSystemicFailureTasks is a pointer so an explicit YAML zero
	// (max-systemic-failure-tasks: 0) is distinguishable from unset and
	// can override the default (3) to disable the feature. Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
	MaxSystemicFailureTasks *int   `yaml:"max-systemic-failure-tasks"`
	HardEnforcement         *bool  `yaml:"hard-enforcement"`
	WarnOnly                bool   `yaml:"warn-only"`
	VerifierLintMode        string `yaml:"verifier-lint"`
	// No-progress budget pointers (§8.1, WP-12). Pointers so an explicit
	// YAML 0 (disable) is distinguishable from unset (restore default).
	MaxTokensWithoutProgress *int `yaml:"max-tokens-without-progress"`
	MaxTurnsWithoutProgress  *int `yaml:"max-turns-without-progress"`
	MaxTasksWithoutProgress  *int `yaml:"max-tasks-without-progress"`
	// Pointer preserves an explicit zero, which disables the per-attempt
	// circuit breaker for teams that have a justified long-context workflow.
	MaxTokensPerAttempt *int `yaml:"max-tokens-per-attempt"`
}

// rawWorkerMemoryPolicy is the YAML-facing representation of a per-worker
// memory policy. It uses pointer fields and string TTLs so an explicit zero
// is distinguishable from unset, and the YAML decoder never has to parse a
// time.Duration directly.
type rawWorkerMemoryPolicy struct {
	Mode          string `yaml:"mode"`
	AutoRecall    *bool  `yaml:"auto-recall"`
	AutoSave      *bool  `yaml:"auto-save"`
	MaxItems      *int   `yaml:"max-items"`
	MaxTokens     *int   `yaml:"max-tokens"`
	SessionTTL    string `yaml:"session-ttl"`
	PersistentTTL string `yaml:"persistent-ttl"`
}

// isSet returns true when the raw policy has at least one field explicitly
// set in the YAML source.
func (r rawWorkerMemoryPolicy) isSet() bool {
	return r.Mode != "" || r.AutoRecall != nil || r.AutoSave != nil ||
		r.MaxItems != nil || r.MaxTokens != nil || r.SessionTTL != "" || r.PersistentTTL != ""
}

func parseAllowedPaths(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	switch v := raw.(type) {
	case string:
		return parseCommaList(v)
	case []interface{}:
		var result []string
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				result = append(result, s)
			}
		}
		return result
	default:
		return nil
	}
}

func parseCommaList(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

func anyToStrList(v any) []string {
	if v == nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return parseCommaList(s)
	}
	if slice, ok := v.([]any); ok {
		var result []string
		for _, item := range slice {
			if s, ok := item.(string); ok {
				result = append(result, s)
			}
		}
		return result
	}
	return nil
}

func parseAllowedTools(raw interface{}) []string {
	if raw == nil {
		return nil
	}
	// Support nested structure: tools: allowed: [bash, view]
	if m, ok := raw.(map[string]interface{}); ok {
		if allowed, exists := m["allowed"]; exists {
			return anyToStrList(allowed)
		}
	}
	// Support direct list/string: tools: [bash, view]
	return anyToStrList(raw)
}

func parseDeniedTools(raw interface{}) []string {
	if m, ok := raw.(map[string]interface{}); ok {
		if denied, exists := m["denied"]; exists {
			return anyToStrList(denied)
		}
	}
	return nil
}

func anyToStr(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

func anyToInt(v any, fallback int) int {
	if v == nil {
		return fallback
	}
	if n, ok := v.(int); ok {
		return n
	}
	if s, ok := v.(string); ok {
		var n int
		if _, err := fmt.Sscanf(s, "%d", &n); err == nil {
			return n
		}
	}
	return fallback
}

func expandAllowedPaths(paths []string) []string {
	var result []string
	for _, p := range paths {
		expanded := os.ExpandEnv(p)
		if strings.HasPrefix(expanded, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(home, expanded[1:])
			}
		}
		abs, err := filepath.Abs(expanded)
		if err == nil {
			result = append(result, abs)
		}
	}
	return result
}

func parseSimpleYAML(data string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, ":")
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		if len(val) >= 2 &&
			((val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result
}

func agentFrontmatterFromSimple(m map[string]string) agentFrontmatter {
	var fm agentFrontmatter
	fm.Name = m["name"]
	fm.Description = m["description"]
	fm.Role = m["role"]
	fm.Tools = m["tools"]
	fm.Skills = m["skills"]
	fm.Model = m["model"]
	fm.Temperature = m["temperature"]
	fm.MaxTokens = m["max-tokens"]
	fm.TopP = m["top-p"]
	fm.TopK = m["top-k"]
	fm.ReasoningEffort = m["reasoning-effort"]
	fm.ProviderURL = m["provider-url"]
	fm.AllowedPaths = m["allowed-paths"]
	fm.RestrictedPath = m["restricted-path"]
	if v := m["no-net"]; v == "true" || v == "yes" || v == "1" {
		fm.NoNet = true
	}
	if v := m["force-mcp"]; v == "true" || v == "yes" || v == "1" {
		fm.ForceMCP = true
	}
	if v := m["timeout"]; v != "" {
		var n int64
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			fm.Timeout = n
		}
	}
	if v := m["max-retries"]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n >= 0 {
			fm.MaxRetries = n
		}
	}
	if v := m["max-steps"]; v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			fm.MaxSteps = n
		}
	}
	return fm
}

func parseAgentFile(path string, vars map[string]string) (*agent.AgentDef, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read agent file %s: %w", path, err)
	}
	text := string(raw)

	templated, err := applyTemplate(text, filepath.Base(path), vars)
	if err != nil {
		return nil, fmt.Errorf("template error in agent file %s: %w", path, err)
	}
	text = templated

	if !strings.HasPrefix(text, "---\n") {
		return nil, nil
	}
	rest := text[4:]
	idx := strings.Index(rest, "\n---\n")
	if idx < 0 {
		return nil, fmt.Errorf("agent file %s has malformed frontmatter (missing closing '---')", path)
	}

	var fm agentFrontmatter
	if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
		fmt.Fprintf(os.Stderr, "warning: YAML parse failed in %s, using fallback: %v\n", path, err)
		fm = agentFrontmatterFromSimple(yamlutil.ParseSimpleYAML(rest[:idx]))
	}
	body := strings.TrimSpace(rest[idx+5:])

	if fm.Name == "" {
		return nil, fmt.Errorf("agent file %s is missing required 'name' field in frontmatter", path)
	}

	role := fm.Role
	if role == "" {
		role = "worker"
	}

	toolsList := anyToStrList(fm.Tools)
	skillsList := anyToStrList(fm.Skills)
	toolsStr := agent.ExpandImpliedTools(strings.Join(toolsList, ","))
	skillsStr := strings.Join(skillsList, ",")
	maxRetries := anyToInt(fm.MaxRetries, -1)

	def := &agent.AgentDef{
		Name:           fm.Name,
		Description:    fm.Description,
		Tools:          toolsStr,
		Role:           role,
		System:         body,
		Capabilities:   ExtractCapabilitiesFromSystem(body),
		Skills:         skillsStr,
		Guard:          fm.Guard,
		MaxRetries:     maxRetries,
		MaxSteps:       fm.MaxSteps,
		AllowedPaths:   expandAllowedPaths(anyToStrList(fm.AllowedPaths)),
		RestrictedPath: fm.RestrictedPath,
		NoNet:          fm.NoNet,
		ForceMCP:       fm.ForceMCP,
		Shell:          fm.Shell,
		MCPTools:       fm.MCPTools,
		Generation: agent.GenerationParams{
			Model:           fm.Model,
			Temperature:     fm.Temperature,
			MaxTokens:       fm.MaxTokens,
			TopP:            fm.TopP,
			TopK:            fm.TopK,
			ReasoningEffort: fm.ReasoningEffort,
		},
		ProviderURL:   fm.ProviderURL,
		ExtraModels:   fm.ExtraModels,
		SideEffect:    fm.SideEffect,
		Recovery:      fm.Recovery,
		ReconcileTool: fm.ReconcileTool,
		MemoryID:      fm.MemoryID,
	}
	if fm.Memory.isSet() {
		def.Memory = resolveWorkerMemoryPolicy(fm.Memory, rawWorkerMemoryPolicy{}, agent.DefaultWorkerMemoryPolicy())
	} else {
		def.Memory = agent.DefaultWorkerMemoryPolicy()
	}
	if fm.Timeout > 0 {
		def.Timeout = fm.Timeout
	}
	if err := validateWorkerMemoryPolicy(def.Memory); err != nil {
		return nil, fmt.Errorf("agent %s: %w", def.Name, err)
	}
	return def, nil
}

func ExtractCapabilitiesFromSystem(system string) string {
	if system == "" {
		return ""
	}
	lines := strings.Split(system, "\n")
	var caps []string
	inList := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			inList = false
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			item := strings.TrimPrefix(strings.TrimPrefix(trimmed, "- "), "* ")
			if len(item) > 3 {
				caps = append(caps, item)
				inList = true
			}
		} else if inList && len(caps) > 0 && len(trimmed) > 3 {
			caps[len(caps)-1] += " " + trimmed
		} else {
			inList = false
		}
	}
	for i := range caps {
		caps[i] = strings.TrimSpace(caps[i])
	}
	if len(caps) == 0 {
		sentences := strings.Split(system, ". ")
		for _, s := range sentences {
			s = strings.TrimSpace(s)
			if len(s) > 10 && len(caps) < 5 {
				caps = append(caps, s)
			}
		}
	}
	var result []string
	seen := map[string]bool{}
	for _, c := range caps {
		lower := strings.ToLower(c)
		if !seen[lower] && len(c) > 5 {
			seen[lower] = true
			result = append(result, c)
		}
	}
	return strings.Join(result, "\n")
}

func parseTeamYML(teamDir string, vars map[string]string) (agent.TeamConfig, error) {
	cfg := agent.TeamConfig{
		MaxRounds:     10,
		WorkspaceDir:  "workspace",
		Timeout:       600,
		VerifyTimeout: 120,
		MaxRetries:    2,
		Reliability:   agent.DefaultReliabilityConfig(),
		Generation: agent.GenerationParams{
			Temperature: agent.DefaultTemperature,
			MaxTokens:   agent.DefaultMaxTokens,
			TopP:        agent.DefaultTopP,
		},
	}

	var data []byte
	var dataFilename string
	var found bool
	for _, name := range []string{"team.yml", "team.yaml"} {
		d, err := os.ReadFile(filepath.Join(teamDir, name))
		if err == nil {
			data = d
			dataFilename = name
			found = true
			break
		}
	}
	if !found {
		// team.yml/team.yaml is optional. Return defaults; LoadTeam will
		// fall back to using the directory basename as the team name.
		return cfg, nil
	}

	text := string(data)
	templated, err := applyTemplate(text, dataFilename, vars)
	if err != nil {
		return cfg, fmt.Errorf("template error in team config: %w", err)
	}
	data = []byte(templated)

	var yc teamConfigYAML
	if err := yaml.Unmarshal(data, &yc); err != nil {
		return cfg, fmt.Errorf("failed to parse team config: %w", err)
	}

	if yc.Name != "" {
		cfg.Name = yc.Name
	}
	if yc.Description != "" {
		cfg.Description = yc.Description
	}
	if yc.MaxRounds > 0 {
		cfg.MaxRounds = yc.MaxRounds
	}
	if yc.Workspace != "" {
		cfg.WorkspaceDir = yc.Workspace
	}
	if yc.Timeout > 0 {
		cfg.Timeout = yc.Timeout
	}
	if yc.VerifyTimeout > 0 {
		cfg.VerifyTimeout = yc.VerifyTimeout
	}
	if yc.MaxRetries >= 0 {
		cfg.MaxRetries = yc.MaxRetries
	}
	if yc.MaxSteps > 0 {
		cfg.MaxSteps = yc.MaxSteps
	}
	cfg.Generation = agent.GenerationParams{
		Model:           yc.Model,
		Temperature:     agent.DefaultTemperature,
		MaxTokens:       agent.DefaultMaxTokens,
		TopP:            agent.DefaultTopP,
		TopK:            yc.TopK,
		ReasoningEffort: yc.ReasoningEffort,
	}
	if yc.MaxTokens != "" {
		cfg.Generation.MaxTokens = yc.MaxTokens
	}
	if yc.Temperature != "" {
		cfg.Generation.Temperature = yc.Temperature
	}
	if yc.TopP != "" {
		cfg.Generation.TopP = yc.TopP
	}
	if yc.Skills != "" {
		cfg.Skills = yc.Skills
	}
	if yc.SkillsExclude != "" {
		cfg.SkillsExclude = yc.SkillsExclude
	}
	if yc.ProviderURL != "" {
		cfg.ProviderURL = yc.ProviderURL
	}
	if yc.ProviderAPIKey != "" {
		cfg.ProviderAPIKey = yc.ProviderAPIKey
	}
	if len(yc.Providers) > 0 {
		cfg.Providers = yc.Providers
	}
	if len(yc.ModelList) > 0 {
		cfg.ModelList = yc.ModelList
	}
	if yc.SidecarModel != "" {
		cfg.SidecarModel = yc.SidecarModel
	}
	if yc.PlanReviewerModel != "" {
		cfg.PlanReviewerModel = yc.PlanReviewerModel
	}
	if yc.GuardModel != "" {
		cfg.GuardModel = yc.GuardModel
	}
	if yc.JudgeModel != "" {
		cfg.JudgeModel = yc.JudgeModel
	}
	if yc.MaxConcurrent > 0 {
		cfg.MaxConcurrent = yc.MaxConcurrent
	}
	if yc.MaxCoordinatorTurns > 0 {
		cfg.MaxCoordinatorTurns = yc.MaxCoordinatorTurns
	}
	if yc.EscalateOnRetry {
		cfg.EscalateOnRetry = true
	}
	if yc.Notify.Enabled() {
		cfg.Notify = yc.Notify
	}
	if paths := parseAllowedPaths(yc.AllowedPaths); len(paths) > 0 {
		cfg.AllowedPaths = expandAllowedPaths(paths)
	}
	if yc.RestrictedPath != "" {
		cfg.RestrictedPath = os.ExpandEnv(yc.RestrictedPath)
		if strings.HasPrefix(cfg.RestrictedPath, "~") {
			if home, err := os.UserHomeDir(); err == nil {
				cfg.RestrictedPath = filepath.Join(home, cfg.RestrictedPath[1:])
			}
		}
	}
	if yc.NoNet {
		cfg.NoNet = true
	}
	if yc.ForceMCP {
		cfg.ForceMCP = true
	}
	if yc.ProjectContext {
		cfg.ProjectContext = true
	}
	if yc.Unattended {
		cfg.Unattended = true
	}
	if yc.AutoApprove {
		cfg.AutoApprove = true
	}
	if yc.MaxWallClock > 0 {
		cfg.MaxWallClock = yc.MaxWallClock
	}
	if yc.MaxTotalTokens > 0 {
		cfg.MaxTotalTokens = yc.MaxTotalTokens
	}
	if yc.Acceptance != nil {
		switch v := yc.Acceptance.(type) {
		case string:
			if v != "" {
				cfg.Acceptance = v
				cfg.AcceptanceSpec = &agent.AcceptanceSpec{Commands: []string{v}}
			} else {
				cfg.AcceptanceSpec = &agent.AcceptanceSpec{}
			}
		case map[string]interface{}, map[interface{}]interface{}:
			rawBytes, err := yaml.Marshal(v)
			if err != nil {
				return cfg, fmt.Errorf("failed to marshal acceptance config: %w", err)
			}
			var spec agent.AcceptanceSpec
			dec := yaml.NewDecoder(bytes.NewReader(rawBytes))
			dec.KnownFields(true)
			if err := dec.Decode(&spec); err != nil {
				return cfg, fmt.Errorf("invalid acceptance spec format: %w", err)
			}
			cfg.AcceptanceSpec = &spec
			if spec.Mode != "" {
				switch strings.ToLower(strings.TrimSpace(spec.Mode)) {
				case string(AcceptanceAdvisory), string(AcceptanceBlocking):
					cfg.AcceptanceMode = strings.ToLower(strings.TrimSpace(spec.Mode))
				default:
					mode, err := ParseGoalMode(spec.Mode)
					if err != nil {
						return cfg, fmt.Errorf("invalid acceptance goal mode: %w", err)
					}
					// An explicit team-level goal-mode remains authoritative; otherwise
					// preserve the mode embedded in the acceptance contract.
					if cfg.GoalMode == "" {
						cfg.GoalMode = string(mode)
					}
				}
			}
			if len(spec.Commands) > 0 {
				cfg.Acceptance = spec.Commands[0]
			}
		default:
			return cfg, fmt.Errorf("unsupported acceptance config type: %T", v)
		}
	}
	if yc.Rollback != "" {
		cfg.Rollback = yc.Rollback
	}
	if yc.ExecutionProfile != "" {
		cfg.ExecutionProfile = yc.ExecutionProfile
	}
	if yc.GoalMode != "" {
		gm, err := ParseGoalMode(yc.GoalMode)
		if err != nil {
			return cfg, fmt.Errorf("invalid team configuration: %w", err)
		}
		cfg.GoalMode = string(gm)
	}
	if yc.WorkerMemory.isSet() {
		cfg.WorkerMemory = resolveWorkerMemoryPolicy(yc.WorkerMemory, rawWorkerMemoryPolicy{}, agent.DefaultWorkerMemoryPolicy())
		if err := validateWorkerMemoryPolicy(cfg.WorkerMemory); err != nil {
			return cfg, fmt.Errorf("invalid worker-memory config: %w", err)
		}
	} else {
		cfg.WorkerMemory = agent.DefaultWorkerMemoryPolicy()
	}
	effectiveGoalMode, err := ResolveEffectiveGoalMode(cfg.GoalMode, cfg.ExecutionProfile)
	if err != nil {
		return cfg, fmt.Errorf("invalid effective goal mode: %w", err)
	}
	if err := ValidateAcceptanceSpec(cfg.AcceptanceSpec, string(effectiveGoalMode)); err != nil {
		return cfg, err
	}
	cfg.Reliability = agent.DefaultReliabilityConfig()
	if strings.TrimSpace(yc.Reliability.Rollout) != "" {
		cfg.Reliability.Rollout = strings.TrimSpace(yc.Reliability.Rollout)
	}
	if yc.Reliability.MaxDiagnosticTasksWithoutProgress > 0 {
		cfg.Reliability.MaxDiagnosticTasksWithoutProgress = yc.Reliability.MaxDiagnosticTasksWithoutProgress
	}
	if yc.Reliability.MaxSameFailureFingerprint > 0 {
		cfg.Reliability.MaxSameFailureFingerprint = yc.Reliability.MaxSameFailureFingerprint
	}
	if yc.Reliability.MaxRepairsPerCriterion > 0 {
		cfg.Reliability.MaxRepairsPerCriterion = yc.Reliability.MaxRepairsPerCriterion
	}
	if yc.Reliability.MaxSystemicFailureTasks != nil {
		// An explicit YAML value (including 0) overrides the default.
		// 0 disables systemic counting entirely. MaxSystemicFailureTasksSet
		// records that the value was explicitly set so reliabilityConfig()
		// honors the zero override instead of restoring the default. Refs:
		// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
		cfg.Reliability.MaxSystemicFailureTasks = *yc.Reliability.MaxSystemicFailureTasks
		cfg.Reliability.MaxSystemicFailureTasksSet = true
	}
	// No-progress budget (§8.1, WP-12): explicit value (including 0 to
	// disable one counter) overrides the default; unset restores the
	// default. Mirrors the MaxSystemicFailureTasks pointer pattern.
	if yc.Reliability.MaxTokensWithoutProgress != nil {
		cfg.Reliability.MaxTokensWithoutProgress = *yc.Reliability.MaxTokensWithoutProgress
		cfg.Reliability.MaxTokensWithoutProgressSet = true
	}
	if yc.Reliability.MaxTurnsWithoutProgress != nil {
		cfg.Reliability.MaxTurnsWithoutProgress = *yc.Reliability.MaxTurnsWithoutProgress
		cfg.Reliability.MaxTurnsWithoutProgressSet = true
	}
	if yc.Reliability.MaxTasksWithoutProgress != nil {
		cfg.Reliability.MaxTasksWithoutProgress = *yc.Reliability.MaxTasksWithoutProgress
		cfg.Reliability.MaxTasksWithoutProgressSet = true
	}
	if yc.Reliability.MaxTokensPerAttempt != nil {
		cfg.Reliability.MaxTokensPerAttempt = *yc.Reliability.MaxTokensPerAttempt
		cfg.Reliability.MaxTokensPerAttemptSet = true
	}
	if yc.Reliability.WarnOnly {
		cfg.Reliability.WarnOnly = true
		cfg.Reliability.HardEnforcement = false
	} else if yc.Reliability.HardEnforcement != nil {
		cfg.Reliability.HardEnforcement = *yc.Reliability.HardEnforcement
		cfg.Reliability.WarnOnly = !cfg.Reliability.HardEnforcement
	}
	cfg.Reliability.VerifierLintMode = agent.NormalizeVerifierLintMode(yc.Reliability.VerifierLintMode)
	if yc.Shell != "" {
		cfg.Shell = yc.Shell
	}
	if len(yc.Vars) > 0 {
		cfg.Vars = yc.Vars
	}
	if yc.WorkerContextSize > 0 {
		cfg.WorkerContextSize = yc.WorkerContextSize
	}
	if tools := parseAllowedTools(yc.ToolsAllowed); len(tools) > 0 {
		cfg.ToolsAllowed = strings.Split(agent.ExpandImpliedTools(strings.Join(tools, ",")), ",")
	}
	if tools := parseDeniedTools(yc.ToolsAllowed); len(tools) > 0 {
		cfg.ToolsDenied = tools
	}
	if len(yc.Delegation.AllowedWorkers) > 0 {
		cfg.Delegation.AllowedWorkers = yc.Delegation.AllowedWorkers
	}
	if len(yc.Delegation.InitialBatch.Agents) > 0 {
		cfg.Delegation.InitialBatch = yc.Delegation.InitialBatch.Agents
		cfg.Delegation.RequireExactInitialBatch = yc.Delegation.InitialBatch.Exact
	}
	if yc.Delegation.InitialBatch.FirstTool != "" {
		cfg.Delegation.InitialCoordinatorTool = yc.Delegation.InitialBatch.FirstTool
	}
	if yc.Delegation.InitialBatch.BindContracts {
		cfg.Delegation.BindInitialTaskContracts = true
	}
	if len(yc.Delegation.NoRedispatchAfterSuccess) > 0 {
		cfg.Delegation.NoRedispatchAfterSuccess = yc.Delegation.NoRedispatchAfterSuccess
	}
	if yc.Delegation.ForbidContextFiles {
		cfg.Delegation.ForbidContextFiles = true
	}
	if len(yc.Delegation.TaskGoalInvariants) > 0 {
		cfg.Delegation.TaskGoalInvariants = yc.Delegation.TaskGoalInvariants
	}
	if len(yc.Preflight) > 0 {
		cfg.Preflight = yc.Preflight
	}

	return cfg, nil
}

// loadTeamContractTasks reads optional static task contracts from team YAML.
// They are intentionally kept on TeamSession for preflight tooling only: task
// dispatch remains coordinator-driven and is not changed by declaring them.
func loadTeamContractTasks(teamDir string, vars map[string]string) ([]TaskDef, error) {
	for _, name := range []string{"team.yml", "team.yaml"} {
		data, err := os.ReadFile(filepath.Join(teamDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read team task contracts: %w", err)
		}
		text, err := applyTemplate(string(data), name, vars)
		if err != nil {
			return nil, fmt.Errorf("template team task contracts: %w", err)
		}
		var config struct {
			Tasks []TaskDef `yaml:"tasks"`
		}
		if err := yaml.Unmarshal([]byte(text), &config); err != nil {
			return nil, fmt.Errorf("parse team task contracts: %w", err)
		}
		return config.Tasks, nil
	}
	return nil, nil
}

func LoadTeam(teamDir string, vars map[string]string, forcedSkills []string) (*TeamSession, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("invalid team directory: %w", err)
	}

	cfg, err := parseTeamYML(absDir, vars)
	if err != nil {
		return nil, err
	}
	effectiveGoalMode, err := ResolveEffectiveGoalMode(cfg.GoalMode, cfg.ExecutionProfile)
	if err != nil {
		return nil, fmt.Errorf("invalid effective goal mode: %w", err)
	}
	if err := ValidateAcceptanceSpec(cfg.AcceptanceSpec, string(effectiveGoalMode)); err != nil {
		return nil, err
	}
	if cfg.Name == "" {
		cfg.Name = filepath.Base(absDir)
	}
	contractTasks, err := loadTeamContractTasks(absDir, vars)
	if err != nil {
		return nil, err
	}

	// Build template vars: CLI --var (string) + team.yaml vars (interface{}) + built-in
	// CLI --var takes precedence over team.yaml vars for the same key
	templateVars := make(map[string]string)
	// Copy team.yaml vars first (lower priority)
	for k, v := range cfg.Vars {
		templateVars[k] = fmt.Sprintf("%v", v)
	}
	// Copy CLI --var (higher priority, can override)
	for k, v := range vars {
		templateVars[k] = v
	}

	var workspace string
	if filepath.IsAbs(cfg.WorkspaceDir) {
		workspace = cfg.WorkspaceDir
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get working directory: %w", err)
		}
		workspace = filepath.Join(cwd, cfg.WorkspaceDir)
	}
	session := &TeamSession{
		Config:        cfg,
		Dir:           absDir,
		Workspace:     workspace,
		Agents:        make(map[string]*agent.AgentDef),
		MCPServers:    make(map[string]mcp.MCPServerConfig),
		ContractTasks: contractTasks,
	}

	// Inject built-in vars BEFORE loading agents
	if _, ok := templateVars["TEAM_NAME"]; !ok && cfg.Name != "" {
		templateVars["TEAM_NAME"] = cfg.Name
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read team directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		path := filepath.Join(absDir, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil || resolved == path {
				continue
			}
		}
		def, err := parseAgentFile(path, templateVars)
		if err != nil {
			return nil, err
		}
		if def == nil {
			continue
		}
		fileAlias := strings.TrimSuffix(entry.Name(), ".md")
		def.FileAlias = fileAlias
		fileKey := strings.ToLower(fileAlias)
		nameKey := strings.ToLower(def.Name)
		if _, exists := session.Agents[fileKey]; !exists {
			session.Agents[fileKey] = def
		}
		if nameKey != fileKey {
			if _, exists := session.Agents[nameKey]; !exists {
				session.Agents[nameKey] = def
			}
		}
	}

	builtInHelper := &agent.AgentDef{
		Name:        "Helper",
		FileAlias:   "helper",
		Description: "Versatile worker for text processing, string comparison, file I/O, calculations, and miscellaneous tasks",
		Role:        "worker",
		Tools:       "view,write,edit,multiedit,grep,glob,ls,random,math",
		System:      "You are a versatile helper agent. You handle text processing, string comparisons, file reading/writing, mathematical calculations via the math tool, and miscellaneous tasks that don't require specialized domain knowledge. Be thorough and precise.",
		MaxRetries:  -1,
		Generation:  cfg.Generation,
		ProviderURL: cfg.ProviderURL,
		Memory:      agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff},
	}
	session.Agents["helper"] = builtInHelper

	if len(session.Agents) == 0 {
		return nil, fmt.Errorf("no valid agent .md files found in %s", absDir)
	}
	if findings := ValidateTeamTaskContracts(session); len(findings) > 0 {
		return nil, fmt.Errorf("team task contract validation failed: %s", strings.Join(sortedContractFindingMessages(findings), "; "))
	}

	// Resolve per-worker memory: apply team defaults to agents that didn't
	// override, normalize memory-id (fallback to agent name), and detect
	// duplicate memory-ids within the same team.
	if err := resolveAndValidateWorkerMemory(session); err != nil {
		return nil, err
	}

	// Inject built-in vars AFTER agents loaded (AGENT_COUNT, AGENT_NAMES)
	workerNames := make([]string, 0)
	for _, def := range session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" {
			workerNames = append(workerNames, def.Name)
		}
	}
	if _, ok := templateVars["AGENT_COUNT"]; !ok {
		templateVars["AGENT_COUNT"] = fmt.Sprintf("%d", len(workerNames))
	}
	if _, ok := templateVars["AGENT_NAMES"]; !ok {
		templateVars["AGENT_NAMES"] = strings.Join(workerNames, ", ")
	}
	// Store template vars in session config for later use
	interfaceVars := make(map[string]interface{})
	for k, v := range templateVars {
		interfaceVars[k] = v
	}
	// session.Config was captured by value at session creation, so mutating
	// cfg.Vars here would leave session.Config.Vars as the pre-population
	// empty map. Mutate the session copy directly so readers of
	// session.Config.Vars see the populated template vars.
	session.Config.Vars = interfaceVars

	skillDirs := []string{filepath.Join(absDir, "skills")}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		skillDirs = append(skillDirs, filepath.Join(cwd, ".agents", "skills"))
	}
	skillDirs = append(skillDirs, filepath.Join(os.Getenv("HOME"), ".agents", "skills"))

	allSkills := skill.DiscoverSkills(skillDirs, false)

	includeSkills := skill.ParseSkillList(session.Config.Skills)
	excludeSkills := skill.ParseSkillList(session.Config.SkillsExclude)
	session.Skills = skill.FilterSkills(allSkills, includeSkills, excludeSkills)

	if len(forcedSkills) > 0 {
		forcedSet := map[string]bool{}
		for _, name := range forcedSkills {
			forcedSet[strings.ToLower(strings.TrimSpace(name))] = true
		}
		// Build set of already-added skill names for O(1) duplicate check
		existingSet := map[string]bool{}
		for _, s := range session.Skills {
			existingSet[strings.ToLower(s.Name)] = true
		}
		// Warn about unmatched forced skill names
		knownSkills := map[string]bool{}
		for _, sk := range allSkills {
			knownSkills[strings.ToLower(sk.Name)] = true
		}
		for _, name := range forcedSkills {
			trimmed := strings.TrimSpace(name)
			if trimmed != "" && !knownSkills[strings.ToLower(trimmed)] {
				fmt.Fprintf(os.Stderr, "warning: forced skill %q not found in discovered skills\n", trimmed)
			}
		}
		for _, sk := range allSkills {
			lowerName := strings.ToLower(sk.Name)
			if forcedSet[lowerName] && !existingSet[lowerName] {
				session.Skills = append(session.Skills, sk)
				existingSet[lowerName] = true
			}
		}
	}
	session.Skills = skill.ExpandSkillDependenciesForSet(session.Skills, allSkills, excludeSkills)

	return session, nil
}

// normalizeMemoryID lowercases and trims the input, then validates it
// contains only safe characters (alphanumeric, hyphen, underscore). It
// rejects path separators and other characters that could break scope
// queries or filesystem paths. Returns the normalized ID or an error.
func normalizeMemoryID(name string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(name))
	if id == "" {
		return "", nil
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return "", fmt.Errorf("memory-id %q contains invalid character %q; only lowercase letters, digits, hyphen, and underscore are allowed", name, string(r))
		}
	}
	return id, nil
}

// resolveWorkerMemoryPolicy merges a raw agent-level policy, a raw team-level
// default, and the built-in default into a single resolved policy. Agent
// fields take precedence over team fields, which take precedence over the
// built-in default.
func resolveWorkerMemoryPolicy(agentRaw, teamRaw rawWorkerMemoryPolicy, builtIn agent.WorkerMemoryPolicy) agent.WorkerMemoryPolicy {
	p := builtIn
	// Apply team defaults first (lower priority).
	if teamRaw.Mode != "" {
		p.Mode = agent.WorkerMemoryMode(teamRaw.Mode)
	}
	if teamRaw.AutoRecall != nil {
		p.AutoRecall = *teamRaw.AutoRecall
	}
	if teamRaw.AutoSave != nil {
		p.AutoSave = *teamRaw.AutoSave
	}
	if teamRaw.MaxItems != nil {
		p.MaxItems = *teamRaw.MaxItems
	}
	if teamRaw.MaxTokens != nil {
		p.MaxTokens = *teamRaw.MaxTokens
	}
	if teamRaw.SessionTTL != "" {
		p.SessionTTL = teamRaw.SessionTTL
	}
	if teamRaw.PersistentTTL != "" {
		p.PersistentTTL = teamRaw.PersistentTTL
	}
	// Apply agent overrides (higher priority).
	if agentRaw.Mode != "" {
		p.Mode = agent.WorkerMemoryMode(agentRaw.Mode)
	}
	if agentRaw.AutoRecall != nil {
		p.AutoRecall = *agentRaw.AutoRecall
	}
	if agentRaw.AutoSave != nil {
		p.AutoSave = *agentRaw.AutoSave
	}
	if agentRaw.MaxItems != nil {
		p.MaxItems = *agentRaw.MaxItems
	}
	if agentRaw.MaxTokens != nil {
		p.MaxTokens = *agentRaw.MaxTokens
	}
	if agentRaw.SessionTTL != "" {
		p.SessionTTL = agentRaw.SessionTTL
	}
	if agentRaw.PersistentTTL != "" {
		p.PersistentTTL = agentRaw.PersistentTTL
	}
	return p
}

// validateWorkerMemoryPolicy checks that the resolved policy has a valid mode,
// non-negative limits, and parseable TTL strings.
func validateWorkerMemoryPolicy(p agent.WorkerMemoryPolicy) error {
	switch p.Mode {
	case agent.WorkerMemoryOff, agent.WorkerMemorySession, agent.WorkerMemoryPersistent:
		// valid
	default:
		return fmt.Errorf("invalid memory mode %q: must be off, session, or persistent", p.Mode)
	}
	if p.MaxItems < 0 {
		return fmt.Errorf("max-items must be non-negative, got %d", p.MaxItems)
	}
	if p.MaxTokens < 0 {
		return fmt.Errorf("max-tokens must be non-negative, got %d", p.MaxTokens)
	}
	if p.SessionTTL != "" && p.SessionTTL != "0" {
		if _, err := time.ParseDuration(p.SessionTTL); err != nil {
			return fmt.Errorf("invalid session-ttl %q: %w", p.SessionTTL, err)
		}
	}
	if p.PersistentTTL != "" && p.PersistentTTL != "0" {
		if _, err := time.ParseDuration(p.PersistentTTL); err != nil {
			return fmt.Errorf("invalid persistent-ttl %q: %w", p.PersistentTTL, err)
		}
	}
	return nil
}

// resolveAndValidateWorkerMemory applies team defaults to agents that didn't
// override their memory policy, normalizes each agent's memory-id (falling
// back to the normalized agent name), and detects duplicate memory-ids
// within the same team.
func resolveAndValidateWorkerMemory(session *TeamSession) error {
	teamDefaults := session.Config.WorkerMemory
	seenIDs := map[string]string{} // memory-id → agent name (for duplicate detection)
	for _, def := range session.Agents {
		// If the agent's policy is still the built-in default (i.e. the agent
		// frontmatter didn't set a memory block), apply team defaults.
		if !agentMemoryOverridden(def.Memory) {
			def.Memory = teamDefaults
		}
		// Normalize memory-id: use explicit memory-id if set, otherwise
		// fall back to the normalized agent name.
		id := def.MemoryID
		if id == "" {
			id = def.Name
		}
		normalized, err := normalizeMemoryID(id)
		if err != nil {
			return fmt.Errorf("agent %s: %w", def.Name, err)
		}
		def.MemoryID = normalized
		// Duplicate detection: only check agents with mode != off, since
		// off-mode agents don't use memory and their memory-id is irrelevant.
		if def.Memory.Mode != agent.WorkerMemoryOff && normalized != "" {
			if existing, dup := seenIDs[normalized]; dup {
				return fmt.Errorf("duplicate memory-id %q: agents %q and %q share the same identity", normalized, existing, def.Name)
			}
			seenIDs[normalized] = def.Name
		}
	}
	return nil
}

// agentMemoryOverridden returns true when the policy differs from the
// built-in default in any field, indicating the agent frontmatter set a
// memory block.
func agentMemoryOverridden(p agent.WorkerMemoryPolicy) bool {
	d := agent.DefaultWorkerMemoryPolicy()
	return p.Mode != d.Mode || p.AutoRecall != d.AutoRecall || p.AutoSave != d.AutoSave ||
		p.MaxItems != d.MaxItems || p.MaxTokens != d.MaxTokens ||
		p.SessionTTL != d.SessionTTL || p.PersistentTTL != d.PersistentTTL
}
