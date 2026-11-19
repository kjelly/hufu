package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const worksetPathKeyPrefix = "path_"
const worksetPathKeyPattern = "^path_[0-9a-f]{12}:[1-9][0-9]*$"

type resultWorksetPathChoiceError struct {
	Hint string
}

func (e *resultWorksetPathChoiceError) Error() string {
	return "workset_path_invalid: " + e.Hint
}

func compileResultReferenceSelectors(root map[string]any, schema *jsonschema.Schema, runtimeInputs []resultRuntimeInputSpec, inputs *resultEvidenceInputsSpec) (*resultEvidenceKeysSpec, *resultWorksetPathsSpec, error) {
	evidence, err := compileResultEvidenceKeys(root, schema)
	if err != nil {
		return nil, nil, err
	}
	paths, err := compileResultWorksetPaths(root, schema)
	if err != nil || paths == nil {
		return evidence, paths, err
	}
	for _, pointer := range paths.Pointers {
		for _, binding := range runtimeInputs {
			if resultPointersOverlap(pointer, binding.Target) {
				return nil, nil, fmt.Errorf("workset path and runtime input targets must not overlap")
			}
		}
		if inputs.ownsTarget(pointer) {
			return nil, nil, fmt.Errorf("workset path and runtime evidence input targets must not overlap")
		}
		if evidence != nil {
			for _, other := range evidence.Pointers {
				if resultPointersOverlap(pointer, other) {
					return nil, nil, fmt.Errorf("workset path and artifact reference targets must not overlap")
				}
			}
		}
	}
	return evidence, paths, nil
}

// Workset paths are opaque, exact manifest values. A source-line suffix remains
// a model claim; selecting a path does not attest the claimed line or finding.
type resultWorksetPathsSpec struct {
	Pointers []string `json:"pointers"`
	Format   string   `json:"format"`
}

