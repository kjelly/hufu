// Package paired defines the versioned, offline contracts for controlled
// harness comparisons. It does not create providers or runtime workspaces.
package paired

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion = 1
	MaxDataBytes  = 1 << 20
	MaxTextBytes  = 64 << 10
	MaxFiles      = 1000
	MaxTotalBytes = 32 << 20
	MaxCases      = 100
	MaxReplicates = 20
)

// Config contains authored paths, relative to the configuration file.
// It is not a committed runtime plan.
type Config struct {
	SchemaVersion   int               `yaml:"schema_version" json:"schema_version"`
	ExperimentID    string            `yaml:"experiment_id" json:"experiment_id"`
	Mode            string            `yaml:"mode" json:"mode"`
	ExecutionMode   string            `yaml:"execution_mode" json:"execution_mode"`
	Profile         string            `yaml:"profile" json:"profile"`
	BaselineDir     string            `yaml:"baseline_dir" json:"baseline_dir"`
	CandidateDirs   []Candidate       `yaml:"candidate_dirs" json:"candidate_dirs,omitempty"`
	Fixture         string            `yaml:"fixture" json:"fixture"`
	Worker          string            `yaml:"worker" json:"worker"`
	Vars            map[string]string `yaml:"vars" json:"vars"`
	AllowedSurfaces []Surface         `yaml:"allowed_surfaces" json:"allowed_surfaces"`
	Models          map[string]Model  `yaml:"models" json:"models"`
	FixtureProgram  string            `yaml:"fixture_program" json:"fixture_program,omitempty"`
	Replicates      int64             `yaml:"replicates" json:"replicates"`
	Limits          Limits            `yaml:"limits" json:"limits"`
	Objective       Objective         `yaml:"objective" json:"objective"`
	Refinement      *Refinement       `yaml:"refinement" json:"refinement,omitzero"`
}

type Candidate struct {
	ID   string `yaml:"id" json:"id"`
	Path string `yaml:"path" json:"path"`
}

type Surface struct {
	Kind  string `yaml:"kind" json:"kind"`
	Agent string `yaml:"agent" json:"agent,omitempty"`
	Path  string `yaml:"path" json:"path,omitempty"`
}

type Model struct {
	Backend string `yaml:"backend" json:"backend"`
	Model   string `yaml:"model" json:"model"`
}

type Limits struct {
	PerStageTokens  int64 `yaml:"per_stage_tokens" json:"per_stage_tokens"`
	PerStageSeconds int64 `yaml:"per_stage_seconds" json:"per_stage_seconds"`
	PipelineTokens  int64 `yaml:"pipeline_tokens" json:"pipeline_tokens"`
	PipelineSeconds int64 `yaml:"pipeline_seconds" json:"pipeline_seconds"`
	MaxProbeRuns    int64 `yaml:"max_probe_runs" json:"max_probe_runs"`
	StageRetries    int64 `yaml:"stage_retries" json:"stage_retries"`
}

type Objective struct {
	Kind                     string `yaml:"kind" json:"kind"`
	MinPassGain              *int64 `yaml:"min_pass_gain" json:"min_pass_gain,omitzero"`
	MinTokenReductionPercent *int64 `yaml:"min_token_reduction_percent" json:"min_token_reduction_percent,omitzero"`
}

