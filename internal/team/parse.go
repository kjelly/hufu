package team

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/notify"
	"github.com/kjelly/hufu/internal/skill"
	"github.com/kjelly/hufu/internal/team/preset"
	"github.com/kjelly/hufu/internal/yamlutil"
)

type TeamSession struct {
	Config        agent.TeamConfig
	Dir           string
	Workspace     string
	Agents        map[string]*agent.AgentDef
	MCPServers    map[string]mcp.MCPServerConfig
	Skills        []*skill.SkillDef
	ContractTasks []TaskDef // Optional static task contracts used by preflight tooling and policy binding.
	// ProviderRegistry is injected by the host at load time. It stays outside
	// persistent session data so a resumed workflow rebinds only to providers
	// explicitly registered by the current host process.
	ProviderRegistry *ProviderRegistry
}

type agentFrontmatter struct {
	Name            string                         `yaml:"name"`
	Description     string                         `yaml:"description"`
	Role            string                         `yaml:"role"`
	Preset          string                         `yaml:"preset"`
	Tools           any                            `yaml:"tools"`  // string, []string, or {allowed: [...], denied: [...]}
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
	Requirements    agent.ContractRequirements     `yaml:"requires"`
	SideEffect      string                         `yaml:"side_effect"`
	Recovery        string                         `yaml:"recovery"`
	ReconcileTool   string                         `yaml:"reconcile-tool"`
	MemoryID        string                         `yaml:"memory-id"`
	Memory          rawWorkerMemoryPolicy          `yaml:"memory"`
}

type teamConfigYAML struct {
	Name                     string `yaml:"name"`
	Description              string `yaml:"description"`
	MaxRounds                int    `yaml:"max-rounds"`
	MinimumCoordinatorRounds int    `yaml:"minimum-coordinator-rounds"`
	MaxSteps                 int    `yaml:"max-steps"`
	Workspace                string `yaml:"workspace"`
	Timeout                  int64  `yaml:"timeout"`
	VerifyTimeout            int64  `yaml:"verify-timeout"`
	// MaxRetries is a pointer so an omitted key (built-in default) can be
	// distinguished from an explicit "max-retries: 0" override; the zero
	// value of a plain int is indistinguishable from an explicit 0.
	MaxRetries           *int                             `yaml:"max-retries"`
	AutoReport           bool                             `yaml:"auto-report"`
	AllowFreeTextResults bool                             `yaml:"allow-free-text-results"`
	Model                string                           `yaml:"model"`
	ContextWindow        int                              `yaml:"context-window"`
	Temperature          string                           `yaml:"temperature"`
	MaxTokens            string                           `yaml:"max-tokens"`
	TopP                 string                           `yaml:"top-p"`
	TopK                 string                           `yaml:"top-k"`
	ReasoningEffort      string                           `yaml:"reasoning-effort"`
	Skills               string                           `yaml:"skills"`
	SkillsExclude        string                           `yaml:"skills-exclude"`
	ProviderURL          string                           `yaml:"provider-url"`
	ProviderAPIKey       string                           `yaml:"provider-api-key"`
	Providers            map[string]config.ProviderConfig `yaml:"providers"`
	ModelList            []config.ModelEntry              `yaml:"model-list"`
	SidecarModel         string                           `yaml:"sidecar-model"`
	GuardModel           string                           `yaml:"guard-model"`
	JudgeModel           string                           `yaml:"judge-model"`
	PlanReviewerModel    string                           `yaml:"plan-reviewer-model"`
	MaxConcurrent        int                              `yaml:"max-concurrent"`
	StallThreshold       string                           `yaml:"stall-threshold"`
	MaxCoordinatorTurns  int                              `yaml:"max-coordinator-turns"`
	EscalateOnRetry      bool                             `yaml:"escalate-on-retry"`
	AutoSkills           bool                             `yaml:"auto-skills"`
	Notify               notify.NotifyConfig              `yaml:"notify"`
	AllowedPaths         interface{}                      `yaml:"allowed-paths"`
	RestrictedPath       string                           `yaml:"restricted-path"`
	NoNet                bool                             `yaml:"no-net"`
	ForceMCP             bool                             `yaml:"force-mcp"`
	ProjectContext       bool                             `yaml:"project-context"`
	Shell                string                           `yaml:"shell"`
	Vars                 map[string]interface{}           `yaml:"vars"`
	// WorkerContextSize is a token budget, not a character count (spec.md
	// item 7); the YAML key is kept as-is for backward compatibility.
	WorkerContextSize int                                   `yaml:"worker-context-size"`
	ToolsAllowed      interface{}                           `yaml:"tools"` // tools.allowed/tools.denied in YAML - string or []string
	Requirements      agent.ContractRequirements            `yaml:"requires"`
	Delegation        rawDelegationPolicy                   `yaml:"delegation"`
	Preflight         []agent.CapabilityRequirement         `yaml:"preflight"`
	Workflow          agent.WorkflowConfig                  `yaml:"workflow"`
	Policies          agent.WorkflowPolicies                `yaml:"policies"`
	Capabilities      agent.CapabilityConfig                `yaml:"capabilities"`
	Verification      agent.VerificationConfig              `yaml:"verification"`
	Retry             agent.RetryConfig                     `yaml:"retry"`
	ActionProviders   map[string]agent.ActionProviderConfig `yaml:"action-providers"`
	// Kept as an opaque map here because MCP server loading is owned by the
	// session layer; declaring the key preserves this long-standing manifest
	// field while strict validation still rejects unknown top-level keys.
	MCPServers       map[string]interface{}  `yaml:"mcp-servers"`
	Unattended       bool                    `yaml:"unattended"`
	AutoApprove      bool                    `yaml:"auto-approve"`
	MaxWallClock     int64                   `yaml:"max-duration"`
	MaxTotalTokens   int64                   `yaml:"max-total-tokens"`
	Acceptance       interface{}             `yaml:"acceptance"`
	Rollback         string                  `yaml:"rollback"`
	ExecutionProfile string                  `yaml:"execution-profile"`
	GoalMode         string                  `yaml:"goal-mode"`
	Reliability      rawReliabilityConfig    `yaml:"reliability"`
	WorkerMemory     rawWorkerMemoryPolicy   `yaml:"worker-memory"`
	MemoryLearning   rawMemoryLearningPolicy `yaml:"memory-learning"`
	Compaction       rawCompactionPolicy     `yaml:"compaction"`
	Tasks            []TaskDef               `yaml:"tasks"`
	Advanced         rawAdvancedSection      `yaml:"advanced"`
}

