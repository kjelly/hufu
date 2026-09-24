package agent

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WorkerWorkspaceMode selects where a worker's attempts write project files.
type WorkerWorkspaceMode string

const (
	// WorkerWorkspaceShared runs attempts in the canonical project (default).
	WorkerWorkspaceShared WorkerWorkspaceMode = "shared"
	// WorkerWorkspaceIsolated runs each writing attempt in a private copy of
	// the project whose changes are applied back under per-path
	// preconditions.
	WorkerWorkspaceIsolated WorkerWorkspaceMode = "isolated"
)

// WorkerWorkspaceIntegrateOnVerified applies an isolated attempt's changes
// after its verification passes. It is the only integration mode.
const WorkerWorkspaceIntegrateOnVerified = "on-verified"

// WorkerWorkspaceSpec is the authoring shape of `worker-workspace` in agent
// frontmatter or as a team default.
type WorkerWorkspaceSpec struct {
	Mode      WorkerWorkspaceMode `yaml:"mode" json:"mode"`
	Integrate string              `yaml:"integrate,omitempty" json:"integrate,omitempty"`
}

// Clone returns an independent copy, or nil for a nil spec.
func (s *WorkerWorkspaceSpec) Clone() *WorkerWorkspaceSpec {
	if s == nil {
		return nil
	}
	clone := *s
	return &clone
}

// Isolated reports whether the spec selects isolated attempt worlds.
func (s *WorkerWorkspaceSpec) Isolated() bool {
	return s != nil && s.Mode == WorkerWorkspaceIsolated
}

// UnmarshalYAML decodes strictly: an unknown or misspelled key would silently
// fall back to the shared workspace.
func (s *WorkerWorkspaceSpec) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("worker-workspace must be a mapping with mode and integrate")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "mode", "integrate":
		default:
			return fmt.Errorf("worker-workspace: unknown key %q (supported: mode, integrate)", key)
		}
	}
	type plain WorkerWorkspaceSpec
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return fmt.Errorf("worker-workspace: %w", err)
	}
	spec := WorkerWorkspaceSpec(decoded)
	if err := spec.Validate(); err != nil {
		return err
	}
	*s = spec
	return nil
}

// Validate checks the mode and its integration setting.
func (s *WorkerWorkspaceSpec) Validate() error {
	if s == nil {
		return nil
	}
	switch s.Mode {
	case WorkerWorkspaceShared:
		if s.Integrate != "" {
			return fmt.Errorf("worker-workspace: integrate applies only to mode %q", WorkerWorkspaceIsolated)
		}
	case WorkerWorkspaceIsolated:
		if s.Integrate != WorkerWorkspaceIntegrateOnVerified {
			return fmt.Errorf("worker-workspace: mode %q requires integrate: %s", WorkerWorkspaceIsolated, WorkerWorkspaceIntegrateOnVerified)
		}
	default:
		return fmt.Errorf("worker-workspace: unknown mode %q (supported: %s, %s)", s.Mode, WorkerWorkspaceShared, WorkerWorkspaceIsolated)
	}
	return nil
}
