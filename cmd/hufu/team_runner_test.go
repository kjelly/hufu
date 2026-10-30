package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestLoadTeamsForSegmentsPreservesStartupDiagnostic(t *testing.T) {
	original := opts
	t.Cleanup(func() { opts = original })
	opts = runOptions{unattended: true}
	for _, name := range []string{"missing", "invalid"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if name == "invalid" {
				teamDir := filepath.Join(root, name)
				if err := os.Mkdir(teamDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: invalid\nmax-concurrent: [invalid]\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			registry := team.NewTeamRegistry([]string{root})
			if err := registry.Discover(); err != nil {
				t.Fatal(err)
			}
			_, _, err := loadTeamsForSegments(t.Context(), []team.PromptSegment{{Type: team.SegmentSwitchTeam, Name: name}}, registry, nil, nil, nil, nil)
			if err == nil {
				t.Fatal("invalid team startup succeeded")
			}
			message := err.Error()
			if !strings.Contains(message, "failed to load team") {
				t.Errorf("startup context missing: %v", err)
			}
			if strings.Contains(message, "Verify the team exists") {
				t.Errorf("unrelated discovery advice appended: %v", err)
			}
			if name == "invalid" && !strings.Contains(message, "unmarshal") {
				t.Errorf("underlying parse diagnostic missing: %v", err)
			}
		})
	}
}
