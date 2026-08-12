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

// Capabilities defines what actions are allowed during execution.
type Capabilities struct {
	Required []string `json:"required,omitempty" yaml:"required,omitempty"`
}

// RuntimeWorkspace defines the formal isolation boundary for the execution.
type RuntimeWorkspace struct {
	Root string `json:"root,omitempty" yaml:"root,omitempty"`
}

// Resolve returns a path under the runtime workspace and rejects traversal.
// Runtime artifacts therefore have a stable, project-neutral home even when
// the task's source checkout lives elsewhere.
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

// Policies defines rules around execution, phase transitions, and retries.
type Policies struct {
	RequirePhaseSuccess bool `json:"require_phase_success,omitempty" yaml:"require_phase_success,omitempty"`
	AllowPhaseSkip      bool `json:"allow_phase_skip,omitempty" yaml:"allow_phase_skip,omitempty"`
}

// ExecutionContext formalizes the execution environment, strictly separated from
// the natural language task intent.
type ExecutionContext struct {
	Team             string           `json:"team,omitempty" yaml:"team,omitempty"`
	Workflow         Workflow         `json:"workflow,omitempty" yaml:"workflow,omitempty"`
	Capabilities     Capabilities     `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	RuntimeWorkspace RuntimeWorkspace `json:"runtime_workspace,omitempty" yaml:"runtime_workspace,omitempty"`
	Policies         Policies         `json:"policies,omitempty" yaml:"policies,omitempty"`
}

// Task represents the pure user intent, separated from execution mechanics.
type Task struct {
	Instruction string `json:"instruction,omitempty" yaml:"instruction,omitempty"`
}
