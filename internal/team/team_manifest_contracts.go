package team

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// manifestMapping resolves YAML merge keys with yaml.v3 precedence: explicit
// entries win and earlier mappings in a merge sequence win. Presence includes
// inherited keys, including null/empty values. Cyclic aliases fail closed.
func manifestMapping(node *yaml.Node, active map[*yaml.Node]bool) (map[string][2]*yaml.Node, error) {
	if node == nil {
		return nil, nil
	}
	if active[node] {
		return nil, fmt.Errorf("cyclic YAML mapping alias")
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		return manifestMapping(node.Alias, active)
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected YAML mapping")
	}
	result := make(map[string][2]*yaml.Node)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag != "!!merge" {
			continue
		}
		nodes := []*yaml.Node{value}
		if value.Kind == yaml.SequenceNode {
			nodes = value.Content
		}
		for _, source := range nodes {
			fields, err := manifestMapping(source, active)
			if err != nil {
				return nil, err
			}
			for name, pair := range fields {
				if _, exists := result[name]; !exists {
					result[name] = pair
				}
			}
		}
	}
	explicit := make(map[string]bool)
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if key.Tag == "!!merge" {
			continue
		}
		if explicit[key.Value] {
			return nil, fmt.Errorf("manifest field %q is duplicated", key.Value)
		}
		explicit[key.Value] = true
		result[key.Value] = [2]*yaml.Node{key, value}
	}
	return result, nil
}

func normalizeManifestContracts(data []byte, version string, yc *teamConfigYAML) error {
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return fmt.Errorf("scan contract declarations: %w", err)
	}
	if len(document.Content) == 0 {
		return nil
	}
	fields, err := manifestMapping(document.Content[0], make(map[*yaml.Node]bool))
	if err != nil {
		return err
	}
	prefix := ""
	if version == SchemaVersionV1Alpha1 {
		fields, err = manifestMapping(fields["spec"][1], make(map[*yaml.Node]bool))
		if err != nil {
			return err
		}
		prefix = "spec."
	}
	var declared []string
	for _, key := range []string{"contracts", "tasks"} {
		if _, present := fields[key]; present {
			declared = append(declared, prefix+key)
		}
	}
	if version == SchemaVersionLegacyFlatV0 {
		if advanced, present := fields["advanced"]; present && advanced[1].Tag != "!!null" {
			nested, mapErr := manifestMapping(advanced[1], make(map[*yaml.Node]bool))
			if mapErr != nil {
				return mapErr
			}
			if _, present := nested["tasks"]; present {
				declared = append(declared, "advanced.tasks")
			}
		}
	}
	if len(declared) > 1 {
		return fmt.Errorf("conflicting contract collections: %s; declare only one", strings.Join(declared, ", "))
	}
	if len(declared) == 0 {
		return nil
	}
	yc.contractCollection = declared[0]
	switch declared[0] {
	case "tasks", "spec.tasks":
		yc.Contracts = yc.Tasks
	case "advanced.tasks":
		yc.Contracts = yc.Advanced.Tasks
	}
	if len(yc.Contracts) == 0 {
		yc.Contracts = nil
	}
	yc.Tasks = nil
	yc.Advanced.Tasks = nil
	return nil
}

// contractCollectionPath projects internal validator paths without changing
// the runtime's established finding codes or the contract identity.
func (i *TeamSourceIndex) contractCollectionPath(field string) string {
	if i == nil || i.ContractCollection == "" {
		return field
	}
	if field == "tasks" {
		return i.ContractCollection
	}
	if strings.HasPrefix(field, "tasks[") || strings.HasPrefix(field, "tasks.") {
		return i.ContractCollection + strings.TrimPrefix(field, "tasks")
	}
	return field
}

func lintLegacyTasksKey(sources *TeamSourceIndex) []ContractFinding {
	if sources == nil || !slices.Contains([]string{"tasks", "spec.tasks", "advanced.tasks"}, sources.ContractCollection) {
		return nil
	}
	return []ContractFinding{{Code: FindingLegacyTasksKeyDeprecated, Severity: FindingSeverityWarning,
		Field: sources.ContractCollection, Message: "tasks is a deprecated name for static execution contracts", Hint: "rename tasks to contracts"}}
}
