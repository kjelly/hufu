package team

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Runtime inputs copy receipt-verified native outputs into a canonical result.
// The schema owns the source and target; workers must omit these fields.
type resultRuntimeInputSpec struct {
	Target      string `json:"target"`
	SourceTask  string `json:"source_task"`
	Output      string `json:"output"`
	valueSchema *jsonschema.Schema
}

func compileResultRuntimeInputs(root map[string]any) ([]resultRuntimeInputSpec, error) {
	raw, present := root["x-hufu-runtime-inputs"]
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var specs []resultRuntimeInputSpec
	if err := json.Unmarshal(encoded, &specs, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-runtime-inputs: %w", err)
	}
	if len(specs) == 0 || len(specs) > maxActionInputBindings {
		return nil, fmt.Errorf("x-hufu-runtime-inputs requires a bounded nonempty list")
	}
	for i := range specs {
		spec := &specs[i]
		spec.SourceTask = strings.TrimSpace(spec.SourceTask)
		spec.Output = strings.TrimSpace(spec.Output)
		if spec.SourceTask == "" || spec.Output == "" || len(spec.SourceTask) > maxTaskResultPointerBytes || len(spec.Output) > maxTaskResultPointerBytes {
			return nil, fmt.Errorf("runtime input %d requires bounded source_task and output names", i)
		}
		if spec.Target == "" || len(spec.Target) > maxTaskResultPointerBytes {
			return nil, fmt.Errorf("runtime input %d requires a bounded nonempty target pointer", i)
		}
		if err := validateJSONPointer(spec.Target); err != nil {
			return nil, fmt.Errorf("runtime input %d: %w", i, err)
		}
		if _, err := fixedCollectionSchemaAt(root, spec.Target); err != nil {
			return nil, fmt.Errorf("runtime input %d target must select directly declared properties: %w", i, err)
		}
		for _, previous := range specs[:i] {
			if resultPointersOverlap(previous.Target, spec.Target) {
				return nil, fmt.Errorf("runtime input targets must not overlap")
			}
		}
	}
	return specs, nil
}

func bindRuntimeInputSchemas(specs []resultRuntimeInputSpec, schema *jsonschema.Schema) {
	for i := range specs {
		node := schema
		for _, token := range fixedCollectionPointerTokens(specs[i].Target) {
			node = node.Properties[token]
		}
		specs[i].valueSchema = node
	}
}

func validateRuntimeInputSources(session *TeamSession, contracts map[string]*CompiledResultContract, agents map[string]ResultContractRef) error {
	sources := make(map[string]TaskDef)
	for _, task := range session.ContractTasks {
		sources[strings.ToLower(task.ID)] = task
	}
	for id, compiled := range contracts {
		for _, spec := range compiled.runtimeInputs {
			source, exists := sources[strings.ToLower(spec.SourceTask)]
			if !exists || source.Action == nil {
				return fmt.Errorf("schema %q runtime input requires a declared native source task %q", id, spec.SourceTask)
			}
		}
	}
	for _, task := range session.ContractTasks {
		ref := task.ResultContract
		if ref == nil {
			if value, ok := agents[strings.ToLower(task.Agent)]; ok {
				ref = new(value)
			}
		}
		if ref == nil || contracts[ref.ID] == nil || len(contracts[ref.ID].runtimeInputs) == 0 {
			continue
		}
		if task.Action != nil {
			return fmt.Errorf("runtime result inputs require a model-submitted result, not action task %q", task.ID)
		}
	}
	return nil
}

func (c *Coordinator) resultRuntimeInputValues(compiled *CompiledResultContract) (map[string]any, error) {
	if compiled == nil || len(compiled.runtimeInputs) == 0 {
		return nil, nil
	}
	values := make(map[string]any, len(compiled.runtimeInputs))
	for _, spec := range compiled.runtimeInputs {
		value, err := c.resolveRuntimeOutputValue(FactRef{Name: spec.Target, TaskID: spec.SourceTask, RuntimeOutput: spec.Output}, spec.Output)
		if err != nil {
			return nil, fmt.Errorf("runtime input %s: %w", spec.Target, err)
		}
		// Detach without redacting again or rounding JSON numbers. A bound
		// value is already canonical and must not be changed by model coercion.
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode runtime input %s: %w", spec.Target, err)
		}
		value, err = decodeUniqueJSON(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode runtime input %s: %w", spec.Target, err)
		}
		if spec.valueSchema == nil || spec.valueSchema.Validate(value) != nil {
			return nil, fmt.Errorf("runtime input %s does not satisfy its declared schema", spec.Target)
		}
		values[spec.Target] = value
	}
	return values, nil
}

func assembleResultRuntimeInputs(raw []byte, specs []resultRuntimeInputSpec, values map[string]any) ([]byte, error) {
	if len(specs) == 0 {
		return raw, nil
	}
	if len(raw) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("runtime input payload exceeds the byte limit")
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		if _, err := resolveJSONPointer(value, spec.Target); err == nil {
			return nil, fmt.Errorf("%s is runtime-owned; omit it and submit decisions only", spec.Target)
		}
		source, exists := values[spec.Target]
		if !exists {
			return nil, fmt.Errorf("runtime input %s is unavailable", spec.Target)
		}
		if err := setRuntimeInputValue(value, spec.Target, source); err != nil {
			return nil, err
		}
	}
	encoded, err := json.Marshal(value)
	if err == nil && len(encoded) > resultPayloadMaxBytes {
		return nil, fmt.Errorf("runtime-bound payload exceeds the %d byte limit", resultPayloadMaxBytes)
	}
	return encoded, err
}

func setRuntimeInputValue(value any, pointer string, source any) error {
	tokens := fixedCollectionPointerTokens(pointer)
	for i, token := range tokens {
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("runtime input target must traverse objects")
		}
		if i == len(tokens)-1 {
			object[token] = source
			return nil
		}
		next, exists := object[token]
		if !exists {
			next = make(map[string]any)
			object[token] = next
		}
		value = next
	}
	return fmt.Errorf("runtime input cannot replace the root")
}

func validateResultRuntimeInputs(value any, specs []resultRuntimeInputSpec, values map[string]any) error {
	for _, spec := range specs {
		expected, exists := values[spec.Target]
		actual, err := resolveJSONPointer(value, spec.Target)
		if !exists || err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("runtime input %s differs from its receipt-verified source", spec.Target)
		}
	}
	return nil
}

func omitRuntimeResultInputs(root map[string]any, specs []resultRuntimeInputSpec) {
	for _, spec := range specs {
		removeRuntimeEvidenceSchema(root, &resultEvidenceInputsSpec{RuntimeOwned: true, ItemsPointer: spec.Target})
	}
	delete(root, "x-hufu-runtime-inputs")
}

func resultPointersOverlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