// rawAdvancedSection is the authoring-time `advanced:` namespace (spec.md
// Specification 01 §9): a purely optional alternative spelling for these
// same fields at the top level. mergeAdvancedNamespace folds it back into
// the legacy top-level fields before any other logic in this file runs, so
// nothing downstream needs to know this namespace exists. Tasks is
// declared here only so `advanced: tasks:` decodes under this file's
// strict teamConfigYAML — like the legacy top-level Tasks field above, it
// is not itself consumed here; loadTeamContractTasks re-parses tasks
// (legacy and advanced) independently, since it is the actual source of
// session.ContractTasks.
type rawAdvancedSection struct {
	Workflow     agent.WorkflowConfig     `yaml:"workflow"`
	Tasks        []TaskDef                `yaml:"tasks"`
	Reliability  rawReliabilityConfig     `yaml:"reliability"`
	Verification agent.VerificationConfig `yaml:"verification"`
	Retry        agent.RetryConfig        `yaml:"retry"`
}

// mergeAdvancedNamespace normalizes yc.Advanced's fields into yc's
// legacy top-level fields. A field defined in both forms fails closed
// (Specification 01 §9 "Conflict Rule": "Hufu must not silently choose
// one") rather than picking a winner.
func mergeAdvancedNamespace(yc *teamConfigYAML) error {
	if len(yc.Advanced.Workflow.Phases) > 0 {
		if len(yc.Workflow.Phases) > 0 {
			return errors.New("\"workflow\" is defined both at the top level and under \"advanced\"; remove one")
		}
		yc.Workflow = yc.Advanced.Workflow
	}
	if !reflect.DeepEqual(yc.Advanced.Reliability, rawReliabilityConfig{}) {
		if !reflect.DeepEqual(yc.Reliability, rawReliabilityConfig{}) {
			return errors.New("\"reliability\" is defined both at the top level and under \"advanced\"; remove one")
		}
		yc.Reliability = yc.Advanced.Reliability
	}
	if !reflect.DeepEqual(yc.Advanced.Verification, agent.VerificationConfig{}) {
		if !reflect.DeepEqual(yc.Verification, agent.VerificationConfig{}) {
			return errors.New("\"verification\" is defined both at the top level and under \"advanced\"; remove one")
		}
		yc.Verification = yc.Advanced.Verification
	}
	if !reflect.DeepEqual(yc.Advanced.Retry, agent.RetryConfig{}) {
		if !reflect.DeepEqual(yc.Retry, agent.RetryConfig{}) {
			return errors.New("\"retry\" is defined both at the top level and under \"advanced\"; remove one")
		}
		yc.Retry = yc.Advanced.Retry
	}
	return nil
}