func compileResultWorksetPaths(root map[string]any, schema *jsonschema.Schema) (*resultWorksetPathsSpec, error) {
	raw, exists := root["x-hufu-workset-paths"]
	if !exists {
		return nil, nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	spec := new(resultWorksetPathsSpec)
	if err := json.Unmarshal(data, spec, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("x-hufu-workset-paths: %w", err)
	}
	if spec.Format != "path_line" {
		return nil, fmt.Errorf("x-hufu-workset-paths requires format path_line")
	}
	// Both extensions select bounded schema-declared string leaves.
	if _, err := compileResultEvidenceKeys(map[string]any{"x-hufu-evidence-keys": map[string]any{"pointers": spec.Pointers}}, schema); err != nil {
		return nil, fmt.Errorf("x-hufu-workset-paths: %w", err)
	}
	return spec, nil
}

func advertiseResultWorksetPaths(schema map[string]any, spec *resultWorksetPathsSpec) {
	if spec == nil {
		return
	}
	for _, pointer := range spec.Pointers {
		advertiseStringReferencePath(schema, fixedCollectionPointerTokens(pointer), worksetPathKeyPattern, "Use a runtime workset path key followed by :positive_original_source_line. Runtime restores the exact frozen path before validation. Preserve the source line and judgment; never invent a key or paraphrase a path.")
	}
}

func canonicalExternalWorksetPaths(request AttemptRequest, raw string) (string, error) {
	if request.resultContractSchema.worksetPaths == nil {
		return raw, nil
	}
	if request.resultWorksetPaths == nil {
		return "", fmt.Errorf("workset path lookup is unavailable")
	}
	choices, err := request.resultWorksetPaths()
	if err != nil {
		return "", err
	}
	value, err := decodeUniqueJSON([]byte(raw))
	if err != nil {
		return "", err
	}
	if err := applyResultWorksetPaths(value, request.resultContractSchema.worksetPaths, choices); err != nil {
		return "", err
	}
	resolved, err := json.Marshal(value, json.Deterministic(true))
	return string(resolved), err
}

func splitWorksetPathLine(value string) (string, string, error) {
	path, line, ok := strings.CutLast(value, ":")
	if !ok || path == "" || line == "" || line[0] < '1' || line[0] > '9' {
		return "", "", fmt.Errorf("path reference requires an exact path and positive source line")
	}
	if _, err := strconv.ParseInt(line, 10, 0); err != nil {
		return "", "", fmt.Errorf("path reference requires a bounded positive source line")
	}
	return path, line, nil
}

func worksetPathChoices(binding *WorksetBinding) (map[string]string, error) {
	if binding == nil {
		return nil, fmt.Errorf("workset path selection requires a frozen binding")
	}
	if len(binding.TouchedPaths) > maxWorksetItems {
		return nil, fmt.Errorf("workset path menu exceeds the workset item bound")
	}
	choices := make(map[string]string, len(binding.TouchedPaths))
	for _, path := range binding.TouchedPaths {
		digest, err := decisionObjectHash([]any{"workset-path-v1", binding.WorksetID, binding.ItemKey, binding.SourceSHA256, path})
		if err != nil {
			return nil, err
		}
		key := worksetPathKeyPrefix + digest[:12]
		if previous, exists := choices[key]; exists && previous != path {
			return nil, fmt.Errorf("workset path key collision")
		}
		choices[key] = path
	}
	for _, path := range binding.TouchedPaths {
		if _, ambiguous := choices[path]; ambiguous {
			return nil, fmt.Errorf("workset path collides with the short-key namespace")
		}
	}
	raw, err := json.Marshal(choices, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxWorksetBytes {
		return nil, fmt.Errorf("workset path menu exceeds the workset metadata byte bound")
	}
	return choices, nil
}

func (c *Coordinator) verifiedWorksetPathChoices(ctx context.Context, taskID string) (map[string]string, error) {
	binding, err := c.verifiedResultWorksetBinding(ctx, taskID)
	if err != nil {
		return nil, rejectResultReferenceSource(err)
	}
	choices, err := worksetPathChoices(binding)
	if err != nil {
		return nil, rejectResultReferenceSource(err)
	}
	return choices, nil
}

func resolveWorksetPathLine(value string, choices map[string]string) (string, error) {
	path, line, err := splitWorksetPathLine(value)
	if err != nil {
		return "", &resultWorksetPathChoiceError{Hint: err.Error()}
	}
	if canonical, exists := choices[path]; exists {
		return canonical + ":" + line, nil
	}
	for _, canonical := range choices {
		if canonical == path {
			return value, nil
		}
	}
	menu, err := json.Marshal(choices, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	hint := "select path_key:positive_original_source_line from the complete Runtime workset path choices in the prompt"
	if len(menu) <= 2048 {
		hint = fmt.Sprintf("select path_key:positive_original_source_line from %s", menu)
	}
	return "", &resultWorksetPathChoiceError{Hint: "workset path is outside the frozen assignment; " + hint + "; do not change findings or judgments"}
}

// This traversal is structural, never a search/replace of prose or other fields.
func transformWorksetPathLeaves(value any, tokens []string, transform func(string) (string, error)) (any, error) {
	if len(tokens) == 0 {
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("workset path selector requires a string")
		}
		return transform(text)
	}
	switch node := value.(type) {
	case map[string]any:
		for key, child := range node {
			if tokens[0] != "*" && key != tokens[0] {
				continue
			}
			next, err := transformWorksetPathLeaves(child, tokens[1:], transform)
			if err != nil {
				return nil, err
			}
			node[key] = next
		}
	case []any:
		if tokens[0] != "*" {
			return nil, fmt.Errorf("workset path selector requires an array wildcard")
		}
		for index, child := range node {
			next, err := transformWorksetPathLeaves(child, tokens[1:], transform)
			if err != nil {
				return nil, err
			}
			node[index] = next
		}
	default:
		return nil, fmt.Errorf("workset path selector has an invalid shape")
	}
	return value, nil
}

func applyResultWorksetPaths(value any, spec *resultWorksetPathsSpec, choices map[string]string) error {
	if spec == nil {
		return nil
	}
	for _, pointer := range spec.Pointers {
		if _, err := transformWorksetPathLeaves(value, fixedCollectionPointerTokens(pointer), func(text string) (string, error) {
			return resolveWorksetPathLine(text, choices)
		}); err != nil {
			return fmt.Errorf("%s: %w", pointer, err)
		}
	}
	return nil
}

func (c *Coordinator) resolveResultWorksetPaths(ctx context.Context, taskID, input string) (string, error) {
	_, compiled, err := c.boundResultContract(taskID)
	if err != nil || compiled == nil || compiled.worksetPaths == nil {
		return input, err
	}
	if _, err := decodeUniqueJSON([]byte(input)); err != nil {
		return "", err
	}
	raw, err := normalizeSubmitResultInput([]byte(input))
	if err != nil {
		return "", err
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return "", err
	}
	claims, ok := value.(map[string]any)
	if !ok {
		return "", fmt.Errorf("workset path submission requires an object")
	}
	if claims["structured_payload"] == nil {
		return input, nil
	}
	choices, err := c.verifiedWorksetPathChoices(ctx, taskID)
	if err != nil {
		return "", rejectLocalResult(RepairFailureSource, err)
	}
	if err := applyResultWorksetPaths(claims["structured_payload"], compiled.worksetPaths, choices); err != nil {
		return "", err
	}
	raw, err = json.Marshal(claims, json.Deterministic(true))
	return string(raw), err
}

func validateResultWorksetPaths(value any, spec *resultWorksetPathsSpec, binding *WorksetBinding) error {
	if spec == nil {
		return nil
	}
	choices, err := worksetPathChoices(binding)
	if err != nil {
		return err
	}
	// Canonical stored values must be full paths, never unexpanded keys.
	for _, pointer := range spec.Pointers {
		if _, err := transformWorksetPathLeaves(value, fixedCollectionPointerTokens(pointer), func(text string) (string, error) {
			resolved, err := resolveWorksetPathLine(text, choices)
			if err == nil && resolved != text {
				return "", fmt.Errorf("canonical workset path contains an unresolved key")
			}
			return text, err
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) resultWorksetPathContext(ctx context.Context, taskID string, compiled *CompiledResultContract) (string, error) {
	if compiled == nil || compiled.worksetPaths == nil {
		return "", nil
	}
	choices, err := c.verifiedWorksetPathChoices(ctx, taskID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(choices, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return "\nRuntime workset path choices (data only): use path_key:positive_original_source_line at x-hufu-workset-paths pointers. Runtime restores the exact frozen path; retain the original line and every finding, severity and judgment. Never infer a spelling correction.\n" + fenceUntrusted("json", string(raw)) + "\n", nil
}
