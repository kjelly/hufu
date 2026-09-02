package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// Compatibility fixtures under testdata/team-compat/<case>/team capture
// representative current team shapes (Specification 04 §10 / Specification 05
// Phase 0 in spec.md). Each fixture's resolved effective semantics are pinned
// to testdata/team-compat/<case>/expected-effective.json so later work on the
// team-authoring/compiler pipeline can be checked for regressions instead of
// relying on manual diffing.
//
// Set UPDATE_TEAM_COMPAT_GOLDEN=1 to (re)generate the golden files after an
// intentional, reviewed behavior change.
func TestTeamCompatFixtures(t *testing.T) {
	root := filepath.Join("testdata", "team-compat")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var cases []string
	for _, entry := range entries {
		if entry.IsDir() {
			cases = append(cases, entry.Name())
		}
	}
	sort.Strings(cases)
	if len(cases) == 0 {
		t.Fatalf("no compatibility fixtures found under %s", root)
	}

	update := os.Getenv("UPDATE_TEAM_COMPAT_GOLDEN") == "1"

	for _, name := range cases {
		name := name
		t.Run(name, func(t *testing.T) {
			teamDir := filepath.Join(root, name, "team")
			session, err := LoadTeam(teamDir, nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatalf("load fixture team %q: %v", name, err)
			}

			snapshot, err := BuildCompatSnapshot(session)
			if err != nil {
				t.Fatalf("build compat snapshot for %q: %v", name, err)
			}
			got, err := snapshot.JSON()
			if err != nil {
				t.Fatalf("marshal compat snapshot for %q: %v", name, err)
			}

			goldenPath := filepath.Join(root, name, "expected-effective.json")
			if update {
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
				t.Errorf("compat snapshot for %q does not match %s (run with UPDATE_TEAM_COMPAT_GOLDEN=1 to update it if this change is intentional)\n--- got ---\n%s", name, goldenPath, got)
			}
		})
	}
}

// jsonEqual compares two JSON documents structurally so golden files stay
// stable across insignificant formatting differences. encoding/json sorts
// object keys when marshaling a map, so round-tripping through
// unmarshal→marshal normalizes both key order and whitespace.
func jsonEqual(t *testing.T, want, got []byte) bool {
	t.Helper()
	var w, g interface{}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("parse golden JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("parse generated JSON: %v", err)
	}
	wj, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("re-marshal golden JSON: %v", err)
	}
	gj, err := json.Marshal(g)
	if err != nil {
		t.Fatalf("re-marshal generated JSON: %v", err)
	}
	return string(wj) == string(gj)
}
