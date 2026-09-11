package team

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// bundledTeamsDir locates the real .agent-teams/ directory relative to this
// source file (runtime.Caller), independent of the test binary's working
// directory — the same pattern hufu_coding_team_config_test.go's
// hufuCodingTeamDir uses for a single bundled team.
func bundledTeamsDir(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(sourceFile), "..", "..", ".agent-teams")
}

// TestBundledTeamsNormalize pins every real bundled team's resolved effective
// semantics to a golden fixture under
// testdata/bundled-teams-compat/<name>/expected-effective.json, the same
// CompatSnapshot mechanism TestTeamCompatFixtures uses for the synthetic
// testdata/team-compat/* cases (Versioned AgentTeam Schema plan, PR-1). This
// is the baseline the schema-versioning loader/normalizer work must not
// silently change: legacy-flat and (once it exists) v1alpha1 parsing of the
// same bundled team must normalize to an identical snapshot.
//
// Set UPDATE_TEAM_COMPAT_GOLDEN=1 to (re)generate the golden files after an
// intentional, reviewed behavior change — the same env var
// TestTeamCompatFixtures uses, since both are the one golden-fixture
// mechanism.
func TestBundledTeamsNormalize(t *testing.T) {
	root := bundledTeamsDir(t)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no bundled teams found under %s", root)
	}

	update := os.Getenv("UPDATE_TEAM_COMPAT_GOLDEN") == "1"

	for _, name := range names {
		name := name
		t.Run(name, func(t *testing.T) {
			session, err := LoadTeam(filepath.Join(root, name), nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatalf("load bundled team %q: %v", name, err)
			}

			snapshot, err := BuildCompatSnapshot(session)
			if err != nil {
				t.Fatalf("build compat snapshot for %q: %v", name, err)
			}
			got, err := snapshot.JSON()
			if err != nil {
				t.Fatalf("marshal compat snapshot for %q: %v", name, err)
			}

			goldenDir := filepath.Join("testdata", "bundled-teams-compat", name)
			goldenPath := filepath.Join(goldenDir, "expected-effective.json")
			if update {
				if err := os.MkdirAll(goldenDir, 0o755); err != nil {
					t.Fatalf("create golden dir %s: %v", goldenDir, err)
				}
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden file %s: %v", goldenPath, err)
				}
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden file %s (run with UPDATE_TEAM_COMPAT_GOLDEN=1 to create it): %v", goldenPath, err)
			}
			if !jsonEqual(t, want, got) {
				t.Errorf("compat snapshot for bundled team %q does not match %s (run with UPDATE_TEAM_COMPAT_GOLDEN=1 to update it if this change is intentional)\n--- got ---\n%s", name, goldenPath, got)
			}
		})
	}
}
