package teampkg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/team"
)

var (
	privateKeyPattern           = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	highConfidenceTokenPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{20,}\b`),
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`),
		regexp.MustCompile(`\bnpm_[A-Za-z0-9]{20,}\b`),
		regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	}
	secretEnvironmentKey = regexp.MustCompile(`(?i)(?:password|passwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)`)
	templatePlaceholder  = regexp.MustCompile(`^\{@\s*[A-Za-z0-9_.-]+\s*@\}$`)
	rawTemplatePattern   = regexp.MustCompile(`\{@[^{}\r\n]+@\}`)
)

const rawTemplateMarker = "__HUFU_PACKAGE_TEMPLATE_PLACEHOLDER__"

var forbiddenRuntimeDirectories = map[string]struct{}{
	"workspace": {}, ".hufu": {}, ".hufu-state": {}, "runtime-state": {}, ".runtime-state": {},
}

var forbiddenStateFiles = map[string]struct{}{
	"session.json": {}, "session_tree.json": {}, "context.sqlite": {}, "task_journal.jsonl": {},
	"events.jsonl": {}, "audit.jsonl": {},
}

var forbiddenCredentialFiles = map[string]struct{}{
	".netrc": {}, "_netrc": {}, ".npmrc": {}, ".pypirc": {}, "credentials": {},
	"credentials.json": {}, "application_default_credentials.json": {}, "authorized_keys": {},
	"id_rsa": {}, "id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {},
	"id_rsa.pub": {}, "id_dsa.pub": {}, "id_ecdsa.pub": {}, "id_ed25519.pub": {},
}

func preflightRawManifest(teamDir string) error {
	var found []string
	for _, name := range []string{"team.yaml", "team.yml"} {
		path := filepath.Join(teamDir, name)
		info, err := os.Lstat(path)
		switch {
		case err == nil:
			if !info.Mode().IsRegular() {
				return validationError(name, "path", "source_not_regular")
			}
			found = append(found, name)
		case errors.Is(err, os.ErrNotExist):
		default:
			return fmt.Errorf("inspect raw team manifest: %w", err)
		}
	}
	if len(found) != 1 {
		return validationError("", "team_manifest", "manifest_count")
	}
	data, err := os.ReadFile(filepath.Join(teamDir, found[0]))
	if err != nil {
		return fmt.Errorf("read raw team manifest: %w", err)
	}
	if err := scanSourceContent(found[0], data); err != nil {
		return err
	}
	return scanManifestFields(found[0], data)
}

func validateInventoryPolicy(inventory Inventory, session *team.TeamSession) error {
	files := make(map[string][]byte, len(inventory.Files))
	registry := newPathRegistry()
	var total int64
	for _, file := range inventory.Files {
		if err := registry.add(file.Path); err != nil {
			return err
		}
		if len(file.Data) > MaxFileBytes {
			return validationError(file.Path, "size", "file_too_large")
		}
		total += int64(len(file.Data))
		if total > MaxTotalBytes {
			return validationError(file.Path, "size", "total_too_large")
		}
		files[file.Path] = file.Data
	}
	if len(files)+2 > MaxEntries {
		return validationError("", validationFieldArchive, "too_many_entries")
	}
	if err := validateSourceFiles(inventory.TeamManifest, files); err != nil {
		return err
	}
	for capability, config := range session.Config.ActionProviders {
		if strings.EqualFold(strings.TrimSpace(config.Runtime), "golang") && isAbsolutePackageReference(config.Source) {
			return validationError(inventory.TeamManifest, "action-providers."+capability+".source", "absolute_package_reference")
		}
	}
	return nil
}

func validateSourceFiles(teamManifest string, files map[string][]byte) error {
	paths := make([]string, 0, len(files))
	for path := range files {
		if path == ManifestFilename || path == LockFilename {
			continue
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := validateSourceFilename(path); err != nil {
			return err
		}
		if err := scanSourceContent(path, files[path]); err != nil {
			return err
		}
		if isAgentMarkdown(path) {
			if err := scanAgentFrontmatterFields(path, files[path]); err != nil {
				return err
			}
		}
	}
	manifest, exists := files[teamManifest]
	if !exists {
		return validationError(teamManifest, "team_manifest", "manifest_missing")
	}
	return scanManifestFields(teamManifest, manifest)
}

func validateSourceFilename(value string) error {
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		lower := strings.ToLower(segment)
		if _, forbidden := forbiddenRuntimeDirectories[lower]; forbidden {
			return validationError(value, "path", "runtime_state_directory")
		}
	}
	base := strings.ToLower(segments[len(segments)-1])
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return validationError(value, "path", "environment_file")
	}
	if _, forbidden := forbiddenCredentialFiles[base]; forbidden {
		return validationError(value, "path", "credential_file")
	}
	if _, forbidden := forbiddenStateFiles[base]; forbidden {
		return validationError(value, "path", "runtime_state_file")
	}
	if strings.HasSuffix(base, ".key") || strings.Contains(base, "private-key") || strings.Contains(base, "private_key") {
		return validationError(value, "path", "private_key_file")
	}
	return nil
}

