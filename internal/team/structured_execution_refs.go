package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// StructuredFact is an immutable, typed value produced by one execution step.
// SHA256 makes fact handoff independently auditable without copying the value
// into coordinator prose.
type StructuredFact struct {
	Name   string `json:"name"`
	Schema string `json:"schema,omitempty"`
	SHA256 string `json:"sha256"`
	Value  any    `json:"value,omitempty"`
}

// StructuredFactRef is the receipt-safe identity of a produced fact. Secret
// values are represented only by this digest-bearing reference.
type StructuredFactRef struct {
	Name   string `json:"name"`
	Schema string `json:"schema,omitempty"`
	SHA256 string `json:"sha256"`
}

// StructuredOutputValue is the runtime-owned representation of a named task
// output made available to downstream typed references.
type StructuredOutputValue struct {
	Kind      ExecutionOutputKind `json:"kind"`
	Schema    string              `json:"schema,omitempty"`
	Scope     string              `json:"scope,omitempty"`
	Artifact  *ArtifactRef        `json:"artifact,omitempty"`
	Fact      *StructuredFact     `json:"fact,omitempty"`
	ReceiptID string              `json:"receipt_id,omitempty"`
}

func normalizedExecutionOutputKind(kind ExecutionOutputKind) ExecutionOutputKind {
	if kind == "" {
		return ExecutionOutputArtifact
	}
	return kind
}

func normalizedPolicyVerdict(verdict string) string {
	if strings.TrimSpace(verdict) == "" {
		return "allowed"
	}
	return strings.TrimSpace(verdict)
}

func normalizedExecutionFailureClass(class string) string {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "validation", "policy", "execution", "infrastructure", "approval":
		return strings.ToLower(strings.TrimSpace(class))
	default:
		return "execution"
	}
}

func cloneStructuredFacts(values map[string]StructuredFact) map[string]StructuredFact {
	clone := make(map[string]StructuredFact, len(values))
	for key, value := range values {
		value.Value = cloneTaskResultValue(value.Value)
		clone[key] = value
	}
	return clone
}

func (e *structuredExecutionRun) resolveStepInput(step ExecutionStep) (map[string]any, []ResolvedStepRef, error) {
	resolved := make(map[string]any, len(step.Input)+len(step.References))
	bindings := make([]ResolvedStepRef, 0, len(step.References))
	for key, value := range step.Input {
		resolved[key] = value
	}
	for _, reference := range step.References {
		var value any
		if reference.TaskID != "" {
			output, ok := e.request.UpstreamOutputs[reference.TaskID][reference.Output]
			if !ok {
				return nil, nil, fmt.Errorf("step %q reference %q has no successful upstream task output", step.ID, reference.Output)
			}
			binding := ResolvedStepRef{Target: reference.Target, ProducerTask: reference.TaskID, Output: reference.Output, Kind: normalizedExecutionOutputKind(reference.Kind), Schema: reference.Schema}
			switch normalizedExecutionOutputKind(reference.Kind) {
			case ExecutionOutputArtifact:
				if output.Artifact == nil || strings.TrimSpace(output.Artifact.ID) == "" {
					return nil, nil, fmt.Errorf("upstream task output %q is not an artifact", reference.Output)
				}
				// Only the opaque ID crosses the provider/tool input boundary.
				// Paths remain runtime metadata and cannot be recopied or mutated
				// by the coordinator model.
				value = output.Artifact.ID
				binding.RefID, binding.SHA256 = output.Artifact.ID, output.Artifact.SHA256
			case ExecutionOutputFact:
				if output.Fact == nil {
					return nil, nil, fmt.Errorf("upstream task output %q is not a fact", reference.Output)
				}
				value = output.Fact.Value
				binding.SHA256 = output.Fact.SHA256
			case ExecutionOutputReceipt:
				if output.ReceiptID == "" {
					return nil, nil, fmt.Errorf("upstream task output %q is not a receipt", reference.Output)
				}
				value = output.ReceiptID
				binding.RefID = output.ReceiptID
			}
			resolved[reference.Target] = value
			bindings = append(bindings, binding)
			continue
		}
		binding := ResolvedStepRef{Target: reference.Target, ProducerStep: reference.StepID, Output: reference.Output, Kind: normalizedExecutionOutputKind(reference.Kind), Schema: reference.Schema}
		switch normalizedExecutionOutputKind(reference.Kind) {
		case ExecutionOutputArtifact:
			artifact, ok := e.result.Artifacts[reference.Output]
			if !ok || strings.TrimSpace(artifact.ID) == "" {
				return nil, nil, fmt.Errorf("step %q reference %q has no successful upstream artifact", step.ID, reference.Output)
			}
			value = artifact.ID
			binding.RefID, binding.SHA256 = artifact.ID, artifact.SHA256
		case ExecutionOutputFact:
			fact, ok := e.result.Facts[reference.Output]
			if !ok {
				return nil, nil, fmt.Errorf("step %q reference %q has no successful upstream fact", step.ID, reference.Output)
			}
			value = fact.Value
			binding.SHA256 = fact.SHA256
		case ExecutionOutputReceipt:
			receipt, ok := e.stepReceipts[reference.StepID]
			if !ok || receipt.ExitCode != 0 {
				return nil, nil, fmt.Errorf("step %q reference %q has no successful upstream receipt", step.ID, reference.Output)
			}
			value = cloneExecutionStepReceipt(receipt)
			binding.RefID = receipt.ID
		default:
			return nil, nil, fmt.Errorf("step %q reference %q has invalid kind %q", step.ID, reference.Output, reference.Kind)
		}
		resolved[reference.Target] = value
		bindings = append(bindings, binding)
	}
	return resolved, bindings, nil
}

