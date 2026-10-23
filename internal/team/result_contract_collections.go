package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxResultFixedCollections    = 8
	maxResultFixedCollectionRows = 2048
)

// A fixed collection derives its membership from accepted evidence, while the
// model supplies only schema-declared decisions for runtime-issued row keys.
// All domain vocabulary, including the finite choices, stays in the schema.
type resultFixedCollectionSpec struct {
	DecisionGroups  []resultDecisionGroup `json:"decision_groups,omitempty"`
	ItemsPointer    string                `json:"items_pointer"`
	SourceContracts []string              `json:"source_contracts"`
	SourceScope     string                `json:"source_scope,omitempty"`
	SourcePointer   string                `json:"source_pointer"`
	TaskIDField     string                `json:"task_id_field,omitempty"`
	IndexField      string                `json:"index_field,omitempty"`
	ValueBindings   map[string]string     `json:"value_bindings,omitempty"`
	ChoiceFields    []string              `json:"choice_fields"`
	decisionSchema  map[string]any
	arraySchema     *jsonschema.Schema
}

// ResultCollectionBinding is runtime-owned provenance for an assembled array.
// It is stored with the payload, so replay cannot apply old decisions to a new
// source with the same row count and IDs.
type ResultCollectionBinding struct {
	Decisions    []ResultDecisionProvenance `json:"decisions,omitempty"`
	ItemsPointer string                     `json:"items_pointer"`
	Sources      []ResultCollectionSource   `json:"sources"`
}

type ResultCollectionSource struct {
	TaskID        string `json:"task_id"`
	PayloadSHA256 string `json:"payload_sha256"`
}

type resultFixedCollectionRow struct {
	SourceTask  string         `json:"-"`
	SourceHash  string         `json:"-"`
	SourceIndex int            `json:"-"`
	Finding     any            `json:"-"`
	Key         string         `json:"key"`
	Fixed       map[string]any `json:"fixed"`
}

type resultFixedCollectionPlan struct {
	spec       resultFixedCollectionSpec
	binding    ResultCollectionBinding
	rows       []resultFixedCollectionRow
	fixedBytes int
}

