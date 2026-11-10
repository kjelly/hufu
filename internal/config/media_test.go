package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOperatorMediaPolicyLayers(t *testing.T) {
	for _, tt := range []struct {
		name, first, second string
		enabled             bool
		count               int
		wantError           bool
	}{
		{"lower-limit", "media: {enabled: true, image: {max-count: 4}}", "media: {enabled: true, image: {max-count: 2}}", true, 2, false},
		{"cannot-reenable", "media: {enabled: false}", "media: {enabled: true}", false, 0, false},
		{"invalid-restriction", "media: {enabled: true}", "media: {enabled: true, image: {max-cout: 1}}", false, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			c := &Config{}
			for index, text := range []string{tt.first, tt.second} {
				path := filepath.Join(root, []string{"operator.yaml", "project.yaml"}[index])
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				c.mergeFromFile(path)
			}
			policy, err := c.ResolveMediaPolicy()
			if (err != nil) != tt.wantError {
				t.Fatalf("resolve policy: %v", err)
			}
			if err != nil {
				return
			}
			if (policy != nil && policy.Enabled) != tt.enabled {
				t.Fatalf("enabled policy: %+v", policy)
			}
			if policy != nil && policy.Image.MaxCount != tt.count {
				t.Fatalf("effective limit: %+v", policy.Image)
			}
		})
	}
}
