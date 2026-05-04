package team

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// applyTemplate applies Go template processing to content using the provided variables.
// If vars is nil or empty, or the content contains no {{ delimiters, the content is returned as-is.
// Missing variables cause an error with a descriptive message.
func applyTemplate(content string, name string, vars map[string]string) (string, error) {
	if len(vars) == 0 || !strings.Contains(content, "{{") {
		return content, nil
	}

	tmpl, err := template.New(name).Option("missingkey=error").Parse(content)
	if err != nil {
		return "", fmt.Errorf("template parse error in %s: %w", name, err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("template execution error in %s: %w", name, err)
	}

	return buf.String(), nil
}