func compileResultFixedCollections(root map[string]any) ([]resultFixedCollectionSpec, error) {
	raw, exists := root["x-hufu-fixed-collections"]
	if !exists {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var specs []resultFixedCollectionSpec
	if err := json.Unmarshal(data, &specs, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-fixed-collections: %w", err)
	}
	if len(specs) == 0 || len(specs) > maxResultFixedCollections {
		return nil, fmt.Errorf("x-hufu-fixed-collections requires 1–%d declarations", maxResultFixedCollections)
	}
	for i := range specs {
		if err := specs[i].compile(root); err != nil {
			return nil, fmt.Errorf("x-hufu-fixed-collections[%d]: %w", i, err)
		}
		for _, earlier := range specs[:i] {
			if specs[i].ItemsPointer == earlier.ItemsPointer || strings.HasPrefix(specs[i].ItemsPointer, earlier.ItemsPointer+"/") || strings.HasPrefix(earlier.ItemsPointer, specs[i].ItemsPointer+"/") {
				return nil, fmt.Errorf("fixed collection targets must not overlap")
			}
		}
	}
	return specs, nil
}

func (s *resultFixedCollectionSpec) compile(root map[string]any) error {
	if s.SourceScope != "" && s.SourceScope != "evidence_from" && s.SourceScope != "dependency_results" {
		return fmt.Errorf("source_scope must be evidence_from or dependency_results")
	}
	for _, pointer := range []string{s.ItemsPointer, s.SourcePointer} {
		if pointer == "" || len(pointer) > maxTaskResultPointerBytes {
			return fmt.Errorf("bounded, non-empty JSON pointers are required")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return err
		}
	}
	if len(s.SourceContracts) == 0 || len(s.SourceContracts) > maxConfiguredEvidenceSources {
		return fmt.Errorf("a bounded source_contracts list is required")
	}
	seen := make(map[string]bool)
	for _, id := range s.SourceContracts {
		if strings.TrimSpace(id) == "" || len(id) > maxTaskResultPointerBytes || seen[id] {
			return fmt.Errorf("source_contracts requires unique, non-empty schema IDs")
		}
		seen[id] = true
	}
	array, err := fixedCollectionSchemaAt(root, s.ItemsPointer)
	if err != nil || array["type"] != "array" {
		return fmt.Errorf("items_pointer must select a directly declared array schema")
	}
	item, ok := array["items"].(map[string]any)
	if !ok || item["type"] != "object" || item["additionalProperties"] != false || item["$ref"] != nil {
		return fmt.Errorf("collection items must be directly declared closed objects")
	}
	properties, _ := item["properties"].(map[string]any)
	fixed := s.fixedFields()
	if len(fixed) == 0 || len(fixed) != len(s.ValueBindings)+boolInt(s.TaskIDField != "")+boolInt(s.IndexField != "") {
		return fmt.Errorf("distinct fixed fields are required")
	}
	for name := range fixed {
		if _, ok := properties[name]; !ok || strings.TrimSpace(name) == "" {
			return fmt.Errorf("fixed fields must name declared item properties")
		}
	}
	for _, pointer := range s.ValueBindings {
		if len(pointer) > maxTaskResultPointerBytes {
			return fmt.Errorf("value binding pointer is oversized")
		}
		if err := validateJSONPointer(pointer); err != nil {
			return err
		}
	}
	required, _ := item["required"].([]any)
	if err := s.compileChoices(properties, fixed, required); err != nil {
		return err
	}
	if err := s.compileDecisionGroups(properties); err != nil {
		return err
	}
	// Keep all conditionals for canonical validation after assembly. The
	// submission projection removes only fields that the runtime owns.
	s.decisionSchema = maps.Clone(item)
	if len(s.DecisionGroups) > 0 {
		for _, key := range []string{"allOf", "anyOf", "oneOf", "if", "then", "else", "dependentSchemas", "dependentRequired"} {
			delete(s.decisionSchema, key)
		}
	}
	decisionProperties := maps.Clone(properties)
	for _, group := range s.DecisionGroups {
		for _, name := range group.Outputs {
			fixed[name] = true
		}
	}
	for name := range fixed {
		delete(decisionProperties, name)
	}
	s.decisionSchema["properties"] = decisionProperties
	decisionRequired := make([]any, 0, len(required))
	for _, name := range required {
		if text, ok := name.(string); ok && !fixed[text] {
			decisionRequired = append(decisionRequired, name)
		}
	}
	s.decisionSchema["required"] = decisionRequired
	return nil
}

func (s resultFixedCollectionSpec) compileChoices(properties map[string]any, fixed map[string]bool, required []any) error {
	if len(s.ChoiceFields) == 0 || len(s.ChoiceFields) > 16 {
		return fmt.Errorf("choice_fields must name finite model decisions")
	}
	seen := make(map[string]bool)
	for _, name := range s.ChoiceFields {
		field, _ := properties[name].(map[string]any)
		options, _ := field["enum"].([]any)
		if fixed[name] || seen[name] || !slices.Contains(required, any(name)) || field["type"] != "string" || len(options) < 2 || len(options) > 21 {
			return fmt.Errorf("choice fields require distinct string enums with 2–21 options")
		}
		seen[name] = true
		values := make(map[string]bool)
		for _, option := range options {
			value, ok := option.(string)
			if !ok || value == "" || values[value] || strings.Trim(value, "abcdefghijklmnopqrstuvwxyz0123456789_") != "" {
				return fmt.Errorf("choice options must be unique lowercase or snake_case machine IDs")
			}
			values[value] = true
		}
	}
	return nil
}

func (s resultFixedCollectionSpec) fixedFields() map[string]bool {
	names := make(map[string]bool)
	for name := range s.ValueBindings {
		names[name] = true
	}
	for _, name := range []string{s.TaskIDField, s.IndexField} {
		if name != "" {
			names[name] = true
		}
	}
	return names
}

// Object-only target paths keep assembly unambiguous. Complex schema branches
// and array parents are rejected at load time rather than guessed at runtime.
func fixedCollectionSchemaAt(root map[string]any, pointer string) (map[string]any, error) {
	current := root
	for _, name := range fixedCollectionPointerTokens(pointer) {
		properties, _ := current["properties"].(map[string]any)
		next, ok := properties[name].(map[string]any)
		if !ok || current["type"] != "object" || current["$ref"] != nil {
			return nil, fmt.Errorf("collection path must traverse directly declared object properties")
		}
		current = next
	}
	return current, nil
}

func validateFixedCollectionSourceContracts(contracts map[string]*CompiledResultContract) error {
	for _, id := range slices.Sorted(maps.Keys(contracts)) {
		for _, collection := range contracts[id].fixedCollections {
			for _, source := range collection.SourceContracts {
				if contracts[source] == nil {
					return fmt.Errorf("fixed collection in %s names unloaded source contract %s", id, source)
				}
			}
		}
	}
	return nil
}

func bindFixedCollectionSchemas(specs []resultFixedCollectionSpec, root *jsonschema.Schema) {
	for i := range specs {
		node := root
		for _, field := range fixedCollectionPointerTokens(specs[i].ItemsPointer) {
			node = node.Properties[field]
		}
		specs[i].arraySchema = node
	}
}

func fixedCollectionPointerTokens(pointer string) []string {
	tokens := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
	for i := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(tokens[i], "~1", "/"), "~0", "~")
	}
	return tokens
}

