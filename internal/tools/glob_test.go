//go:build linux || darwin
// +build linux darwin

package tools

import "testing"

func TestBuildGlobMatcher(t *testing.T) {
	tests := []struct {
		pattern string
		relPath string
		want    bool
	}{
		// Bare patterns match on basename at any depth.
		{"*.json", "a.json", true},
		{"*.json", "dir/sub/a.json", true},
		{"*.json", "a.jsonl", false},

		// `**` spans zero or more directories (rg/gitignore semantics).
		{"**/*.json", "a.json", true},
		{"**/*.json", "dir/a.json", true},
		{"**/*.json", "dir/sub/a.json", true},
		{"**/*.json", "a.jsonl", false},
		{"src/**/*.go", "src/a.go", true},
		{"src/**/*.go", "src/x/y/a.go", true},
		{"src/**/*.go", "other/a.go", false},
		{"a/**", "a/b/c", true},
		{"a/**", "b/c", false},

		// Single-star directory patterns stay anchored per segment.
		{"dir/*.json", "dir/a.json", true},
		{"dir/*.json", "dir/sub/a.json", false},
		{"dir/*.json", "a.json", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"|"+tt.relPath, func(t *testing.T) {
			matcher := buildGlobMatcher(tt.pattern)
			if got := matcher(tt.relPath); got != tt.want {
				t.Errorf("buildGlobMatcher(%q)(%q) = %v, want %v", tt.pattern, tt.relPath, got, tt.want)
			}
		})
	}
}
