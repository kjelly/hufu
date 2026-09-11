package team

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	LocationExact     = "exact"
	LocationRendered  = "rendered"
	LocationFieldOnly = "field_only"
)

// TeamSourceLocation identifies a stable authored or rendered source span.
type TeamSourceLocation struct {
	File   string
	Line   int
	Column int
	Status string
}

// AgentSource preserves the authored prompt body and its line offset.
type AgentSource struct {
	File       string
	Body       string
	BodyLine   int
	BodyStatus string
	Fields     map[string]TeamSourceLocation
}

// TeamSourceText preserves a rendered scalar together with the authored
// position from which its first character was decoded.
type TeamSourceText struct {
	Text   string
	File   string
	Line   int
	Column int
	Status string
	Block  bool
}

// TeamSourceIndex is a side-effect-free source map for lint projection.
type TeamSourceIndex struct {
	TeamDir       string
	ManifestFile  string
	Manifest      map[string]TeamSourceLocation
	ManifestText  map[string]TeamSourceText
	Agents        map[string]AgentSource
	SchemaVersion string
}

// BuildTeamSourceIndex reads only the team definition. It never creates a
// workspace or initializes runtime services.
func BuildTeamSourceIndex(teamDir string, vars map[string]string) (*TeamSourceIndex, error) {
	absDir, err := filepath.Abs(teamDir)
	if err != nil {
		return nil, fmt.Errorf("resolve team directory: %w", err)
	}
	index := &TeamSourceIndex{TeamDir: absDir, Manifest: make(map[string]TeamSourceLocation), ManifestText: make(map[string]TeamSourceText), Agents: make(map[string]AgentSource)}
	rawManifest, manifestFile, found, err := findTeamManifestFile(absDir)
	if err != nil {
		return nil, err
	}
	if found {
		index.ManifestFile = manifestFile
		rendered, renderErr := applyTemplate(string(rawManifest), manifestFile, vars)
		if renderErr != nil {
			return nil, fmt.Errorf("template error in team config: %w", renderErr)
		}
		root, status, parseErr := parseSourceYAML(rawManifest, []byte(rendered))
		if parseErr != nil {
			return nil, fmt.Errorf("parse team source index: %w", parseErr)
		}
		indexYAMLNode(index.Manifest, index.ManifestText, manifestFile, "", root, status)
		if rendered != string(rawManifest) && status == LocationExact {
			var renderedRoot yaml.Node
			if err := yaml.NewDecoder(strings.NewReader(rendered)).Decode(&renderedRoot); err == nil {
				renderedTexts := make(map[string]TeamSourceText)
				indexYAMLNode(make(map[string]TeamSourceLocation), renderedTexts, manifestFile, "", &renderedRoot, LocationRendered)
				for field, renderedText := range renderedTexts {
					if authoredText, ok := index.ManifestText[field]; !ok || authoredText.Text != renderedText.Text {
						index.ManifestText[field] = renderedText
					}
				}
			}
		}
		var envelope manifestEnvelope
		if err := yaml.Unmarshal([]byte(rendered), &envelope); err != nil {
			return nil, fmt.Errorf("detect team manifest envelope: %w", err)
		}
		if envelope.APIVersion == "" {
			index.SchemaVersion = SchemaVersionLegacyFlatV0
		} else {
			index.SchemaVersion = envelope.APIVersion
		}
	} else {
		index.SchemaVersion = SchemaVersionLegacyFlatV0
	}

	entries, err := os.ReadDir(absDir)
	if err != nil {
		return nil, fmt.Errorf("read team directory: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") || strings.EqualFold(entry.Name(), "README.md") {
			continue
		}
		path := filepath.Join(absDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read agent source %s: %w", entry.Name(), err)
		}
		source, err := indexAgentSource(entry.Name(), data, vars)
		if err != nil {
			return nil, err
		}
		index.Agents[strings.TrimSuffix(entry.Name(), ".md")] = source
	}
	return index, nil
}

