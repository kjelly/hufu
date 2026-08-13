package team

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Workflow defines the phase-based state machine for execution.
type Workflow struct {
	Phases []Phase `json:"phases,omitempty" yaml:"phases,omitempty"`
}

func (w Workflow) Validate() error {
	if len(w.Phases) == 0 {
		return fmt.Errorf("workflow must have at least one phase")
	}
	canonical := []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify}
	index := make(map[Phase]int, len(canonical))
	for i, p := range canonical {
		index[p] = i
	}
	last := -1
	seen := make(map[Phase]bool)
	for _, p := range w.Phases {
		i, ok := index[p]
		if !ok {
			return fmt.Errorf("workflow phase %q is unsupported; use prepare, audit, execute, or verify", p)
		}
		if seen[p] || i <= last {
			return fmt.Errorf("workflow phases must be unique and ordered prepare → audit → execute → verify")
		}
		seen[p] = true
		last = i
	}
	if w.Phases[0] != PhasePrepare || w.Phases[len(w.Phases)-1] != PhaseVerify {
		return fmt.Errorf("workflow must start with prepare and end with verify")
	}
	return nil
}

// Capabilities defines what actions are allowed during execution.
type Capabilities struct {
	Required []string `json:"required,omitempty" yaml:"required,omitempty"`
}

func (c Capabilities) Validate() error {
	for i, req := range c.Required {
		if strings.TrimSpace(req) == "" {
			return fmt.Errorf("capability requirement at index %d is empty", i)
		}
	}
	return nil
}

// RuntimeWorkspace defines the formal isolation boundary for the execution.
type RuntimeWorkspace struct {
	Root string `json:"root,omitempty" yaml:"root,omitempty"`
}

// Resolve returns a path under the runtime workspace and rejects traversal.
func (w RuntimeWorkspace) Resolve(relative string) (string, error) {
	if strings.TrimSpace(w.Root) == "" {
		return "", fmt.Errorf("runtime workspace root is empty")
	}
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("runtime workspace path must be relative")
	}
	root := filepath.Clean(w.Root)
	path := filepath.Clean(filepath.Join(root, relative))
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("runtime workspace path escapes root")
	}
	return path, nil
}

// ArtifactPath resolves a durable artifact location under runtime/artifacts.
func (w RuntimeWorkspace) ArtifactPath(name string) (string, error) {
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(name) {
		return "", fmt.Errorf("artifact path escapes artifacts directory")
	}
	return w.Resolve(filepath.Join("artifacts", name))
}

func (w RuntimeWorkspace) Validate() error {
	if strings.TrimSpace(w.Root) == "" {
		return fmt.Errorf("runtime workspace root must not be empty")
	}
	return nil
}

// Policies defines rules around execution, phase transitions, and retries.
type Policies struct {
	RequirePhaseSuccess bool `json:"require_phase_success,omitempty" yaml:"require_phase_success,omitempty"`
	AllowPhaseSkip      bool `json:"allow_phase_skip,omitempty" yaml:"allow_phase_skip,omitempty"`
	MaxRetries          int  `json:"max_retries,omitempty" yaml:"max_retries,omitempty"`
	FailFast            bool `json:"fail_fast,omitempty" yaml:"fail_fast,omitempty"`
}

func (p Policies) Validate(w Workflow) error {
	if p.MaxRetries < 0 {
		return fmt.Errorf("max_retries cannot be negative")
	}
	if !p.AllowPhaseSkip && len(w.Phases) > 0 {
		required := []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify}
		if len(w.Phases) != len(required) {
			return fmt.Errorf("workflow phase skipping is disabled; configure prepare, audit, execute, and verify")
		}
		for i, phase := range required {
			if w.Phases[i] != phase {
				return fmt.Errorf("workflow phase skipping is disabled; configure prepare, audit, execute, and verify in order")
			}
		}
	}
	return nil
}

// ExecutionContext formalizes the execution environment, strictly separated from
// the natural language task intent.
type ExecutionContext struct {
	RunID            string            `json:"run_id,omitempty" yaml:"run_id,omitempty"`
	Team             string            `json:"team,omitempty" yaml:"team,omitempty"`
	CurrentPhase     Phase             `json:"phase,omitempty" yaml:"phase,omitempty"`
	RepositoryRoot   string            `json:"repository_root,omitempty" yaml:"repository_root,omitempty"`
	Workflow         Workflow          `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	Capabilities     Capabilities      `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	RuntimeWorkspace RuntimeWorkspace  `json:"runtime_workspace,omitempty" yaml:"runtime_workspace,omitempty"`
	ArtifactPaths    map[string]string `json:"artifact_paths,omitempty" yaml:"artifact_paths,omitempty"`
	Policies         Policies          `json:"policies,omitempty" yaml:"policies,omitempty"`
}

func (e ExecutionContext) Validate() error {
	if e.Team == "" {
		return fmt.Errorf("team is required")
	}
	if err := e.Workflow.Validate(); err != nil {
		return fmt.Errorf("invalid workflow: %w", err)
	}
	if err := e.Capabilities.Validate(); err != nil {
		return fmt.Errorf("invalid capabilities: %w", err)
	}
	if err := e.RuntimeWorkspace.Validate(); err != nil {
		return fmt.Errorf("invalid runtime workspace: %w", err)
	}
	if strings.TrimSpace(e.RepositoryRoot) == "" {
		return fmt.Errorf("repository root must not be empty")
	}
	if e.CurrentPhase != PhaseInit && e.CurrentPhase != PhaseDone && e.CurrentPhase != PhaseFailed {
		found := false
		for _, phase := range e.Workflow.Phases {
			if phase == e.CurrentPhase {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("current phase %q is not in workflow", e.CurrentPhase)
		}
	}
	for name, path := range e.ArtifactPaths {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(path) == "" {
			return fmt.Errorf("artifact path %q must have a non-empty name and path", name)
		}
	}
	if err := e.Policies.Validate(e.Workflow); err != nil {
		return fmt.Errorf("invalid policies: %w", err)
	}
	return nil
}

// Task represents the pure user intent, separated from execution mechanics.
type Task struct {
	Instruction string `json:"instruction,omitempty" yaml:"instruction,omitempty"`
}