func (e *structuredExecutionRun) recordDeclaredOutputs(step ExecutionStep, stepResult ExecutionStepResult, receipt *ExecutionStepReceipt) error {
	for _, output := range step.Outputs {
		switch normalizedExecutionOutputKind(output.Kind) {
		case ExecutionOutputArtifact:
			artifact, ok := stepResult.Artifacts[output.Name]
			if !ok {
				return e.outputError(receipt, step, output, "immutable artifact")
			}
			if e.request.PublishArtifact != nil {
				published, err := e.request.PublishArtifact(e.ctx, artifact)
				if err != nil {
					return e.outputErrorCause(receipt, step, output, "coordinator-attested artifact", err)
				}
				artifact = published
			} else if strings.TrimSpace(artifact.SHA256) == "" {
				return e.outputError(receipt, step, output, "immutable artifact")
			}
			if artifact.Kind == "" {
				artifact.Kind = string(ExecutionOutputArtifact)
			}
			e.result.Artifacts[output.Name] = artifact
			receipt.Produced = append(receipt.Produced, artifact)
			if receipt.ProducedDigests == nil {
				receipt.ProducedDigests = make(map[string]string)
			}
			receipt.ProducedDigests[output.Name] = artifact.SHA256
		case ExecutionOutputFact:
			value, ok := stepResult.Facts[output.Name]
			if !ok {
				return e.outputError(receipt, step, output, "structured fact")
			}
			fact, err := newStructuredFact(output.Name, output.Schema, value)
			if err != nil {
				return e.outputError(receipt, step, output, err.Error())
			}
			e.result.Facts[output.Name] = fact
			receipt.ProducedFacts = append(receipt.ProducedFacts, StructuredFactRef{Name: fact.Name, Schema: fact.Schema, SHA256: fact.SHA256})
		case ExecutionOutputReceipt:
			// The runtime-created receipt for this invocation is the output; a
			// runner cannot provide or replace it.
		default:
			return e.outputError(receipt, step, output, "recognized output kind")
		}
	}
	return nil
}

func (e *structuredExecutionRun) outputError(receipt *ExecutionStepReceipt, step ExecutionStep, output ExecutionStepOutput, want string) error {
	err := fmt.Errorf("step %q did not return declared %s output %q", step.ID, want, output.Name)
	return e.outputErrorValue(receipt, err)
}

func (e *structuredExecutionRun) outputErrorCause(receipt *ExecutionStepReceipt, step ExecutionStep, output ExecutionStepOutput, want string, cause error) error {
	err := fmt.Errorf("step %q did not return declared %s output %q: %w", step.ID, want, output.Name, cause)
	return e.outputErrorValue(receipt, err)
}

func (e *structuredExecutionRun) outputErrorValue(receipt *ExecutionStepReceipt, err error) error {
	receipt.ExitCode = 1
	receipt.Stderr = err.Error()
	receipt.FailureClass = "execution"
	receipt.StderrRef = executionOutputRef(receipt.ID, "stderr", receipt.Stderr)
	return err
}

func newStructuredFact(name, schema string, value any) (StructuredFact, error) {
	if err := validateStructuredFactSchema(schema, value); err != nil {
		return StructuredFact{}, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return StructuredFact{}, fmt.Errorf("fact is not JSON-serializable: %w", err)
	}
	sum := sha256.Sum256(data)
	return StructuredFact{Name: name, Schema: schema, SHA256: hex.EncodeToString(sum[:]), Value: value}, nil
}