func parseSourceYAML(authored, rendered []byte) (*yaml.Node, string, error) {
	var root yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(authored)).Decode(&root); err == nil {
		return &root, LocationExact, nil
	}
	if err := yaml.NewDecoder(bytes.NewReader(rendered)).Decode(&root); err != nil {
		return nil, "", err
	}
	return &root, LocationRendered, nil
}

func indexYAMLNode(target map[string]TeamSourceLocation, texts map[string]TeamSourceText, file, prefix string, node *yaml.Node, status string) {
	if node == nil {
		return
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		indexYAMLNode(target, texts, file, prefix, node.Content[0], status)
		return
	}
	if node.Kind == yaml.SequenceNode {
		for index, child := range node.Content {
			path := fmt.Sprintf("%s[%d]", prefix, index)
			target[path] = TeamSourceLocation{File: file, Line: child.Line, Column: child.Column, Status: status}
			indexYAMLNode(target, texts, file, path, child, status)
		}
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		path := key.Value
		if prefix != "" {
			path = prefix + "." + path
		}
		target[path] = TeamSourceLocation{File: file, Line: key.Line, Column: key.Column, Status: status}
		if texts != nil && value.Kind == yaml.ScalarNode {
			texts[path] = TeamSourceText{
				Text: value.Value, File: file, Line: value.Line, Column: value.Column,
				Status: status, Block: value.Style == yaml.LiteralStyle || value.Style == yaml.FoldedStyle,
			}
		}
		indexYAMLNode(target, texts, file, path, value, status)
	}
}

func indexAgentSource(file string, authored []byte, vars map[string]string) (AgentSource, error) {
	rendered, err := applyTemplate(string(authored), file, vars)
	if err != nil {
		return AgentSource{}, fmt.Errorf("template error in agent file %s: %w", file, err)
	}
	source := AgentSource{File: file, Body: rendered, BodyLine: 1, BodyStatus: LocationExact, Fields: make(map[string]TeamSourceLocation)}
	text := string(authored)
	if !strings.HasPrefix(text, "---\n") {
		if rendered != text {
			source.BodyStatus = LocationRendered
		}
		return source, nil
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		return AgentSource{}, fmt.Errorf("agent file %s has malformed frontmatter (missing closing '---')", file)
	}
	renderedRest := strings.TrimPrefix(rendered, "---\n")
	renderedEnd := strings.Index(renderedRest, "\n---\n")
	if renderedEnd < 0 {
		return AgentSource{}, fmt.Errorf("agent file %s has malformed rendered frontmatter", file)
	}
	root, status, err := parseSourceYAML([]byte(rest[:end]), []byte(renderedRest[:renderedEnd]))
	if err != nil {
		return AgentSource{}, fmt.Errorf("parse agent frontmatter %s: %w", file, err)
	}
	indexYAMLNode(source.Fields, nil, file, "", root, status)
	source.Body = renderedRest[renderedEnd+5:]
	source.BodyLine = 1 + strings.Count(rendered[:len(rendered)-len(source.Body)], "\n")
	if source.Body != rest[end+5:] {
		source.BodyStatus = LocationRendered
	}
	return source, nil
}

// Location resolves a runtime field path to the closest authored YAML node.
func (i *TeamSourceIndex) Location(field string) TeamSourceLocation {
	if i == nil {
		return TeamSourceLocation{Status: LocationFieldOnly}
	}
	field = strings.TrimSpace(field)
	if loc, ok := i.Manifest[field]; ok {
		return loc
	}
	if strings.HasPrefix(field, "spec.") {
		if loc, ok := i.Manifest[field]; ok {
			return loc
		}
	} else if loc, ok := i.Manifest["spec."+field]; ok {
		return loc
	}
	return TeamSourceLocation{File: i.ManifestFile, Status: LocationFieldOnly}
}