func buildResultFixedCollections(specs []resultFixedCollectionSpec, ids []string, results []TaskResult) ([]resultFixedCollectionPlan, error) {
	plans := make([]resultFixedCollectionPlan, 0, len(specs))
	for _, spec := range specs {
		sourceIDs := ids
		if spec.SourceScope == "dependency_results" {
			// These results are supplied by the runtime from this occurrence's
			// durable direct dependencies/evidence edges, never by the worker.
			// This lets one atomic DAG batch bind producers whose Todo IDs do
			// not exist yet when the coordinator submits that batch.
			sourceIDs = make([]string, 0, len(results))
			for _, result := range results {
				sourceIDs = append(sourceIDs, result.TaskID)
			}
		}
		if len(sourceIDs) > maxConfiguredEvidenceSources {
			return nil, fmt.Errorf("fixed collection evidence exceeds the source limit")
		}
		plan := resultFixedCollectionPlan{spec: spec, binding: ResultCollectionBinding{ItemsPointer: spec.ItemsPointer, Sources: []ResultCollectionSource{}}, rows: []resultFixedCollectionRow{}}
		seen := make(map[string]bool)
		for _, id := range sourceIDs {
			if id == "" || seen[id] {
				return nil, fmt.Errorf("fixed collection requires unique evidence_from IDs")
			}
			seen[id] = true
			index := slices.IndexFunc(results, func(r TaskResult) bool { return r.TaskID == id })
			if index < 0 || !taskResultStatusIsSuccessful(results[index].Status) {
				return nil, fmt.Errorf("fixed collection evidence %s has no accepted result", id)
			}
			payload := results[index].StructuredPayload
			if payload == nil || !slices.Contains(spec.SourceContracts, payload.Contract.ID) {
				continue
			}
			if err := plan.appendSource(id, payload); err != nil {
				return nil, fmt.Errorf("fixed collection %s: %w", spec.ItemsPointer, err)
			}
		}
		if len(plan.binding.Sources) == 0 {
			return nil, fmt.Errorf("fixed collection %s requires accepted evidence matching source_contracts", spec.ItemsPointer)
		}
		array := spec.arraySchema
		if array != nil && ((array.MinItems != nil && len(plan.rows) < *array.MinItems) || (array.MaxItems != nil && len(plan.rows) > *array.MaxItems)) {
			return nil, fmt.Errorf("fixed collection %s cardinality contradicts its array schema", spec.ItemsPointer)
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (p *resultFixedCollectionPlan) appendSource(id string, payload *ResultPayload) error {
	if len(payload.Value) > resultPayloadMaxBytes {
		return fmt.Errorf("source payload is oversized")
	}
	sum := sha256.Sum256(payload.Value)
	if payload.SHA256 != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("source %s payload hash mismatch", id)
	}
	value, err := decodeResultContractJSON(payload.Value)
	if err != nil {
		return err
	}
	raw, err := resolveJSONPointer(value, p.spec.SourcePointer)
	items, ok := raw.([]any)
	if err != nil || !ok || len(items)+len(p.rows) > maxResultFixedCollectionRows {
		return fmt.Errorf("source %s requires a bounded array at %s", id, p.spec.SourcePointer)
	}
	p.binding.Sources = append(p.binding.Sources, ResultCollectionSource{TaskID: id, PayloadSHA256: payload.SHA256})
	for i, item := range items {
		fixed := make(map[string]any)
		if p.spec.TaskIDField != "" {
			fixed[p.spec.TaskIDField] = id
		}
		if p.spec.IndexField != "" {
			fixed[p.spec.IndexField] = i
		}
		for field, pointer := range p.spec.ValueBindings {
			value, err := resolveJSONPointer(item, pointer)
			if err != nil {
				return fmt.Errorf("source %s item %d lacks a declared fixed value", id, i)
			}
			fixed[field] = value
		}
		if p.spec.arraySchema != nil {
			for field, value := range fixed {
				if err := p.spec.arraySchema.Items2020.Properties[field].Validate(value); err != nil {
					return fmt.Errorf("source %s item %d fixed field %s contradicts its schema", id, i, field)
				}
			}
		}
		encoded, err := json.Marshal(fixed)
		if err != nil {
			return err
		}
		p.fixedBytes += len(encoded)
		if p.fixedBytes > resultPayloadMaxBytes {
			return fmt.Errorf("fixed values exceed the result payload byte limit")
		}
		// A key binds the decision to both the source identity and content.
		// If evidence is replaced during an attempt, its old keys cannot be
		// silently reinterpreted against another equally sized inventory.
		digest := sha256.Sum256([]byte(id + "\x00" + payload.SHA256 + "\x00" + strconv.Itoa(i)))
		key := "item_" + strconv.Itoa(len(p.rows)) + "_" + hex.EncodeToString(digest[:])
		p.rows = append(p.rows, resultFixedCollectionRow{Key: key, Fixed: fixed, SourceTask: id, SourceHash: payload.SHA256, SourceIndex: i, Finding: item})
	}
	return nil
}

// assembleResultFixedCollections accepts keyed model decisions, never a
// model-authored array or model-authored identity fields. It runs before JSON
// Schema validation and hashing, so all consumers see the canonical array.
func assembleResultFixedCollections(raw []byte, plans []resultFixedCollectionPlan) ([]byte, []ResultCollectionBinding, error) {
	if len(plans) == 0 {
		return raw, nil, nil
	}
	if len(raw) > resultPayloadMaxBytes {
		return nil, nil, fmt.Errorf("fixed collection submission exceeds the byte limit")
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	bindings := make([]ResultCollectionBinding, 0, len(plans))
	for _, plan := range plans {
		rawDecisions, err := resolveJSONPointer(value, plan.spec.ItemsPointer)
		decisions, ok := rawDecisions.(map[string]any)
		if err != nil || !ok {
			return nil, nil, fmt.Errorf("%s requires an object keyed by runtime-issued item keys; submit decisions only, not an array or identity fields", plan.spec.ItemsPointer)
		}
		if len(decisions) != len(plan.rows) {
			return nil, nil, fmt.Errorf("%s requires exactly %d runtime-issued decision keys; got %d; resubmit using the fixed collection manifest", plan.spec.ItemsPointer, len(plan.rows), len(decisions))
		}
		rows := make([]any, 0, len(plan.rows))
		for _, row := range plan.rows {
			decision, ok := decisions[row.Key].(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%s/%s requires a decision object from the fixed collection manifest", plan.spec.ItemsPointer, row.Key)
			}
			merged := maps.Clone(decision)
			for field, fixed := range row.Fixed {
				if _, exists := decision[field]; exists {
					return nil, nil, fmt.Errorf("%s/%s contains runtime-owned field %s; omit it and resubmit decisions only", plan.spec.ItemsPointer, row.Key, field)
				}
				merged[field] = fixed
			}
			rows = append(rows, merged)
		}
		if err := setFixedCollectionValue(value, plan.spec.ItemsPointer, rows); err != nil {
			return nil, nil, err
		}
		bindings = append(bindings, plan.binding)
	}
	encoded, err := json.Marshal(value)
	return encoded, bindings, err
}

func setFixedCollectionValue(root any, pointer string, value any) error {
	tokens := fixedCollectionPointerTokens(pointer)
	current := root
	for i, token := range tokens {
		object, ok := current.(map[string]any)
		if !ok {
			return fmt.Errorf("fixed collection target must traverse objects")
		}
		if i == len(tokens)-1 {
			object[token] = value
			return nil
		}
		current = object[token]
	}
	return fmt.Errorf("fixed collection cannot replace the root")
}

func validateResultFixedCollections(value any, bindings []ResultCollectionBinding, plans []resultFixedCollectionPlan) error {
	if len(bindings) != len(plans) {
		return fmt.Errorf("fixed collection provenance is missing or unexpected")
	}
	for i, plan := range plans {
		if !reflect.DeepEqual(bindings[i], plan.binding) {
			return fmt.Errorf("fixed collection %s source provenance changed", plan.spec.ItemsPointer)
		}
		raw, err := resolveJSONPointer(value, plan.spec.ItemsPointer)
		rows, ok := raw.([]any)
		if err != nil || !ok || len(rows) != len(plan.rows) {
			return fmt.Errorf("fixed collection %s does not match runtime-owned membership", plan.spec.ItemsPointer)
		}
		for index, row := range plan.rows {
			object, _ := rows[index].(map[string]any)
			for name, expected := range row.Fixed {
				// Index values are decoded as json.Number on the canonical path.
				left, leftErr := json.Marshal(object[name], json.Deterministic(true))
				right, rightErr := json.Marshal(expected, json.Deterministic(true))
				if leftErr != nil || rightErr != nil || string(left) != string(right) {
					return fmt.Errorf("fixed collection %s item %d has changed runtime-owned fields", plan.spec.ItemsPointer, index)
				}
			}
		}
	}
	return nil
}
