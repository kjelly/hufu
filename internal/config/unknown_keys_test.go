package config

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUnknownConfigKeys(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{name: "declared keys", yaml: "model: m\nsidecar-model: s\ndecision-runtime:\n  backend: sidecar\nvars:\n  a: b\nexecution-routes: {}\n"},
		{name: "empty document", yaml: ""},
		{name: "team-only decision primitives", yaml: "model: m\ndecision-primitives:\n  critique_assessment:\n    backend: systemone\n", want: []string{"decision-primitives"}},
		{name: "sorted unknown keys", yaml: "zeta: 1\nmodel: m\nalpha: 2\n", want: []string{"alpha", "zeta"}},
		{name: "unparseable yaml", yaml: "invalid: yaml: content: ["},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unknownConfigKeys([]byte(tt.yaml)); !slices.Equal(got, tt.want) {
				t.Fatalf("unknownConfigKeys() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWarnUnknownConfigKeysOncePerFileWithHint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hufu.yaml")
	data := []byte("decision-primitives: {}\nstray: true\nmodel: m\n")
	var first bytes.Buffer
	warnUnknownConfigKeys(&first, path, data)
	lines := strings.Split(strings.TrimSpace(first.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"decision-primitives"`) || !strings.Contains(lines[0], "team.yaml") ||
		!strings.Contains(lines[1], `"stray"`) || !strings.Contains(lines[1], path) {
		t.Fatalf("warnings = %q", first.String())
	}
	var repeated bytes.Buffer
	warnUnknownConfigKeys(&repeated, path, data)
	if repeated.Len() != 0 {
		t.Fatalf("repeated load warned again: %q", repeated.String())
	}
}
