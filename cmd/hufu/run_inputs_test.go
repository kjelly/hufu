package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestReadRunInputAssignmentsParsesQualifiedCLIAndFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inputs.yaml")
	if err := os.WriteFile(path, []byte("demo::enabled: true\nreview.count: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assignments, err := readRunInputAssignments([]string{`demo::review.scope={"kind":"last_n","count":10}`}, []string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 3 {
		t.Fatalf("assignments = %#v", assignments)
	}
	if assignments[0].teamName != "demo" || assignments[0].assignment.Name != "review.scope" || assignments[0].assignment.Source != team.RunInputSourceCLI {
		t.Fatalf("CLI assignment = %#v", assignments[0])
	}
	if assignments[1].teamName != "demo" || assignments[1].assignment.Name != "enabled" || assignments[1].assignment.Source != team.RunInputSourceFile {
		t.Fatalf("qualified file assignment = %#v", assignments[1])
	}
	if assignments[2].teamName != "" || assignments[2].assignment.Name != "review.count" {
		t.Fatalf("unqualified file assignment = %#v", assignments[2])
	}
}

func TestConfigureRunInputAssignmentsRequiresQualificationForMultipleTeams(t *testing.T) {
	saved := opts
	t.Cleanup(func() { opts = saved })
	opts.inputFlags = []string{"value=1"}
	err := configureRunInputAssignments(map[string]*teamContext{"one": {}, "two": {}})
	if err == nil || err.Error() != `input_team_ambiguous: unqualified input "value" does not uniquely match one selected team` {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigureRunInputAssignmentsInfersUniqueDeclaringTeam(t *testing.T) {
	saved := opts
	t.Cleanup(func() { opts = saved })
	opts.inputFlags = []string{"value=1"}
	first := &teamContext{session: &team.TeamSession{RunInputDefinitions: []team.RunInputDefinition{{Name: "value", Schema: team.RunInputSchema{Type: "integer"}}}}, coordinator: new(team.Coordinator)}
	second := &teamContext{session: &team.TeamSession{}, coordinator: new(team.Coordinator)}
	if err := configureRunInputAssignments(map[string]*teamContext{"one": first, "two": second}); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureRunInputAssignmentsRejectsUnknownQualifiedTeam(t *testing.T) {
	saved := opts
	t.Cleanup(func() { opts = saved })
	opts.inputFlags = []string{"other::value=1"}
	if err := configureRunInputAssignments(map[string]*teamContext{"demo": {}}); err == nil {
		t.Fatal("unknown qualified team was accepted")
	}
}

func TestRootCommandRegistersTypedInputFlagsAndClarifiesVar(t *testing.T) {
	command := newRootCommand()
	for _, name := range []string{"input", "input-file"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("root command omitted --%s", name)
		}
	}
	if usage := command.Flags().Lookup("var").Usage; usage == "" || !containsAll(usage, "template", "does not provide typed execution semantics") {
		t.Fatalf("--var usage = %q", usage)
	}
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
