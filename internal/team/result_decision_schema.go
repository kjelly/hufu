package team

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
)

const resultDecisionContextMax = 48 << 10

type resultDecisionGroup struct {
	ID           string                       `json:"id"`
	CatalogEntry string                       `json:"catalog_entry"`
	Outputs      []string                     `json:"outputs"`
	ValueMap     map[string]map[string]string `json:"value_map"`
	Context      struct {
		Mode     string `json:"mode"`
		MaxBytes int    `json:"max_bytes"`
	} `json:"context"`
	OnAbstained string `json:"on_abstained"`
	OnError     string `json:"on_error"`
}

func (s resultFixedCollectionSpec) compileDecisionGroups(properties map[string]any) error {
	seen, owned := map[string]bool{}, s.fixedFields()
	if len(s.DecisionGroups) > 16 {
		return fmt.Errorf("too many decision groups")
	}
	for _, g := range s.DecisionGroups {
		if g.ID == "" || len(g.ID) > 128 || strings.Trim(g.ID, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" || seen[g.ID] || g.CatalogEntry == "" || len(g.CatalogEntry) > 128 {
			return fmt.Errorf("invalid decision group identity")
		}
		seen[g.ID] = true
		if g.Context.Mode != "observed_artifact_reads" || g.Context.MaxBytes < 1 || g.Context.MaxBytes > resultDecisionContextMax || g.OnAbstained != "blocked" || g.OnError != "blocked" {
			return fmt.Errorf("invalid decision context or failure policy")
		}
		if len(g.Outputs) == 0 || len(g.Outputs) > 16 || len(g.ValueMap) < 2 || len(g.ValueMap) > 21 {
			return fmt.Errorf("invalid decision output mapping size")
		}
		for _, field := range g.Outputs {
			if owned[field] || !slices.Contains(s.ChoiceFields, field) {
				return fmt.Errorf("decision output must be a distinct choice field")
			}
			owned[field] = true
		}
		for choice, values := range g.ValueMap {
			if choice == "" || strings.Trim(choice, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" || len(values) != len(g.Outputs) {
				return fmt.Errorf("invalid decision mapping")
			}
			for _, field := range g.Outputs {
				schema, _ := properties[field].(map[string]any)
				options, _ := schema["enum"].([]any)
				if !slices.Contains(options, any(values[field])) {
					return fmt.Errorf("decision mapping contradicts field enum")
				}
			}
		}
	}
	return nil
}

func resultDecisionInputs() map[string]string {
	inputs := map[string]string{"binding_digest": "string"}
	for i := range 12 {
		inputs[fmt.Sprintf("context_%02d", i)] = "string"
	}
	return inputs
}

func hasResultDecisions(compiled *CompiledResultContract) bool {
	if compiled == nil {
		return false
	}
	for _, s := range compiled.fixedCollections {
		if len(s.DecisionGroups) > 0 {
			return true
		}
	}
	return false
}

func validateResultDecisionCatalog(session *TeamSession, def *agent.AgentDef, compiled *CompiledResultContract) error {
	if !hasResultDecisions(compiled) {
		return nil
	}
	if def == nil || agentUsesExternalBackend(session, def) || len(def.ExtraModels) > 0 || def.ForceMCP || session.Config.ForceMCP || !explicitlyDeclaresTool(def.Tools, decisionPrimitiveToolName) {
		return fmt.Errorf("bound decisions require a local worker with decision_primitive")
	}
	if slices.Contains(session.Config.ToolsDenied, decisionPrimitiveToolName) {
		return fmt.Errorf("bound decision tool is denied")
	}
	for _, s := range compiled.fixedCollections {
		for _, g := range s.DecisionGroups {
			e, ok := session.Config.DecisionPrimitives[g.CatalogEntry]
			if !ok || e.Kind != decisionrt.KindChoice || len(e.BlockOn) > 0 || !slices.Contains(e.Agents, def.Name) || !maps.Equal(e.Inputs, resultDecisionInputs()) || len(e.Options) != len(g.ValueMap) {
				return fmt.Errorf("decision group %s catalog contract or grant mismatch", g.ID)
			}
			for _, option := range e.Options {
				if _, ok := g.ValueMap[option.ID]; !ok {
					return fmt.Errorf("decision group has unmapped catalog option")
				}
			}
		}
	}
	return nil
}

