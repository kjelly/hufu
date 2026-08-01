package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/anomalyco/hufu/internal/agent"
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
//
// This is the legacy wrapper preserved for callers that do not need the
// transitional verifier-lint switch (§4.3, WP-02). It is equivalent to
// ValidateExecutionContractFull with lintMode = "error".
func ValidateExecutionContract(task TaskDef) error {
	result := ValidateExecutionContractFull(task, agent.VerifierLintError)
	return result.Error()
}

// ValidateExecutionContractFull validates the execution contract for a task
// across ALL ExecutionKinds, returning a structured ContractPreflightResult.
//
// Unlike the legacy ValidateExecutionContract, this:
//   - validates the typed VerificationSpec for every ExecutionKind, not only
//     interactive/external with requires_verification (§4.3, WP-02);
//   - runs the verifier assertiveness lint (LintTaskDef) for every kind;
//   - routes error-severity findings according to lintMode:
//     "error" (default) → findings become errors that block dispatch,
//     "warn"             → findings are downgraded to warnings (caller emits
//     an event but still dispatches),
//     "off"              → lint findings are discarded entirely.
//
// It remains a pure function: no I/O, no global state. The caller is
// responsible for emitting warning events for non-blocking findings.
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.1, §4.3, WP-02
func ValidateExecutionContractFull(task TaskDef, lintMode string) ContractPreflightResult {
	mode := agent.NormalizeVerifierLintMode(lintMode)
	c := DefaultExecutionContract(task.Execution)
	var findings []ContractFinding

	switch c.Kind {
	case ExecutionKindInline, ExecutionKindProcess, ExecutionKindInteractive, ExecutionKindExternal:
		// Valid execution kind
	default:
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError,
			Code:     "invalid_execution_kind",
			Field:    "execution",
			Message:  fmt.Sprintf("invalid execution kind: %q", task.Execution.Kind),
		})
		return ContractPreflightResult{Valid: false, Findings: findings}
	}

	// Validate typed VerificationSpec for ALL ExecutionKinds (WP-02). Previously
	// only interactive/external with requires_verification validated the spec.
	if task.VerifySpec != nil {
		spec := NormalizeVerificationSpec(*task.VerifySpec, task.Verify, task.VerifyMode)
		if (c.Kind == ExecutionKindInteractive || c.Kind == ExecutionKindExternal) && c.RequiresVerification {
			if spec.Mode == "observation" {
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityError,
					Code:     FindingObservationMode,
					Field:    "verify_spec",
					Message:  fmt.Sprintf("execution contract for kind %q with requires_verification=true requires an asserting verifier, not observation mode", c.Kind),
				})
			}
		}
		if err := validateVerificationSpec(spec); err != nil {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "verifier_invalid",
				Field:    "verify_spec",
				Message:  fmt.Sprintf("execution contract for kind %q has invalid typed verifier: %s", c.Kind, err.Error()),
			})
		}
	} else if (c.Kind == ExecutionKindInteractive || c.Kind == ExecutionKindExternal) && c.RequiresVerification {
		verifyCmd := strings.TrimSpace(task.Verify)
		verifyMode := strings.ToLower(strings.TrimSpace(task.VerifyMode))
		if verifyCmd == "" || verifyMode == "none" {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "verifier_missing",
				Field:    "verify",
				Message:  fmt.Sprintf("execution contract for kind %q with requires_verification=true requires an objective verifier contract (non-empty verify command and verify_mode != 'none')", c.Kind),
			})
		}
	}

	// Lint the verifier contract for §4.3 anti-patterns and reject non-asserting forms.
	lintFindings := LintTaskDef(task)
	findings = append(findings, lintFindings...)

	valid := true
	for i := range findings {
		// Route error-severity lint findings according to lintMode. Non-lint
		// errors (invalid kind, missing verifier, malformed spec) always block
		// dispatch regardless of lintMode.
		if findings[i].Severity != FindingSeverityError {
			continue
		}
		if !isLintFinding(findings[i].Code) {
			// Structural errors always block.
			valid = false
			continue
		}
		switch mode {
		case agent.VerifierLintOff:
			// Drop the finding entirely.
			findings[i].Severity = FindingSeverityInfo
		case agent.VerifierLintWarn:
			// Downgrade to warning: caller emits event but still dispatches.
			findings[i].Severity = FindingSeverityWarning
		default:
			// error mode: blocks dispatch.
			valid = false
		}
	}

	return ContractPreflightResult{
		Valid:       valid,
		Findings:    findings,
		Environment: ExecutionEnvironment{},
	}
}

// isLintFinding reports whether a finding code originates from the verifier
// assertiveness lint (§4.3) rather than structural contract validation.
func isLintFinding(code string) bool {
	return code == FindingVerifierNotAsserting
}

// Error returns nil if the preflight result is valid, or an error joining all
// error-severity findings otherwise.
func (r ContractPreflightResult) Error() error {
	if r.Valid {
		return nil
	}
	var errs []string
	for _, f := range r.Findings {
		if f.Severity == FindingSeverityError {
			errs = append(errs, fmt.Sprintf("%s (%s): %s", f.Field, f.Code, f.Message))
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("verifier contract error: %s", strings.Join(errs, "; "))
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

// CanAutomaticallyReplay applies both the structural replay contract and the
// explicit recovery policy. Manual, reconcile, and never policies require a
// recovery decision before any second execution, even when the side effect is
// otherwise locally replayable.
func CanAutomaticallyReplay(task TaskDef) bool {
	if !IsTaskReplayable(task) {
		return false
	}
	switch task.Recovery {
	case RecoveryManual, RecoveryReconcile, RecoveryNever:
		return false
	default:
		return true
	}
}
