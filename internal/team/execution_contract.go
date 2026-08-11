package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
)

// ExecutionKind identifies how a task should be executed.
type ExecutionKind string

const (
	ExecutionKindInline      ExecutionKind = "inline"
	ExecutionKindProcess     ExecutionKind = "process"
	ExecutionKindInteractive ExecutionKind = "interactive"
	ExecutionKindExternal    ExecutionKind = "external"
)

// ExecutionEffect describes the externally observable role of one structured
// execution step. It is deliberately domain-neutral: integrations may supply
// their own tools and artifacts, while the runtime only enforces the ordering
// and validation boundary between production and mutation.
type ExecutionEffect string

const (
	ExecutionEffectRead     ExecutionEffect = "read"
	ExecutionEffectProduce  ExecutionEffect = "produce"
	ExecutionEffectValidate ExecutionEffect = "validate"
	ExecutionEffectMutate   ExecutionEffect = "mutate"
	ExecutionEffectVerify   ExecutionEffect = "verify"
)

// StepFailurePolicy controls what the runtime may do after a structured step
// fails. Repairable is intentionally limited to validation steps; a mutation
// failure must follow the task's ordinary rollback or terminal policy.
type StepFailurePolicy string

const (
	StepFailureTerminal   StepFailurePolicy = "terminal"
	StepFailureRepairable StepFailurePolicy = "repairable"
)

// ExecutionOutputKind identifies the runtime representation of a declared
// step output. Empty is treated as artifact for compatibility with the first
// structured-contract revision.
type ExecutionOutputKind string

const (
	ExecutionOutputArtifact ExecutionOutputKind = "artifact"
	ExecutionOutputFact     ExecutionOutputKind = "fact"
	ExecutionOutputReceipt  ExecutionOutputKind = "receipt"
)

// ExecutionStepOutput names a fact or immutable artifact produced by an
// execution step. Names are task-local and are referenced by Consumes.
type ExecutionStepOutput struct {
	Name   string              `json:"name" yaml:"name"`
	Kind   ExecutionOutputKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	Schema string              `json:"schema,omitempty" yaml:"schema,omitempty"`
	Path   string              `json:"path,omitempty" yaml:"path,omitempty"`
	Scope  string              `json:"scope,omitempty" yaml:"scope,omitempty"`
}

// ExecutionStepReference is a typed reference to one declared output of an
// upstream step. Target names the input field supplied to the runner. Scope is
// task-local by default; "secret" prevents the resolved value from being
// copied into receipt previews while still allowing the runner to consume it.
type ExecutionStepReference struct {
	Target string              `json:"target" yaml:"target"`
	StepID string              `json:"step_id" yaml:"step-id"`
	TaskID string              `json:"task_id,omitempty" yaml:"task-id,omitempty"`
	Output string              `json:"output" yaml:"output"`
	Kind   ExecutionOutputKind `json:"kind" yaml:"kind"`
	Schema string              `json:"schema,omitempty" yaml:"schema,omitempty"`
	Scope  string              `json:"scope,omitempty" yaml:"scope,omitempty"`
}

// ExecutionStep is a structured, provider-neutral unit of work. Input is
// decoded data rather than an integration-specific command string; consumers
// can pass provider-specific detail through it without the Hufu runtime
// interpreting it. A mutating step may run only after each consumed artifact
// has been frozen by a successful validate step.
type ExecutionStep struct {
	ID         string                   `json:"id" yaml:"id"`
	Tool       string                   `json:"tool" yaml:"tool"`
	Input      map[string]any           `json:"input,omitempty" yaml:"input,omitempty"`
	DependsOn  []string                 `json:"depends_on,omitempty" yaml:"depends-on,omitempty"`
	Effect     ExecutionEffect          `json:"effect" yaml:"effect"`
	Outputs    []ExecutionStepOutput    `json:"outputs,omitempty" yaml:"outputs,omitempty"`
	References []ExecutionStepReference `json:"references,omitempty" yaml:"references,omitempty"`
	Consumes   []string                 `json:"consumes,omitempty" yaml:"consumes,omitempty"`
	OnFailure  StepFailurePolicy        `json:"on_failure,omitempty" yaml:"on-failure,omitempty"`
	MaxRepairs int                      `json:"max_repairs,omitempty" yaml:"max-repairs,omitempty"`
}

