package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExecutionKind identifies how a task should be executed.
type ExecutionKind string

const (
	ExecutionKindInline      ExecutionKind = "inline"
	ExecutionKindProcess     ExecutionKind = "process"
	ExecutionKindInteractive ExecutionKind = "interactive"
	ExecutionKindExternal    ExecutionKind = "external"
)

// ExecutionContract defines the structured execution contract for a task.
type ExecutionContract struct {
	Kind                 ExecutionKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	RequiresResult       bool          `json:"requires_result,omitempty" yaml:"requires-result,omitempty"`
	RequiresVerification bool          `json:"requires_verification,omitempty" yaml:"requires-verification,omitempty"`
	AllowsReplay         *bool         `json:"allows_replay,omitempty" yaml:"allows-replay,omitempty"`
}

// UnmarshalJSON handles legacy "strict_result" / "strict-result" keys in JSON.
func (c *ExecutionContract) UnmarshalJSON(data []byte) error {
	type Alias ExecutionContract
	var aux struct {
		*Alias
		StrictResult     *bool `json:"strict_result"`
		StrictResultDash *bool `json:"strict-result"`
	}
	aux.Alias = (*Alias)(c)
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if (aux.StrictResult != nil && *aux.StrictResult) || (aux.StrictResultDash != nil && *aux.StrictResultDash) {
		c.RequiresResult = true
	}
	return nil
}

// UnmarshalYAML handles legacy "strict-result" / "strict_result" keys in YAML.
func (c *ExecutionContract) UnmarshalYAML(node *yaml.Node) error {
	type Alias ExecutionContract
	var aux struct {
		*Alias            `yaml:",inline"`
		StrictResult      *bool `yaml:"strict-result"`
		StrictResultUnder *bool `yaml:"strict_result"`
	}
	aux.Alias = (*Alias)(c)
	if err := node.Decode(&aux); err != nil {
		return err
	}
	if (aux.StrictResult != nil && *aux.StrictResult) || (aux.StrictResultUnder != nil && *aux.StrictResultUnder) {
		c.RequiresResult = true
	}
	return nil
}

// DefaultExecutionContract returns a copy of c with default values populated.
func DefaultExecutionContract(c ExecutionContract) ExecutionContract {
	if c.Kind == "" {
		c.Kind = ExecutionKindInline
	}
	return c
}

// ValidateExecutionContract validates the execution contract for a task.
// It is a pure function that returns an error if the execution contract is invalid.
// It DOES NOT read task.Goal, task.Constraints, or any descriptive text.
func ValidateExecutionContract(task TaskDef) error {
	c := DefaultExecutionContract(task.Execution)

	switch c.Kind {
	case ExecutionKindInline, ExecutionKindProcess, ExecutionKindInteractive, ExecutionKindExternal:
		// Valid execution kind
	default:
		return fmt.Errorf("invalid execution kind: %q", task.Execution.Kind)
	}

	if (c.Kind == ExecutionKindInteractive || c.Kind == ExecutionKindExternal) && c.RequiresVerification {
		// Typed verification supersedes legacy verify fields. Validate it at task
		// admission so an interactive/external task cannot claim it has an
		// objective verifier while carrying an unusable (or observation-only)
		// typed contract. Normalize with legacy fields to preserve mixed
		// migration-era definitions.
		if task.VerifySpec != nil {
			spec := NormalizeVerificationSpec(*task.VerifySpec, task.Verify, task.VerifyMode)
			if spec.Mode == "observation" {
				return fmt.Errorf("execution contract for kind %q with requires_verification=true requires an asserting verifier, not observation mode", c.Kind)
			}
			if err := validateVerificationSpec(spec); err != nil {
				return fmt.Errorf("execution contract for kind %q has invalid typed verifier: %w", c.Kind, err)
			}
			return nil
		}
		verifyCmd := strings.TrimSpace(task.Verify)
		verifyMode := strings.ToLower(strings.TrimSpace(task.VerifyMode))
		if verifyCmd == "" || verifyMode == "none" {
			return fmt.Errorf("execution contract for kind %q with requires_verification=true requires an objective verifier contract (non-empty verify command and verify_mode != 'none')", c.Kind)
		}
	}

	return nil
}

// IsTaskReplayable reports whether a task can be safely re-driven / retried upon failure.
// A task is non-replayable if it has non-replayable side effects OR if its Execution contract
// explicitly sets AllowsReplay = false.
func IsTaskReplayable(task TaskDef) bool {
	if nonReplayableSideEffect(task.SideEffect) {
		return false
	}
	if task.Execution.AllowsReplay != nil && !*task.Execution.AllowsReplay {
		return false
	}
	return true
}
