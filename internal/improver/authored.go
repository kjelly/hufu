package improver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/improve/paired"
)

// These are explicit profile inputs, not a blacklist. A new runtime capability
// cannot silently become available through a previously unknown field.
var profileTeamFields = strings.Fields(`name description preset agents vars skills skills-exclude
	model worker-model coordinator-model default-llm-backend context-window temperature max-tokens
	top-p top-k reasoning-effort max-rounds max-steps timeout max-retries max-concurrent tools
	no-net worker-memory memory-learning semantic-retrieval project-context`)

var profileAgentFields = strings.Fields(`name description preset role tools skills model context-window
	temperature max-tokens top-p top-k reasoning-effort timeout max-retries max-steps no-net
	instructions memory side-effect side_effect result-contract`)

func yamlObject(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("YAML is not UTF-8")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("YAML mapping required")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("exactly one YAML document required")
	}
	return result, nil
}

func manifestSpec(object map[string]any) (map[string]any, error) {
	if _, versioned := object["apiVersion"]; !versioned {
		return object, nil
	}
	if err := allowedKeys(object, []string{"apiVersion", "kind", "metadata", "spec"}, "manifest"); err != nil {
		return nil, err
	}
	spec, ok := object["spec"].(map[string]any)
	if !ok {
		return nil, errors.New("manifest.spec: expected mapping")
	}
	return spec, nil
}

func allowedKeys(object map[string]any, allowed []string, owner string) error {
	for _, key := range slices.Sorted(maps.Keys(object)) {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unsupported_profile: %s.%s", owner, key)
		}
	}
	return nil
}

func validateAuthoredTree(tree Tree) error {
	for _, name := range slices.Sorted(maps.Keys(tree.files)) {
		raw := tree.files[name]
		if name == "invariants.yaml" {
			return errors.New("unsupported_profile: invariant catalog")
		}
		switch {
		case name == "team.yaml" || name == "team.yml":
			object, err := yamlObject(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := validateManifest(object); err != nil {
				return err
			}
		case path.Dir(name) == "." && strings.HasSuffix(name, ".md"):
			header, _, err := splitMarkdown(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if len(header) == 0 {
				continue
			}
			fields, err := yamlObject(frontmatterContent(header))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if err := allowedKeys(fields, profileAgentFields, name); err != nil {
				return err
			}
			if err := validateSchemaPath(fields); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateManifest(object map[string]any) error {
	spec, err := manifestSpec(object)
	if err != nil {
		return err
	}
	if err := allowedKeys(spec, profileTeamFields, "team"); err != nil {
		return err
	}
	agents, _ := spec["agents"].(map[string]any)
	for name, declaration := range agents {
		if fields, ok := declaration.(map[string]any); ok {
			if err := allowedKeys(fields, profileAgentFields, "agents."+name); err != nil {
				return err
			}
			if err := validateSchemaPath(fields); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateSchemaPath(fields map[string]any) error {
	if value, present := fields["result-contract"]; present {
		contract, ok := value.(map[string]any)
		if !ok {
			return errors.New("result-contract: mapping required")
		}
		name, _ := contract["schema"].(string)
		if !paired.SafeRelativePath(name) || hasTemplate(name) {
			return errors.New("result-contract.schema: frozen relative path required")
		}
	}
	return nil
}

func hasTemplate(text string) bool {
	return strings.Contains(text, "{@") || strings.Contains(text, "@}")
}

// splitMarkdown preserves exact header bytes including both delimiters.
func splitMarkdown(raw []byte) (header, body []byte, err error) {
	if !bytes.HasPrefix(raw, []byte("---\n")) {
		return nil, raw, nil
	}
	index := bytes.Index(raw[4:], []byte("\n---\n"))
	if index < 0 {
		return nil, nil, errors.New("unterminated Markdown frontmatter")
	}
	offset := 4 + index + len("\n---\n")
	return raw[:offset], raw[offset:], nil
}

func frontmatterContent(header []byte) []byte {
	return header[4 : len(header)-len("\n---\n")]
}

func validateReplacement(text string) error {
	if len(text) > paired.MaxTextBytes || !utf8.ValidString(text) || strings.TrimSpace(text) == "" {
		return errors.New("surface: nonempty UTF-8 text of at most 64 KiB required")
	}
	if hasTemplate(text) {
		return errors.New("surface: replacement cannot contain template delimiters")
	}
	return nil
}
