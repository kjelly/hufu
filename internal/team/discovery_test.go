package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewTeamRegistry(t *testing.T) {
	tests := []struct {
		name        string
		searchPaths []string
		wantLen     int
	}{
		{
			name:        "empty paths",
			searchPaths: []string{},
			wantLen:     0,
		},
		{
			name:        "single path",
			searchPaths: []string{"/tmp/test1"},
			wantLen:     1,
		},
		{
			name:        "multiple paths",
			searchPaths: []string{"/tmp/test1", "/tmp/test2"},
			wantLen:     2,
		},
		{
			name:        "skip empty strings",
			searchPaths: []string{"/tmp/test1", "", "  ", "/tmp/test2"},
			wantLen:     2,
		},
		{
			name:        "tilde expansion",
			searchPaths: []string{"~/test"},
			wantLen:     1, // May fail if home dir not available, but should not panic
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewTeamRegistry(tt.searchPaths)
			if reg == nil {
				t.Fatal("NewTeamRegistry() returned nil")
			}
			if len(reg.searchPaths) != tt.wantLen {
				t.Errorf("NewTeamRegistry() searchPaths length = %d, want %d", len(reg.searchPaths), tt.wantLen)
			}
			if reg.teams == nil {
				t.Error("NewTeamRegistry() teams map is nil")
			}
		})
	}
}

func TestDefaultSearchPaths(t *testing.T) {
	paths := DefaultSearchPaths()
	if len(paths) == 0 {
		t.Error("DefaultSearchPaths() returned empty slice")
	}

	// Check that paths contain .agent-teams
	for _, p := range paths {
		if filepath.Base(p) != ".agent-teams" {
			t.Errorf("DefaultSearchPaths() path %q doesn't end with .agent-teams", p)
		}
	}
}