type Refinement struct {
	ProcedurePrompt string `yaml:"procedure_prompt" json:"procedure_prompt"`
	DomainPrompt    string `yaml:"domain_prompt" json:"domain_prompt"`
	ComposerPrompt  string `yaml:"composer_prompt" json:"composer_prompt"`
	ReflectorPrompt string `yaml:"reflector_prompt" json:"reflector_prompt"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func ValidID(value string) bool { return identifier.MatchString(value) }

func (c *Config) UnmarshalYAML(node *yaml.Node) error {
	type plain Config
	value := plain(*c)
	if err := node.Decode(&value); err != nil {
		return err
	}
	keys := yamlKeys(node)
	if value.ExecutionMode == "llm" && keys["fixture_program"] {
		return errors.New("fixture_program: must be omitted in llm mode")
	}
	if value.Mode == "refine" && keys["candidate_dirs"] {
		return errors.New("candidate_dirs: must be omitted in refine mode")
	}
	*c = Config(value)
	return nil
}

func (s *Surface) UnmarshalYAML(node *yaml.Node) error {
	type plain Surface
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	keys := yamlKeys(node)
	if value.Kind == "skill_body" && keys["agent"] {
		return errors.New("skill_body: agent must be omitted")
	}
	if (value.Kind == "markdown_body" || value.Kind == "inline_instructions") && keys["path"] {
		return errors.New("agent surface: path must be omitted")
	}
	*s = Surface(value)
	return nil
}

func yamlKeys(node *yaml.Node) map[string]bool {
	keys := map[string]bool{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keys[node.Content[i].Value] = true
	}
	return keys
}

func ParseConfig(raw []byte) (Config, error) {
	cfg := Config{Replicates: 1, Vars: map[string]string{}}
	if err := DecodeYAML(raw, &cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return errors.New("schema_version: must be 1")
	}
	if !ValidID(c.ExperimentID) {
		return errors.New("experiment_id: invalid identifier")
	}
	if c.Profile != "structured_only" {
		return errors.New("profile: must be structured_only")
	}
	if c.ExecutionMode != "fixture" && c.ExecutionMode != "llm" {
		return errors.New("execution_mode: must be fixture or llm")
	}
	if (c.ExecutionMode == "fixture") != (strings.TrimSpace(c.FixtureProgram) != "") {
		return errors.New("fixture_program: required only in fixture mode")
	}
	for key, value := range map[string]string{
		"baseline_dir": c.BaselineDir, "fixture": c.Fixture, "worker": c.Worker,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s: required", key)
		}
	}
	if c.Replicates < 1 || c.Replicates > MaxReplicates {
		return fmt.Errorf("replicates: must be between 1 and %d", MaxReplicates)
	}
	if err := c.validateMode(); err != nil {
		return err
	}
	if err := c.validateModels(); err != nil {
		return err
	}
	if err := validateSurfaces(c.AllowedSurfaces); err != nil {
		return err
	}
	if err := c.Limits.Validate(); err != nil {
		return err
	}
	return c.Objective.Validate()
}

func (c Config) validateMode() error {
	switch c.Mode {
	case "manual":
		if len(c.CandidateDirs) != 1 || c.Refinement != nil {
			return errors.New("manual: requires exactly one candidate_dirs entry and no refinement")
		}
		candidate := c.CandidateDirs[0]
		if !ValidID(candidate.ID) || slices.Contains([]string{"baseline", "procedure", "domain", "composition"}, candidate.ID) {
			return errors.New("candidate_dirs.id: invalid or reserved identifier")
		}
		if strings.TrimSpace(candidate.Path) == "" {
			return errors.New("candidate_dirs.path: required")
		}
	case "refine":
		if c.CandidateDirs != nil || c.Refinement == nil {
			return errors.New("refine: omit candidate_dirs and provide refinement")
		}
		for key, value := range map[string]string{
			"procedure_prompt": c.Refinement.ProcedurePrompt, "domain_prompt": c.Refinement.DomainPrompt,
			"composer_prompt": c.Refinement.ComposerPrompt, "reflector_prompt": c.Refinement.ReflectorPrompt,
		} {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("refinement.%s: required", key)
			}
		}
	default:
		return errors.New("mode: must be manual or refine")
	}
	return nil
}

func (c Config) validateModels() error {
	roles := []string{"executor"}
	if c.Mode == "refine" {
		roles = append(roles, "proposer", "composer", "reflector")
	}
	if len(c.Models) != len(roles) {
		return errors.New("models: must contain exactly the roles required by mode")
	}
	for _, role := range roles {
		model, exists := c.Models[role]
		if !exists || strings.TrimSpace(model.Backend) == "" || strings.TrimSpace(model.Model) == "" {
			return fmt.Errorf("models.%s: backend and model required", role)
		}
		if c.ExecutionMode == "fixture" && model != (Model{Backend: "fixture", Model: "scripted"}) {
			return fmt.Errorf("models.%s: fixture mode requires fixture/scripted", role)
		}
		if c.ExecutionMode == "llm" && model.Backend != "hufu-local" {
			return fmt.Errorf("models.%s.backend: only hufu-local is supported", role)
		}
	}
	return nil
}

func validateSurfaces(surfaces []Surface) error {
	if len(surfaces) == 0 {
		return errors.New("allowed_surfaces: at least one required")
	}
	seen := map[Surface]bool{}
	for i, surface := range surfaces {
		if seen[surface] {
			return fmt.Errorf("allowed_surfaces[%d]: duplicate surface", i)
		}
		seen[surface] = true
		switch surface.Kind {
		case "markdown_body", "inline_instructions":
			if strings.TrimSpace(surface.Agent) == "" || surface.Path != "" {
				return fmt.Errorf("allowed_surfaces[%d]: requires agent and forbids path", i)
			}
		case "skill_body":
			if surface.Agent != "" || !SafeRelativePath(surface.Path) || !strings.HasSuffix(surface.Path, "/SKILL.md") {
				return fmt.Errorf("allowed_surfaces[%d]: requires a relative SKILL.md path and forbids agent", i)
			}
		default:
			return fmt.Errorf("allowed_surfaces[%d].kind: unsupported value", i)
		}
	}
	return nil
}

func (l Limits) Validate() error {
	for key, value := range map[string]int64{
		"per_stage_tokens": l.PerStageTokens, "per_stage_seconds": l.PerStageSeconds,
		"pipeline_tokens": l.PipelineTokens, "pipeline_seconds": l.PipelineSeconds, "max_probe_runs": l.MaxProbeRuns,
	} {
		if value <= 0 {
			return fmt.Errorf("limits.%s: must be positive", key)
		}
	}
	// All seconds must also fit time.Duration without wrapping.
	if l.PipelineSeconds > math.MaxInt64/1_000_000_000 || l.PerStageSeconds > math.MaxInt64/1_000_000_000 {
		return errors.New("limits: seconds overflow time.Duration")
	}
	if l.PipelineTokens < l.PerStageTokens || l.PipelineSeconds < l.PerStageSeconds {
		return errors.New("limits: pipeline budget must cover a stage")
	}
	if l.StageRetries < 0 || l.StageRetries > 1 {
		return errors.New("limits.stage_retries: must be 0 or 1")
	}
	return nil
}

// ValidateEnvelope reserves a complete baseline and one complete candidate.
// Refinement also needs at least one proposal before that candidate can exist.
func (c Config) ValidateEnvelope(cases int) error {
	if cases < 1 || cases > MaxCases {
		return fmt.Errorf("cases: must contain between 1 and %d entries", MaxCases)
	}
	if err := c.Validate(); err != nil {
		return err
	}
	executors := int64(cases) * c.Replicates * 2
	stages := executors
	if c.Mode == "refine" {
		stages++
	}
	if c.Limits.MaxProbeRuns < executors || c.Limits.PerStageTokens > c.Limits.PipelineTokens/stages ||
		c.Limits.PerStageSeconds > c.Limits.PipelineSeconds/stages {
		return errors.New("limits: insufficient envelope for baseline and one complete candidate")
	}
	return nil
}

func (o Objective) Validate() error {
	switch o.Kind {
	case "quality_gain":
		if o.MinPassGain == nil || *o.MinPassGain <= 0 || o.MinTokenReductionPercent != nil {
			return errors.New("objective: quality_gain requires only positive min_pass_gain")
		}
	case "token_reduction":
		if o.MinPassGain != nil || o.MinTokenReductionPercent == nil || *o.MinTokenReductionPercent < 1 || *o.MinTokenReductionPercent > 99 {
			return errors.New("objective: token_reduction requires only min_token_reduction_percent between 1 and 99")
		}
	default:
		return errors.New("objective.kind: unsupported value")
	}
	return nil
}