func validateStructuredFactSchema(schema string, value any) error {
	switch strings.ToLower(strings.TrimSpace(schema)) {
	case "", "any":
		return nil
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("fact does not match schema string")
		}
	case "boolean", "bool":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("fact does not match schema boolean")
		}
	case "integer", "int":
		switch number := value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		case float64:
			if math.Trunc(number) != number {
				return fmt.Errorf("fact does not match schema integer")
			}
		default:
			return fmt.Errorf("fact does not match schema integer")
		}
	case "number":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		default:
			return fmt.Errorf("fact does not match schema number")
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("fact does not match schema object")
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("fact does not match schema array")
		}
	}
	return nil
}

func executionOutputRef(receiptID, stream, value string) ArtifactRef {
	if value == "" {
		return ArtifactRef{}
	}
	sum := sha256.Sum256([]byte(value))
	return ArtifactRef{
		ID: receiptID + ":" + stream, Kind: "execution_output", Type: "text/plain",
		SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(value)), ToolCallID: receiptID,
	}
}

func namedArtifactDigests(artifacts map[string]ArtifactRef, names []string) map[string]string {
	digests := make(map[string]string, len(names))
	for _, name := range names {
		if artifact, ok := artifacts[name]; ok {
			digests[name] = artifact.SHA256
		}
	}
	return digests
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func validateStructuredStepReferences(steps []ExecutionStep) []ContractFinding {
	byID := make(map[string]ExecutionStep, len(steps))
	outputs := make(map[string]ExecutionStepOutput)
	producers := make(map[string]string)
	for _, step := range steps {
		byID[step.ID] = step
		for _, output := range step.Outputs {
			outputs[output.Name] = output
			producers[output.Name] = step.ID
		}
	}
	var findings []ContractFinding
	for i, step := range steps {
		path := fmt.Sprintf("execution.steps[%d].references", i)
		ancestors := structuredStepAncestors(steps, step.ID)
		targets := make(map[string]bool)
		for _, reference := range step.References {
			if strings.TrimSpace(reference.Target) == "" {
				findings = append(findings, structuredFinding(path, "execution_reference_target", "reference target is required"))
			}
			if _, exists := step.Input[reference.Target]; exists {
				findings = append(findings, structuredFinding(path, "execution_reference_overwrite", fmt.Sprintf("reference target %q must not overwrite literal input", reference.Target)))
			}
			if targets[reference.Target] {
				findings = append(findings, structuredFinding(path, "execution_reference_duplicate_target", fmt.Sprintf("reference target %q is duplicated", reference.Target)))
			}
			targets[reference.Target] = true
			if (reference.StepID == "") == (reference.TaskID == "") {
				findings = append(findings, structuredFinding(path, "execution_reference_source", "reference must declare exactly one of step_id or task_id"))
				continue
			}
			if reference.TaskID != "" {
				if normalizedExecutionOutputKind(reference.Kind) != ExecutionOutputArtifact && normalizedExecutionOutputKind(reference.Kind) != ExecutionOutputFact && normalizedExecutionOutputKind(reference.Kind) != ExecutionOutputReceipt {
					findings = append(findings, structuredFinding(path, "execution_reference_kind", fmt.Sprintf("task reference %q has invalid kind %q", reference.Output, reference.Kind)))
				}
				if reference.Scope != "" && reference.Scope != "task" && reference.Scope != "secret" {
					findings = append(findings, structuredFinding(path, "execution_reference_scope", fmt.Sprintf("reference %q has invalid scope %q", reference.Output, reference.Scope)))
				}
				continue
			}
			producerID, exists := producers[reference.Output]
			if !exists || producerID != reference.StepID {
				findings = append(findings, structuredFinding(path, "execution_reference_missing_output", fmt.Sprintf("reference %q is not declared by step %q", reference.Output, reference.StepID)))
				continue
			}
			if _, exists := byID[reference.StepID]; !exists || !ancestors[reference.StepID] {
				findings = append(findings, structuredFinding(path, "execution_reference_not_upstream", fmt.Sprintf("reference step %q must be a successful dependency ancestor of %q", reference.StepID, step.ID)))
			}
			output := outputs[reference.Output]
			if normalizedExecutionOutputKind(output.Kind) != normalizedExecutionOutputKind(reference.Kind) {
				findings = append(findings, structuredFinding(path, "execution_reference_kind", fmt.Sprintf("reference %q kind %q does not match output kind %q", reference.Output, reference.Kind, normalizedExecutionOutputKind(output.Kind))))
			}
			if reference.Schema != "" && output.Schema != reference.Schema {
				findings = append(findings, structuredFinding(path, "execution_reference_schema", fmt.Sprintf("reference %q schema %q does not match output schema %q", reference.Output, reference.Schema, output.Schema)))
			}
			if reference.Scope != "" && reference.Scope != "task" && reference.Scope != "secret" {
				findings = append(findings, structuredFinding(path, "execution_reference_scope", fmt.Sprintf("reference %q has invalid scope %q", reference.Output, reference.Scope)))
			}
		}
	}
	return findings
}