type rawCompactionPolicy struct {
	MaxHistoryMessages          *int `yaml:"max-history-messages"`
	RetainHistoryMessages       *int `yaml:"retain-history-messages"`
	VerifiedHistoryTargetTokens *int `yaml:"verified-history-target-tokens"`
	ToolOutputMaxBytes          *int `yaml:"tool-output-max-bytes"`
	ToolOutputMaxRunes          *int `yaml:"tool-output-max-runes"`
	ToolOutputMaxTokens         *int `yaml:"tool-output-max-tokens"`
	DiagnosticMaxLines          *int `yaml:"diagnostic-max-lines"`
	DiagnosticMaxTokens         *int `yaml:"diagnostic-max-tokens"`
}

func (r rawCompactionPolicy) apply(base agent.CompactionPolicy) (agent.CompactionPolicy, error) {
	p := base
	if r.MaxHistoryMessages != nil {
		p.MaxHistoryMessages = *r.MaxHistoryMessages
	}
	if r.RetainHistoryMessages != nil {
		p.RetainHistoryMessages = *r.RetainHistoryMessages
	}
	if r.VerifiedHistoryTargetTokens != nil {
		p.VerifiedHistoryTargetTokens = *r.VerifiedHistoryTargetTokens
	}
	if r.ToolOutputMaxBytes != nil {
		p.ToolOutputMaxBytes = *r.ToolOutputMaxBytes
	}
	if r.ToolOutputMaxRunes != nil {
		p.ToolOutputMaxRunes = *r.ToolOutputMaxRunes
	}
	if r.ToolOutputMaxTokens != nil {
		p.ToolOutputMaxTokens = *r.ToolOutputMaxTokens
	}
	if r.DiagnosticMaxLines != nil {
		p.DiagnosticMaxLines = *r.DiagnosticMaxLines
	}
	if r.DiagnosticMaxTokens != nil {
		p.DiagnosticMaxTokens = *r.DiagnosticMaxTokens
	}
	if err := p.Validate(); err != nil {
		return agent.CompactionPolicy{}, fmt.Errorf("invalid compaction config: %w", err)
	}
	return p, nil
}

type rawMemoryLearningPolicy struct {
	Mode                string   `yaml:"mode"`
	PolicyVersion       string   `yaml:"policy-version"`
	PriorAlpha          *float64 `yaml:"prior-alpha"`
	PriorBeta           *float64 `yaml:"prior-beta"`
	UtilityPercentile   *float64 `yaml:"utility-percentile"`
	MaxCreditPerSignal  *float64 `yaml:"max-credit-per-signal"`
	MinConfirmedSupport *int     `yaml:"min-confirmed-support"`
	MinIndependentTasks *int     `yaml:"min-independent-tasks"`
	MaxHarmRate         *float64 `yaml:"max-harm-rate"`
}