// ExecutionContract defines the structured execution contract for a task.
type ExecutionContract struct {
	Kind                 ExecutionKind `json:"kind,omitempty" yaml:"kind,omitempty"`
	RequiresResult       bool          `json:"requires_result,omitempty" yaml:"requires-result,omitempty"`
	RequiresVerification bool          `json:"requires_verification,omitempty" yaml:"requires-verification,omitempty"`
	AllowsReplay         *bool         `json:"allows_replay,omitempty" yaml:"allows-replay,omitempty"`
	ForbidArtifacts      bool          `json:"forbid_artifacts,omitempty" yaml:"forbid-artifacts,omitempty"`
	// Steps is the structured execution contract for workflows that need
	// artifact/validator dataflow and bounded repair. It is mutually exclusive
	// with the legacy ToolSequence, which remains supported for existing
	// atomic workflows.
	Steps []ExecutionStep `json:"steps,omitempty" yaml:"steps,omitempty"`
	// ToolSequence is an optional closed, ordered list of tool calls for an
	// atomic task. When set, the worker is shown only these tools and the
	// runtime admits calls only in this order. The final entry must be
	// submit_result, which closes the task's tool budget.
	ToolSequence []string `json:"tool_sequence,omitempty" yaml:"tool-sequence,omitempty"`
	// ToolInputSequence optionally requires JSON-object fields for each
	// ToolSequence slot. It has the same length as ToolSequence; an empty object
	// leaves that slot unconstrained. Every declared field must match before the
	// underlying tool runs. This makes an atomic task's call budget enforceable
	// without assigning semantics to a provider, command, or integration.
	ToolInputSequence []map[string]any `json:"tool_input_sequence,omitempty" yaml:"tool-input-sequence,omitempty"`
	// ToolInputField and ToolInputValueSequence provide a compact alternative
	// only for a homogeneous ToolSequence. Mixed tool calls use
	// ToolInputSequence so each slot can constrain its own typed payload.
	// Values align with ToolSequence; an empty value is wildcard.
	ToolInputField         string   `json:"tool_input_field,omitempty" yaml:"tool-input-field,omitempty"`
	ToolInputValueSequence []string `json:"tool_input_value_sequence,omitempty" yaml:"tool-input-value-sequence,omitempty"`
	// ToolExpectedExitCodes declares non-zero process exit codes that are an
	// expected observation for the corresponding ToolSequence slot. This is
	// useful for bounded discovery commands such as timeout, where exit 124 is
	// evidence that the observation window ended rather than a task failure.
	// Each entry aligns with ToolSequence; an empty entry retains the normal
	// success-only policy. It is deliberately limited to non-terminal slots:
	// submit_result is a protocol action, not a process observation.
	ToolExpectedExitCodes [][]int `json:"tool_expected_exit_codes,omitempty" yaml:"tool-expected-exit-codes,omitempty"`
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

	if len(c.ToolSequence) > 0 {
		if len(c.Steps) > 0 {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "execution_contract_mixed_modes",
				Field:    "execution",
				Message:  "structured execution steps cannot be combined with legacy tool_sequence",
			})
		}
		if task.PlanFirst {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "tool_sequence_plan_first",
				Field:    "execution.tool_sequence",
				Message:  "a closed tool sequence cannot be combined with plan_first",
			})
		}
		for i, tool := range c.ToolSequence {
			if strings.TrimSpace(tool) == "" {
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityError,
					Code:     "tool_sequence_empty_tool",
					Field:    "execution.tool_sequence",
					Message:  fmt.Sprintf("tool sequence entry %d must name a tool", i),
				})
			}
		}
		if strings.TrimSpace(c.ToolSequence[len(c.ToolSequence)-1]) != "submit_result" {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "tool_sequence_terminal_result",
				Field:    "execution.tool_sequence",
				Message:  "a closed tool sequence must end with submit_result",
			})
		}
		if !c.RequiresResult {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "tool_sequence_requires_result",
				Field:    "execution.requires_result",
				Message:  "a closed tool sequence requires requires_result=true",
			})
		}
		if len(c.ToolInputSequence) > 0 {
			if len(c.ToolInputSequence) != len(c.ToolSequence) {
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityError,
					Code:     "tool_input_sequence_length",
					Field:    "execution.tool_input_sequence",
					Message:  "tool_input_sequence must have one entry for every tool_sequence slot",
				})
			}
		}
	} else if len(c.ToolInputSequence) > 0 {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError,
			Code:     "tool_input_sequence_requires_tools",
			Field:    "execution.tool_input_sequence",
			Message:  "tool_input_sequence requires a non-empty tool_sequence",
		})
	}
	findings = append(findings, validateToolInputValueSequence(c)...)
	findings = append(findings, validateToolExpectedExitCodes(c)...)
	if len(c.Steps) > 0 {
		findings = append(findings, validateStructuredExecutionSteps(c.Steps)...)
		if c.RequiresVerification && !structuredStepsContainEffect(c.Steps, ExecutionEffectVerify) {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     "execution_steps_verifier_missing",
				Field:    "execution.steps",
				Message:  "structured execution with requires_verification=true must declare a verify step",
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

func validateToolInputValueSequence(c ExecutionContract) []ContractFinding {
	if c.ToolInputField == "" && len(c.ToolInputValueSequence) == 0 {
		return nil
	}
	if toolSequenceIsMixed(c.ToolSequence) {
		return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_input_value_sequence_mixed_tools", Field: "execution.tool_input_field", Message: "scalar tool input pinning is forbidden for a mixed tool_sequence; use tool_input_sequence for per-slot typed constraints"}}
	}
	if len(c.ToolSequence) == 0 || c.ToolInputField == "" || len(c.ToolInputValueSequence) != len(c.ToolSequence) {
		return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_input_value_sequence_invalid", Field: "execution.tool_input_value_sequence", Message: "tool_input_field and a value for every tool_sequence slot are required together"}}
	}
	for i, tool := range c.ToolSequence {
		if tool == "submit_result" && c.ToolInputField != "status" && c.ToolInputValueSequence[i] != "" {
			return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_input_value_sequence_terminal_field", Field: "execution.tool_input_value_sequence", Message: "only the status field may constrain a submit_result slot; other fields must use an empty wildcard"}}
		}
	}
	return nil
}

// toolSequenceIsMixed treats submit_result as a distinct tool: its result
// payload has different fields from a process or provider call. A scalar field
// can therefore never safely constrain both slots. Per-slot input constraints
// preserve command safety without guessing an integration-specific payload.
func toolSequenceIsMixed(sequence []string) bool {
	first := ""
	for _, tool := range sequence {
		tool = strings.TrimSpace(tool)
		if tool == "" {
			continue
		}
		if first == "" {
			first = tool
			continue
		}
		if tool != first {
			return true
		}
	}
	return false
}

func validateToolExpectedExitCodes(c ExecutionContract) []ContractFinding {
	if len(c.ToolExpectedExitCodes) == 0 {
		return nil
	}
	if len(c.ToolSequence) == 0 || len(c.ToolExpectedExitCodes) != len(c.ToolSequence) {
		return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_expected_exit_codes_invalid", Field: "execution.tool_expected_exit_codes", Message: "tool_expected_exit_codes requires one entry for every tool_sequence slot"}}
	}
	for index, codes := range c.ToolExpectedExitCodes {
		if len(codes) == 0 {
			continue
		}
		if strings.TrimSpace(c.ToolSequence[index]) == "submit_result" {
			return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_expected_exit_codes_terminal", Field: "execution.tool_expected_exit_codes", Message: "submit_result cannot declare expected process exit codes"}}
		}
		seen := make(map[int]bool, len(codes))
		for _, code := range codes {
			if code == 0 {
				return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_expected_exit_codes_zero", Field: "execution.tool_expected_exit_codes", Message: "expected exit codes must be non-zero; zero is always an ordinary successful tool result"}}
			}
			if seen[code] {
				return []ContractFinding{{Severity: FindingSeverityError, Code: "tool_expected_exit_codes_duplicate", Field: "execution.tool_expected_exit_codes", Message: fmt.Sprintf("expected exit code %d is duplicated for tool sequence position %d", code, index+1)}}
			}
			seen[code] = true
		}
	}
	return nil
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
