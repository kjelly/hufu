package team

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// applyTemplate applies Go template processing to content using the provided variables.
// Template delimiters are {@ and @} to avoid conflicts with {{ }} syntaxes
// found in GitHub Actions, Hugo, and other tools.
// If vars is nil or empty, or the content contains no {@ delimiters, the content is returned as-is.
// Missing variables cause an error with a descriptive message.
// Dotted keys like "project.name" are supported: they are expanded into nested maps
// so that {@ .project.name @} works correctly.
func applyTemplate(content string, name string, vars map[string]string) (string, error) {
	if len(vars) == 0 || !strings.Contains(content, "{@") {
		return content, nil
	}

	tmpl, err := template.New(name).Delims("{@", "@}").Option("missingkey=error").Parse(content)
	if err != nil {
		return "", fmt.Errorf("template parse error in %s: %w", name, err)
	}

	data, err := expandToNestedMap(vars)
	if err != nil {
		return "", fmt.Errorf("template variable error in %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("template execution error in %s: %w", name, err)
	}

	return buf.String(), nil
}

// expandToNestedMap converts a flat map with dotted keys like "project.name"
// into a nested map structure so that Go templates can access them as
// {{.project.name}}. Top-level keys without dots are converted to string values.
// Returns an error if a key conflicts with an existing non-map value or vice versa
// (e.g., both "project" and "project.name" are present).
func expandToNestedMap(flat map[string]string) (map[string]interface{}, error) {
	root := make(map[string]interface{})
	for key, value := range flat {
		parts := strings.Split(key, ".")
		if len(parts) == 1 {
			if existing, ok := root[key]; ok {
				if _, ok := existing.(map[string]interface{}); ok {
					return nil, fmt.Errorf("variable %q conflicts: already expanded as a nested map from a dotted key like %q", key, key+".<child>")
				}
			}
			root[key] = value
			continue
		}
		current := root
		for i, part := range parts {
			if i == len(parts)-1 {
				if existing, ok := current[part]; ok {
					if _, isMap := existing.(map[string]interface{}); !isMap {
						return nil, fmt.Errorf("variable %q conflicts: key %q is already a scalar value from a non-dotted key", key, strings.Join(parts[:i+1], "."))
					}
				}
				current[part] = value
			} else {
				if next, ok := current[part]; ok {
					if m, ok := next.(map[string]interface{}); ok {
						current = m
					} else {
						return nil, fmt.Errorf("variable %q conflicts: key %q is already a scalar value from a non-dotted key", key, strings.Join(parts[:i+1], "."))
					}
				} else {
					newMap := make(map[string]interface{})
					current[part] = newMap
					current = newMap
				}
			}
		}
	}
	return root, nil
}
