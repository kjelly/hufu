package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/skill"
)

func TestSkillGraphCommandIsRegistered(t *testing.T) {
	command, _, err := skillCmd.Find([]string{"graph"})
	if err != nil {
		t.Fatalf("skillCmd.Find(graph) error = %v", err)
	}
	if command != skillGraphCmd {
		t.Fatalf("skill graph command = %v, want %v", command, skillGraphCmd)
	}
}

func TestRunSkillGraphMissingSnapshot(t *testing.T) {
	workspace := configureSkillGraphTest(t, "text", "", 0)
	if _, err := os.Stat(skill.SkillPatternSnapshotPath(workspace)); !os.IsNotExist(err) {
		t.Fatalf("snapshot unexpectedly exists: %v", err)
	}

	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := runSkillGraph(command, nil); err != nil {
		t.Fatalf("runSkillGraph() error = %v", err)
	}
	want := "No skill-pattern snapshot is available for this workspace.\n"
	if got := output.String(); got != want {
		t.Errorf("text output = %q, want %q", got, want)
	}

	skillGraphFormat = "json"
	output.Reset()
	if err := runSkillGraph(command, nil); err != nil {
		t.Fatalf("runSkillGraph(json) error = %v", err)
	}
	var graph skill.SkillPatternGraph
	if err := json.Unmarshal(output.Bytes(), &graph); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %s", err, output.String())
	}
	if graph.SnapshotAvailable {
		t.Fatal("snapshot_available = true, want false")
	}
	if graph.Patterns == nil || graph.Nodes == nil || graph.Edges == nil {
		t.Fatalf("missing snapshot arrays must be []: %s", output.String())
	}
}

func TestRunSkillGraphJSONAppliesFilters(t *testing.T) {
	workspace := configureSkillGraphTest(t, "json", "alice", 3)
	writeSkillGraphTestSnapshot(t, workspace)

	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := runSkillGraph(command, nil); err != nil {
		t.Fatalf("runSkillGraph() error = %v", err)
	}

	var graph skill.SkillPatternGraph
	if err := json.Unmarshal(output.Bytes(), &graph); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; output = %s", err, output.String())
	}
	if !graph.SnapshotAvailable {
		t.Fatal("snapshot_available = false, want true")
	}
	if got, want := graph.TeamName, "test-team"; got != want {
		t.Errorf("team_name = %q, want %q", got, want)
	}
	if got, want := len(graph.Patterns), 1; got != want {
		t.Fatalf("pattern count = %d, want %d", got, want)
	}
	if got, want := graph.Patterns[0].ID, "pat_b"; got != want {
		t.Errorf("pattern ID = %q, want %q", got, want)
	}
}

func TestRunSkillGraphTextUsesCommandWriter(t *testing.T) {
	workspace := configureSkillGraphTest(t, "text", "", 0)
	writeSkillGraphTestSnapshot(t, workspace)

	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := runSkillGraph(command, nil); err != nil {
		t.Fatalf("runSkillGraph() error = %v", err)
	}
	for _, expected := range []string{"Skill pattern graph", "Team: test-team", "bash -> write"} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("output does not contain %q:\n%s", expected, output.String())
		}
	}
}

func TestRunSkillGraphRejectsInvalidOptionsBeforeReadingSnapshot(t *testing.T) {
	configureSkillGraphTest(t, "dot", "", 0)
	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	if err := runSkillGraph(command, nil); err == nil || !strings.Contains(err.Error(), "allowed: text, json") {
		t.Fatalf("runSkillGraph() error = %v, want unsupported format", err)
	}

	skillGraphFormat = "text"
	skillGraphMinFrequency = -1
	if err := runSkillGraph(command, nil); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("runSkillGraph() error = %v, want negative frequency error", err)
	}
}

func TestRunSkillGraphRejectsInvalidSnapshot(t *testing.T) {
	workspace := configureSkillGraphTest(t, "json", "", 0)
	path := skill.SkillPatternSnapshotPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":99}`), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	command := &cobra.Command{}
	command.SetOut(&bytes.Buffer{})
	err := runSkillGraph(command, nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported skill pattern snapshot schema version") {
		t.Fatalf("runSkillGraph() error = %v, want schema error", err)
	}
}

func configureSkillGraphTest(t *testing.T, format, agent string, minFrequency int64) string {
	t.Helper()
	previousOptions := opts
	previousFormat := skillGraphFormat
	previousAgent := skillGraphAgent
	previousMinimum := skillGraphMinFrequency
	t.Cleanup(func() {
		opts = previousOptions
		skillGraphFormat = previousFormat
		skillGraphAgent = previousAgent
		skillGraphMinFrequency = previousMinimum
	})

	workspace := t.TempDir()
	opts.workspace = workspace
	skillGraphFormat = format
	skillGraphAgent = agent
	skillGraphMinFrequency = minFrequency
	return workspace
}

func writeSkillGraphTestSnapshot(t *testing.T, workspace string) {
	t.Helper()
	generatedAt := time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
	snapshot := skill.SkillPatternSnapshot{
		SchemaVersion: skill.SkillPatternSnapshotVersion,
		RunID:         "run-test",
		TeamName:      "test-team",
		GeneratedAt:   generatedAt,
		Patterns: []skill.SkillPatternSummary{
			{
				ID:               "pat_a",
				Tools:            []string{"view", "write"},
				ParameterClasses: [][]string{{"file"}, {"file"}},
				Count:            2,
				Agents:           []skill.SkillPatternAgent{{Name: "alice", Count: 2}},
				FirstSeen:        generatedAt.Add(-2 * time.Hour),
				LastSeen:         generatedAt.Add(-time.Hour),
			},
			{
				ID:               "pat_b",
				Tools:            []string{"bash", "write"},
				ParameterClasses: [][]string{{"string"}, {"file"}},
				Count:            4,
				Agents:           []skill.SkillPatternAgent{{Name: "alice", Count: 4}},
				FirstSeen:        generatedAt.Add(-2 * time.Hour),
				LastSeen:         generatedAt.Add(-time.Hour),
			},
			{
				ID:               "pat_c",
				Tools:            []string{"grep", "view"},
				ParameterClasses: [][]string{{"string"}, {"file"}},
				Count:            5,
				Agents:           []skill.SkillPatternAgent{{Name: "bob", Count: 5}},
				FirstSeen:        generatedAt.Add(-2 * time.Hour),
				LastSeen:         generatedAt.Add(-time.Hour),
			},
		},
	}
	data, err := skill.EncodeSkillPatternSnapshot(snapshot)
	if err != nil {
		t.Fatalf("skill.EncodeSkillPatternSnapshot() error = %v", err)
	}
	path := skill.SkillPatternSnapshotPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}
}