func scanSourceContent(path string, data []byte) error {
	if privateKeyPattern.Match(data) {
		return validationError(path, "content", "private_key_content")
	}
	for _, pattern := range highConfidenceTokenPatterns {
		if pattern.Match(data) {
			return validationError(path, "content", "known_token_prefix")
		}
	}
	return nil
}

func isAgentMarkdown(path string) bool {
	return !strings.Contains(path, "/") && strings.EqualFold(filepath.Ext(path), ".md") && !strings.EqualFold(path, "README.md")
}

func scanAgentFrontmatterFields(path string, data []byte) error {
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return nil
	}
	rest := data[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil
	}
	probe := rawTemplatePattern.ReplaceAll(rest[:end], []byte(rawTemplateMarker))
	var document yaml.Node
	if err := yaml.Unmarshal(probe, &document); err != nil {
		return fmt.Errorf("scan agent frontmatter fields: %w", err)
	}
	if len(document.Content) == 0 {
		return nil
	}
	requires := mappingValue(document.Content[0], "requires")
	environment := mappingValue(requires, "environment")
	if environment == nil || environment.Kind != yaml.SequenceNode {
		return nil
	}
	for index, entry := range environment.Content {
		if entry.Kind != yaml.ScalarNode {
			continue
		}
		name, value, assigned := strings.Cut(strings.TrimSpace(entry.Value), "=")
		if !assigned || !secretEnvironmentKey.MatchString(strings.TrimSpace(name)) {
			continue
		}
		value = strings.TrimSpace(value)
		if value != "" && value != rawTemplateMarker && !templatePlaceholder.MatchString(value) {
			return validationError(path, fmt.Sprintf("requires.environment[%d]", index), "literal_environment_secret")
		}
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

func scanManifestFields(path string, data []byte) error {
	probe := rawTemplatePattern.ReplaceAll(data, []byte(rawTemplateMarker))
	decoder := yaml.NewDecoder(bytes.NewReader(probe))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("scan raw team manifest fields: %w", err)
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return validationError(path, "document", "multiple_yaml_documents")
		}
		return fmt.Errorf("scan raw team manifest fields: %w", err)
	}
	if len(document.Content) == 0 {
		return nil
	}
	return scanYAMLNode(path, document.Content[0], nil)
}

func scanYAMLNode(path string, node *yaml.Node, ancestors []string) error {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			name := key.Value
			field := strings.Join(append(ancestors, name), ".")
			if name == "provider-api-key" && isProviderAPIKeyField(ancestors) && scalarIsLiteral(value) {
				return validationError(path, field, "literal_provider_api_key")
			}
			if name == "source" && underNamedSection(ancestors, "action-providers") && value.Kind == yaml.ScalarNode && isAbsolutePackageReference(value.Value) {
				return validationError(path, field, "absolute_package_reference")
			}
			if name == "schema" && len(ancestors) > 0 && ancestors[len(ancestors)-1] == "result-contract" && value.Kind == yaml.ScalarNode && isAbsolutePackageReference(value.Value) {
				return validationError(path, field, "absolute_package_reference")
			}
			if name == "environment" && underNamedSection(ancestors, "providers", "backends", "mcp-servers") {
				if err := scanEnvironmentMapping(path, field, value); err != nil {
					return err
				}
			}
			if err := scanYAMLNode(path, value, append(ancestors, name)); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := scanYAMLNode(path, child, append(ancestors, fmt.Sprintf("[%d]", index))); err != nil {
				return err
			}
		}
	}
	return nil
}

func scanEnvironmentMapping(path, field string, node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		key, value := node.Content[index], node.Content[index+1]
		if secretEnvironmentKey.MatchString(key.Value) && scalarIsLiteral(value) {
			return validationError(path, field+"."+key.Value, "literal_environment_secret")
		}
	}
	return nil
}

func scalarIsLiteral(node *yaml.Node) bool {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag == "!!null" {
		return false
	}
	value := strings.TrimSpace(node.Value)
	return value != "" && value != rawTemplateMarker && !templatePlaceholder.MatchString(value)
}

func isProviderAPIKeyField(ancestors []string) bool {
	return len(ancestors) == 0 || (len(ancestors) == 1 && ancestors[0] == "spec") || underNamedSection(ancestors, "providers", "backends")
}

func underNamedSection(ancestors []string, sections ...string) bool {
	if len(ancestors) < 2 {
		return false
	}
	sectionIndex := len(ancestors) - 2
	if sectionIndex != 0 && (sectionIndex != 1 || ancestors[0] != "spec") {
		return false
	}
	return slices.Contains(sections, ancestors[sectionIndex])
}

func isAbsolutePackageReference(value string) bool {
	value = strings.TrimSpace(value)
	return filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.HasPrefix(value, `\`) || hasWindowsDrivePrefix(value)
}