func TestTeamRegistryDiscover(t *testing.T) {
	// Create temporary directory structure
	tmpDir := t.TempDir()
	team1Dir := filepath.Join(tmpDir, "team1")
	team2Dir := filepath.Join(tmpDir, "team2")
	nonTeamDir := filepath.Join(tmpDir, "not-a-team")

	// Create team directories with team.yaml
	if err := os.MkdirAll(team1Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(team2Dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nonTeamDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Create team files
	if err := os.WriteFile(filepath.Join(team1Dir, "team.yaml"), []byte("name: team1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(team2Dir, "team.yml"), []byte("name: team2"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create registry and discover
	reg := NewTeamRegistry([]string{tmpDir})
	if err := reg.Discover(); err != nil {
		t.Fatalf("Discover() error = %v", err)
	}

	// Check discovered teams
	if !reg.HasTeam("team1") {
		t.Error("Discover() didn't find team1")
	}
	if !reg.HasTeam("team2") {
		t.Error("Discover() didn't find team2")
	}
	if reg.HasTeam("not-a-team") {
		t.Error("Discover() incorrectly found not-a-team")
	}

	// Check case insensitivity
	if !reg.HasTeam("TEAM1") {
		t.Error("HasTeam() should be case-insensitive")
	}
}

func TestTeamRegistryDiscoverNonExistent(t *testing.T) {
	reg := NewTeamRegistry([]string{"/nonexistent/path"})
	err := reg.Discover()
	if err != nil {
		t.Errorf("Discover() should not error on non-existent path, got %v", err)
	}
}

func TestTeamRegistryResolve(t *testing.T) {
	tmpDir := t.TempDir()
	teamDir := filepath.Join(tmpDir, "myteam")
	if err := os.MkdirAll(teamDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: myteam"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := NewTeamRegistry([]string{tmpDir})
	if err := reg.Discover(); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		team    string
		wantErr bool
	}{
		{"exact match", "myteam", false},
		{"case insensitive", "MYTEAM", false},
		{"not found", "unknown", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := reg.Resolve(tt.team)
			if tt.wantErr {
				if err == nil {
					t.Errorf("Resolve(%q) expected error", tt.team)
				}
			} else {
				if err != nil {
					t.Errorf("Resolve(%q) unexpected error = %v", tt.team, err)
				}
				if path != teamDir {
					t.Errorf("Resolve(%q) path = %q, want %q", tt.team, path, teamDir)
				}
			}
		})
	}
}

func TestTeamRegistryListTeams(t *testing.T) {
	tmpDir := t.TempDir()
	teams := []string{"alpha", "beta", "gamma"}
	for _, name := range teams {
		dir := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: "+name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	reg := NewTeamRegistry([]string{tmpDir})
	if err := reg.Discover(); err != nil {
		t.Fatal(err)
	}

	listed := reg.ListTeams()
	if len(listed) != len(teams) {
		t.Errorf("ListTeams() returned %d teams, want %d", len(listed), len(teams))
	}

	// Check all teams are in the list
	found := make(map[string]bool)
	for _, name := range listed {
		found[name] = true
	}
	for _, name := range teams {
		if !found[name] {
			t.Errorf("ListTeams() missing team %q", name)
		}
	}
}

func TestTeamRegistryTeamCount(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 5; i++ {
		name := "team" + string(rune('a'+i))
		dir := filepath.Join(tmpDir, name)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: "+name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	reg := NewTeamRegistry([]string{tmpDir})
	if err := reg.Discover(); err != nil {
		t.Fatal(err)
	}

	count := reg.TeamCount()
	if count != 5 {
		t.Errorf("TeamCount() = %d, want 5", count)
	}
}

func TestTeamRegistryDuplicateHandling(t *testing.T) {
	// Test that first discovered team wins in case of duplicates
	tmpDir1 := t.TempDir()
	tmpDir2 := t.TempDir()

	// Create same team name in both directories
	for _, dir := range []string{tmpDir1, tmpDir2} {
		teamDir := filepath.Join(dir, "shared")
		if err := os.MkdirAll(teamDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: shared"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	reg := NewTeamRegistry([]string{tmpDir1, tmpDir2})
	if err := reg.Discover(); err != nil {
		t.Fatal(err)
	}

	// Should only have one "shared" team
	if count := reg.TeamCount(); count != 1 {
		t.Errorf("TeamCount() = %d, want 1 (duplicates should be ignored)", count)
	}
}

func TestTeamRegistryHasTeamFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create directories with different team file variants
	yamlDir := filepath.Join(tmpDir, "yaml")
	ymlDir := filepath.Join(tmpDir, "yml")
	noTeamDir := filepath.Join(tmpDir, "noteam")
	mdOnlyDir := filepath.Join(tmpDir, "mdonly")
	emptyDir := filepath.Join(tmpDir, "empty")
	capsMDDir := filepath.Join(tmpDir, "capsmd")

	for _, dir := range []string{yamlDir, ymlDir, noTeamDir, mdOnlyDir, emptyDir, capsMDDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.WriteFile(filepath.Join(yamlDir, "team.yaml"), []byte("name: yaml"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ymlDir, "team.yml"), []byte("name: yml"), 0644); err != nil {
		t.Fatal(err)
	}
	// mdOnlyDir: no team.yaml, but contains an agent .md file
	if err := os.WriteFile(filepath.Join(mdOnlyDir, "agent.md"), []byte("---\nname: agent\nrole: worker\n---\nbody"), 0644); err != nil {
		t.Fatal(err)
	}
	// capsMDDir: uppercase .MD suffix should also count
	if err := os.WriteFile(filepath.Join(capsMDDir, "agent.MD"), []byte("plain"), 0644); err != nil {
		t.Fatal(err)
	}

	reg := NewTeamRegistry([]string{})

	tests := []struct {
		dir  string
		want bool
	}{
		{yamlDir, true},
		{ymlDir, true},
		{noTeamDir, false},
		{mdOnlyDir, true},
		{emptyDir, false},
		{capsMDDir, true},
	}

	for _, tt := range tests {
		t.Run(filepath.Base(tt.dir), func(t *testing.T) {
			got := reg.hasTeamFile(tt.dir)
			if got != tt.want {
				t.Errorf("hasTeamFile(%q) = %v, want %v", tt.dir, got, tt.want)
			}
		})
	}
}