type rawDelegationPolicy struct {
	AllowedWorkers []string `yaml:"allowed-workers"`
	InitialBatch   struct {
		Agents        []string `yaml:"agents"`
		Exact         bool     `yaml:"exact"`
		FirstTool     string   `yaml:"first-tool"`
		BindContracts bool     `yaml:"bind-contracts"`
	} `yaml:"initial-batch"`
	BindTaskGoalContracts    bool                      `yaml:"bind-task-goal-contracts"`
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

// subtractToolNames removes every name in denied from names, preserving
// order. It is the last step of preset/tool resolution so an explicit deny
// always wins, regardless of whether the tool came from an explicit
// allowlist, a preset, or implied-tool expansion (spec.md Specification 01
// §7).
func subtractToolNames(names, denied []string) []string {
	if len(denied) == 0 {
		return dedupeToolNames(names)
	}
	blocked := make(map[string]bool, len(denied))
	for _, name := range denied {
		blocked[strings.TrimSpace(name)] = true
	}
	out := make([]string, 0, len(names))
	for _, name := range dedupeToolNames(names) {
		if !blocked[name] {
			out = append(out, name)
		}
	}
	return out
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
	return parseAgentContent(raw, path, vars)
}

// inferredAgentNamePattern constrains filename-inferred agent names to a
// conservative identifier shape. It does not apply to an explicit `name:`
// frontmatter field, which remains unrestricted for backward compatibility;
// it only governs the fallback used when frontmatter omits (or is absent
// entirely, see parseAgentContent) a name.
var inferredAgentNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// agentFilenameBase returns an agent file's basename without its extension,
// e.g. "coordinator" for ".../coordinator.md".
func agentFilenameBase(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// inferAgentRoleFromFilename infers "coordinator" for coordinator.md and
// "worker" for every other filename. Unlike name inference, this never
// fails: it applies purely from the filename regardless of whether an
// explicit name is also present, so a coordinator.md with an explicit
// `name:` still infers the coordinator role.
func inferAgentRoleFromFilename(path string) string {
	if strings.EqualFold(agentFilenameBase(path), "coordinator") {
		return "coordinator"
	}
	return "worker"
}

// inferAgentNameFromFilename derives an agent's default name from its
// filename. It is only consulted when frontmatter omits (or is absent
// entirely, see parseAgentContent) an explicit name, so an unusable
// filename never blocks a file that already names itself explicitly.
func inferAgentNameFromFilename(path string) (string, error) {
	base := agentFilenameBase(path)
	if !inferredAgentNamePattern.MatchString(base) {
		return "", fmt.Errorf("agent file %s: filename %q cannot be used as an inferred agent name (must start with a letter and contain only letters, digits, '-', or '_'); add an explicit 'name' field in frontmatter", path, base)
	}
	return base, nil
}

func parseAgentContent(raw []byte, path string, vars map[string]string) (*agent.AgentDef, error) {
	text := string(raw)

	templated, err := applyTemplate(text, filepath.Base(path), vars)
	if err != nil {
		return nil, fmt.Errorf("template error in agent file %s: %w", path, err)
	}
	text = templated

	inferredRole := inferAgentRoleFromFilename(path)

	var fm agentFrontmatter
	var body string
	if strings.HasPrefix(text, "---\n") {
		rest := text[4:]
		idx := strings.Index(rest, "\n---\n")
		if idx < 0 {
			return nil, fmt.Errorf("agent file %s has malformed frontmatter (missing closing '---')", path)
		}
		if err := yaml.Unmarshal([]byte(rest[:idx]), &fm); err != nil {
			fmt.Fprintf(os.Stderr, "warning: YAML parse failed in %s, using fallback: %v\n", path, err)
			fm = agentFrontmatterFromSimple(yamlutil.ParseSimpleYAML(rest[:idx]))
		}
		body = strings.TrimSpace(rest[idx+5:])
	} else {
		// No frontmatter at all: the whole file is the system prompt, and
		// name/role are inferred entirely from the filename.
		body = strings.TrimSpace(text)
	}

	name := fm.Name
	if name == "" {
		inferredName, err := inferAgentNameFromFilename(path)
		if err != nil {
			return nil, err
		}
		name = inferredName
	}
	role := fm.Role
	if role == "" {
		role = inferredRole
	}

	toolsList := parseAllowedTools(fm.Tools)
	deniedList := parseDeniedTools(fm.Tools)
	sideEffect := fm.SideEffect

	if presetName := strings.TrimSpace(fm.Preset); presetName != "" {
		p, ok := preset.Lookup(presetName)
		if !ok {
			return nil, fmt.Errorf("agent file %s: unknown preset %q (available presets: %s)", path, presetName, strings.Join(preset.Names(), ", "))
		}
		// Preset grants merge with (never replace) any explicit allowlist;
		// explicit denial below always wins over what the preset grants.
		toolsList = dedupeToolNames(append(append([]string(nil), p.Tools...), toolsList...))
		if sideEffect == "" {
			sideEffect = string(p.SideEffect)
		}
	}

	skillsList := anyToStrList(fm.Skills)
	expanded := strings.Split(agent.ExpandImpliedTools(strings.Join(toolsList, ",")), ",")
	toolsStr := strings.Join(subtractToolNames(expanded, deniedList), ",")
	skillsStr := strings.Join(skillsList, ",")
	maxRetries := anyToInt(fm.MaxRetries, -1)

	def := &agent.AgentDef{
		Name:           name,
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
		Requirements: agent.ContractRequirements{
			Tools:       append([]string(nil), fm.Requirements.Tools...),
			Model:       fm.Requirements.Model,
			Environment: append([]string(nil), fm.Requirements.Environment...),
			Paths:       expandAllowedPaths(fm.Requirements.Paths),
			Interactive: fm.Requirements.Interactive,
			Network:     fm.Requirements.Network,
			PlanFirst:   fm.Requirements.PlanFirst,
		},
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
		SideEffect:    sideEffect,
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

// ResolveTeamTemplateVars parses team.yaml/team.yml in teamDir and builds the effective
// template variable map combining team config vars, CLI vars, and built-in TEAM_NAME.
func ResolveTeamTemplateVars(teamDir string, cliVars map[string]string) (map[string]string, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("invalid team directory: %w", err)
	}
	cfg, err := parseTeamYML(absDir, cliVars)
	if err != nil {
		return nil, err
	}
	if cfg.Name == "" {
		cfg.Name = filepath.Base(absDir)
	}
	templateVars := make(map[string]string)
	for k, v := range cfg.Vars {
		templateVars[k] = fmt.Sprintf("%v", v)
	}
	for k, v := range cliVars {
		templateVars[k] = v
	}
	if _, ok := templateVars["TEAM_NAME"]; !ok && cfg.Name != "" {
		templateVars["TEAM_NAME"] = cfg.Name
	}
	return templateVars, nil
}

// ValidateAgentFile parses an agent Markdown file through the runtime parser.
// Promotion apply uses it before and after writes to prove frontmatter identity.
func ValidateAgentFile(path string) (*agent.AgentDef, error) {
	return ValidateAgentFileWithVars(path, nil)
}

// ValidateAgentFileWithVars parses an agent Markdown file through the runtime parser with effective template variables.
func ValidateAgentFileWithVars(path string, vars map[string]string) (*agent.AgentDef, error) {
	return parseAgentFile(path, vars)
}

// ValidateAgentContent parses complete agent Markdown without filesystem I/O.
// The logical path is used only for diagnostics and template identity.
func ValidateAgentContent(raw []byte, path string) (*agent.AgentDef, error) {
	return ValidateAgentContentWithVars(raw, path, nil)
}

// ValidateAgentContentWithVars parses complete agent Markdown without filesystem I/O with effective template variables.
func ValidateAgentContentWithVars(raw []byte, path string, vars map[string]string) (*agent.AgentDef, error) {
	return parseAgentContent(raw, path, vars)
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
		Compaction:    agent.DefaultCompactionPolicy(),
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
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&yc); err != nil {
		return cfg, fmt.Errorf("failed to parse team config: %w", err)
	}
	if err := mergeAdvancedNamespace(&yc); err != nil {
		return cfg, fmt.Errorf("invalid team config: %w", err)
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
	if yc.MinimumCoordinatorRounds > 0 {
		cfg.MinimumCoordinatorRounds = yc.MinimumCoordinatorRounds
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
	if yc.MaxRetries != nil {
		cfg.MaxRetries = *yc.MaxRetries
	}
	if yc.AutoReport {
		cfg.AutoReport = true
	}
	if yc.AllowFreeTextResults {
		cfg.AllowFreeTextResults = true
	}
	if yc.MaxSteps > 0 {
		cfg.MaxSteps = yc.MaxSteps
	}
	cfg.Generation = agent.GenerationParams{
		Model:           yc.Model,
		ContextWindow:   yc.ContextWindow,
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
	if yc.StallThreshold != "" {
		cfg.StallThreshold = yc.StallThreshold
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
	cfg.MemoryLearning = resolveMemoryLearningPolicy(yc.MemoryLearning)
	if err := validateMemoryLearningPolicy(cfg.MemoryLearning); err != nil {
		return cfg, fmt.Errorf("invalid memory-learning config: %w", err)
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
	if policy, err := yc.Compaction.apply(cfg.Compaction); err != nil {
		return cfg, err
	} else {
		cfg.Compaction = policy
	}
	if tools := parseAllowedTools(yc.ToolsAllowed); len(tools) > 0 {
		cfg.ToolsAllowed = strings.Split(agent.ExpandImpliedTools(strings.Join(tools, ",")), ",")
	}
	if tools := parseDeniedTools(yc.ToolsAllowed); len(tools) > 0 {
		cfg.ToolsDenied = tools
	}
	cfg.Requirements = agent.ContractRequirements{
		Tools:       append([]string(nil), yc.Requirements.Tools...),
		Model:       yc.Requirements.Model,
		Environment: append([]string(nil), yc.Requirements.Environment...),
		Paths:       expandAllowedPaths(yc.Requirements.Paths),
		Interactive: yc.Requirements.Interactive,
		Network:     yc.Requirements.Network,
		PlanFirst:   yc.Requirements.PlanFirst,
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
	if yc.Delegation.BindTaskGoalContracts {
		cfg.Delegation.BindTaskGoalContracts = true
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
	if len(yc.Workflow.Phases) > 0 {
		cfg.Workflow = agent.WorkflowConfig{Phases: append([]string(nil), yc.Workflow.Phases...)}
		cfg.Policies = yc.Policies
		cfg.Capabilities = agent.CapabilityConfig{Required: append([]string(nil), yc.Capabilities.Required...)}
		cfg.Verification = yc.Verification
		cfg.Retry = yc.Retry
	}
	// Action providers are independent of whether the optional phase workflow
	// is enabled. Keeping this outside the workflow block ensures configured
	// providers are available to static validation and runtime action tasks in
	// every supported team shape.
	if len(yc.ActionProviders) > 0 {
		cfg.ActionProviders = make(map[string]agent.ActionProviderConfig, len(yc.ActionProviders))
		for capability, provider := range yc.ActionProviders {
			cfg.ActionProviders[capability] = agent.ActionProviderConfig{
				Command: append([]string(nil), provider.Command...), Dir: provider.Dir, Timeout: provider.Timeout,
			}
		}
	}

	return cfg, nil
}

func resolveMemoryLearningPolicy(raw rawMemoryLearningPolicy) agent.MemoryLearningPolicy {
	p := agent.DefaultMemoryLearningPolicy()
	if raw.Mode != "" {
		p.Mode = agent.MemoryLearningMode(strings.TrimSpace(raw.Mode))
	}
	if raw.PolicyVersion != "" {
		p.PolicyVersion = strings.TrimSpace(raw.PolicyVersion)
	}
	if raw.PriorAlpha != nil {
		p.PriorAlpha = *raw.PriorAlpha
	}
	if raw.PriorBeta != nil {
		p.PriorBeta = *raw.PriorBeta
	}
	if raw.UtilityPercentile != nil {
		p.UtilityPercentile = *raw.UtilityPercentile
	}
	if raw.MaxCreditPerSignal != nil {
		p.MaxCreditPerSignal = *raw.MaxCreditPerSignal
	}
	if raw.MinConfirmedSupport != nil {
		p.MinConfirmedSupport = *raw.MinConfirmedSupport
	}
	if raw.MinIndependentTasks != nil {
		p.MinIndependentTasks = *raw.MinIndependentTasks
	}
	if raw.MaxHarmRate != nil {
		p.MaxHarmRate = *raw.MaxHarmRate
	}
	return p
}

func validateMemoryLearningPolicy(p agent.MemoryLearningPolicy) error {
	switch p.Mode {
	case agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive:
	default:
		return fmt.Errorf("mode %q must be off, observe, shadow, or active", p.Mode)
	}
	if p.PolicyVersion == "" {
		return errors.New("policy-version must not be empty")
	}
	if p.PriorAlpha <= 0 || p.PriorBeta <= 0 {
		return errors.New("prior-alpha and prior-beta must be positive")
	}
	if p.UtilityPercentile <= 0 || p.UtilityPercentile >= 1 {
		return errors.New("utility-percentile must be between 0 and 1")
	}
	if p.MaxCreditPerSignal <= 0 {
		return errors.New("max-credit-per-signal must be positive")
	}
	if p.MinConfirmedSupport < 0 || p.MinIndependentTasks < 0 {
		return errors.New("support thresholds must be non-negative")
	}
	if p.MaxHarmRate < 0 || p.MaxHarmRate > 1 {
		return errors.New("max-harm-rate must be between 0 and 1")
	}
	return nil
}

// loadTeamContractTasks reads optional static task contracts from team YAML.
// A team may opt in to binding the initial batch or goal-selected later tasks;
// otherwise these remain available only to preflight tooling.
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
			Tasks    []TaskDef `yaml:"tasks"`
			Advanced struct {
				Tasks []TaskDef `yaml:"tasks"`
			} `yaml:"advanced"`
		}
		if err := yaml.Unmarshal([]byte(text), &config); err != nil {
			return nil, fmt.Errorf("parse team task contracts: %w", err)
		}
		if len(config.Tasks) > 0 && len(config.Advanced.Tasks) > 0 {
			return nil, errors.New("\"tasks\" is defined both at the top level and under \"advanced\"; remove one")
		}
		if len(config.Advanced.Tasks) > 0 {
			return config.Advanced.Tasks, nil
		}
		return config.Tasks, nil
	}
	return nil, nil
}

func LoadTeam(teamDir string, vars map[string]string, forcedSkills []string, registry *ProviderRegistry) (*TeamSession, error) {
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
	effectiveRegistry, err := registry.Clone()
	if err != nil {
		return nil, fmt.Errorf("initialize action provider registry: %w", err)
	}
	if err := registerConfiguredActionProviders(effectiveRegistry, cfg.ActionProviders); err != nil {
		return nil, err
	}
	session := &TeamSession{
		Config:           cfg,
		Dir:              absDir,
		Workspace:        workspace,
		Agents:           make(map[string]*agent.AgentDef),
		MCPServers:       make(map[string]mcp.MCPServerConfig),
		ContractTasks:    contractTasks,
		ProviderRegistry: effectiveRegistry,
	}

	// Inject built-in vars BEFORE loading agents
	if _, ok := templateVars["TEAM_NAME"]; !ok && cfg.Name != "" {
		templateVars["TEAM_NAME"] = cfg.Name
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read team directory: %w", err)
	}

	type agentIdentityOwner struct {
		def  *agent.AgentDef
		path string
	}
	identityOwners := make(map[string]agentIdentityOwner)
	var coordinatorPath string
	registerCoordinator := func(def *agent.AgentDef, path string) error {
		if def.Role != "coordinator" {
			return nil
		}
		if coordinatorPath != "" {
			return fmt.Errorf("team has more than one coordinator agent: %s and %s both resolve to role \"coordinator\"", coordinatorPath, path)
		}
		coordinatorPath = path
		return nil
	}
	registerIdentity := func(identity string, def *agent.AgentDef, path string) error {
		key := normalizedName(identity)
		if key == "helper" {
			return fmt.Errorf("agent identity %q is reserved by the built-in Helper (agent %q from %s)", identity, def.Name, path)
		}
		if previous, exists := identityOwners[key]; exists && previous.def != def {
			return fmt.Errorf("agent identity collision for %q: agent %q from %s conflicts with agent %q from %s", key, previous.def.Name, previous.path, def.Name, path)
		}
		identityOwners[key] = agentIdentityOwner{def: def, path: path}
		return nil
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
		if err := registerIdentity(fileAlias, def, path); err != nil {
			return nil, err
		}
		if err := registerIdentity(def.Name, def, path); err != nil {
			return nil, err
		}
		if err := registerCoordinator(def, path); err != nil {
			return nil, err
		}
		fileKey := normalizedName(fileAlias)
		nameKey := normalizedName(def.Name)
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
	if err := validateRuntimeWorkflowTeam(session, effectiveRegistry); err != nil {
		return nil, err
	}
	loadFindings := append(ValidateTeamTaskContracts(session), ValidateTeamPolicyContracts(session)...)
	if messages := sortedContractFindingMessages(loadFindings); len(messages) > 0 {
		return nil, fmt.Errorf("team contract validation failed: %s", strings.Join(messages, "; "))
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
	seenIDs := map[string]string{}        // memory-id → agent name (for duplicate detection)
	visited := map[*agent.AgentDef]bool{} // session.Agents registers one *AgentDef under both its file-alias key and its (possibly different) name key; visit each def once.
	for _, def := range session.Agents {
		if visited[def] {
			continue
		}
		visited[def] = true
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