func (c *Coordinator) boundDecisionEntry(name string) bool {
	if c == nil || c.session == nil {
		return false
	}
	for _, compiled := range c.session.ResultContracts {
		for _, s := range compiled.fixedCollections {
			for _, g := range s.DecisionGroups {
				if g.CatalogEntry == name {
					return true
				}
			}
		}
	}
	return false
}

// Runtime-owned input entries have a deliberately narrow, unambiguous shape.
func validateRuntimeEvidenceInputSchema(root map[string]any, spec *resultEvidenceInputsSpec) error {
	array, err := fixedCollectionSchemaAt(root, spec.ItemsPointer)
	if err != nil || array["type"] != "array" {
		return fmt.Errorf("runtime evidence inputs must select a directly declared array")
	}
	item, ok := array["items"].(map[string]any)
	if !ok || item["type"] != "object" || item["additionalProperties"] != false {
		return fmt.Errorf("runtime evidence entries must be closed objects")
	}
	props, _ := item["properties"].(map[string]any)
	fields := map[string]bool{}
	pointers := []string{spec.TaskIDPointer, spec.HashPointer}
	pointers = append(pointers, slices.Collect(maps.Keys(spec.ValueBindings))...)
	for _, pointer := range pointers {
		tokens := fixedCollectionPointerTokens(pointer)
		if len(tokens) != 1 || tokens[0] == "" || fields[tokens[0]] || props[tokens[0]] == nil {
			return fmt.Errorf("runtime evidence bindings require distinct direct entry properties")
		}
		fields[tokens[0]] = true
	}
	required, _ := item["required"].([]any)
	for _, field := range required {
		name, _ := field.(string)
		if !fields[name] {
			return fmt.Errorf("runtime evidence entry has an unbound required property")
		}
	}
	if spec.CompletePointer == spec.ItemsPointer || strings.HasPrefix(spec.CompletePointer, spec.ItemsPointer+"/") {
		return fmt.Errorf("runtime evidence completeness overlaps entries")
	}
	return nil
}

func assembleRuntimeEvidenceInputs(raw []byte, spec *resultEvidenceInputsSpec, ids []string, results []TaskResult) ([]byte, error) {
	if spec == nil || !spec.RuntimeOwned {
		return raw, nil
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, err
	}
	if _, err := resolveJSONPointer(value, spec.ItemsPointer); err == nil {
		return nil, fmt.Errorf("omit runtime-owned evidence inputs %s", spec.ItemsPointer)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("runtime evidence inputs require accepted sources")
	}
	entries := make([]any, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		index := slices.IndexFunc(results, func(r TaskResult) bool { return r.TaskID == id })
		if seen[id] || index < 0 || !taskResultStatusIsSuccessful(results[index].Status) || results[index].StructuredPayload == nil {
			return nil, fmt.Errorf("runtime evidence source unavailable")
		}
		seen[id] = true
		payload := results[index].StructuredPayload
		if decisionBytesHash(payload.Value) != payload.SHA256 {
			return nil, fmt.Errorf("runtime evidence source hash mismatch")
		}
		source, err := decodeResultContractJSON(payload.Value)
		if err != nil {
			return nil, err
		}
		entry := map[string]any{fixedCollectionPointerTokens(spec.TaskIDPointer)[0]: id, fixedCollectionPointerTokens(spec.HashPointer)[0]: payload.SHA256}
		for target, pointer := range spec.ValueBindings {
			v, err := resolveJSONPointer(source, pointer)
			if err != nil {
				return nil, err
			}
			entry[fixedCollectionPointerTokens(target)[0]] = v
		}
		entries = append(entries, entry)
	}
	if err := setFixedCollectionValue(value, spec.ItemsPointer, entries); err != nil {
		return nil, err
	}
	return json.Marshal(value, json.Deterministic(true))
}

func removeRuntimeEvidenceSchema(root map[string]any, spec *resultEvidenceInputsSpec) {
	if spec == nil || !spec.RuntimeOwned {
		return
	}
	tokens := fixedCollectionPointerTokens(spec.ItemsPointer)
	parent := root
	for _, name := range tokens[:len(tokens)-1] {
		props, _ := parent["properties"].(map[string]any)
		parent, _ = props[name].(map[string]any)
		if parent == nil {
			return
		}
	}
	field := tokens[len(tokens)-1]
	props, _ := parent["properties"].(map[string]any)
	delete(props, field)
	required, _ := parent["required"].([]any)
	parent["required"] = slices.DeleteFunc(required, func(v any) bool { return v == field })
}
