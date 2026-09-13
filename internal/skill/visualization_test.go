package skill

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildSkillPatternGraphCountsOccurrencesAndTransitions(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"write", "write", "bash"}, 2, "alice"),
		testSkillPatternSummary("pat_b", []string{"bash", "write"}, 3, "bob"),
	})

	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}

	if got, want := graphNodeCount(graph, "bash"), int64(5); got != want {
		t.Errorf("bash node count = %d, want %d", got, want)
	}
	if got, want := graphNodeCount(graph, "write"), int64(7); got != want {
		t.Errorf("write node count = %d, want %d", got, want)
	}
	if got, want := graphEdgeCount(graph, "write", "write"), int64(2); got != want {
		t.Errorf("write self-edge count = %d, want %d", got, want)
	}
	if got, want := graphEdgeCount(graph, "write", "bash"), int64(2); got != want {
		t.Errorf("write -> bash count = %d, want %d", got, want)
	}
	if got, want := graphEdgeCount(graph, "bash", "write"), int64(3); got != want {
		t.Errorf("bash -> write count = %d, want %d", got, want)
	}

	writeNode := graph.Nodes[1]
	if got, want := writeNode.Agents, []string{"alice", "bob"}; !reflect.DeepEqual(got, want) {
		t.Errorf("write agents = %v, want %v", got, want)
	}
	if got, want := writeNode.PatternIDs, []string{"pat_a", "pat_b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("write pattern IDs = %v, want %v", got, want)
	}
}

func TestBuildSkillPatternGraphFiltersBeforeCounting(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash", "write"}, 2, "alice"),
		testSkillPatternSummary("pat_b", []string{"view", "write"}, 5, "bob"),
		testSkillPatternSummary("pat_c", []string{"grep", "write"}, 7, "alice"),
	})

	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{
		Agent:        "alice",
		MinFrequency: 3,
	})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	if got, want := len(graph.Patterns), 1; got != want {
		t.Fatalf("pattern count = %d, want %d", got, want)
	}
	if got, want := graph.Patterns[0].ID, "pat_c"; got != want {
		t.Errorf("pattern ID = %q, want %q", got, want)
	}
	if got, want := graphNodeCount(graph, "write"), int64(7); got != want {
		t.Errorf("write node count = %d, want %d", got, want)
	}
}

func TestBuildSkillPatternGraphUnknownAgentReturnsAvailableEmptyGraph(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash", "write"}, 2, "alice"),
	})

	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{Agent: "ALICE"})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	if !graph.SnapshotAvailable {
		t.Fatal("SnapshotAvailable = false, want true")
	}
	if graph.Patterns == nil || graph.Nodes == nil || graph.Edges == nil {
		t.Fatal("empty graph arrays must be non-nil")
	}
	if len(graph.Patterns) != 0 || len(graph.Nodes) != 0 || len(graph.Edges) != 0 {
		t.Fatalf("filtered graph is not empty: %#v", graph)
	}
}

func TestBuildSkillPatternGraphIsDeterministicAndDoesNotAliasInput(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"zeta", "alpha"}, 2, "z-agent"),
		testSkillPatternSummary("pat_b", []string{"alpha", "middle"}, 3, "a-agent"),
	})

	first, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("first BuildSkillPatternGraph() error = %v", err)
	}
	second, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("second BuildSkillPatternGraph() error = %v", err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("json.Marshal(first) error = %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("json.Marshal(second) error = %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("JSON differs:\n%s\n%s", firstJSON, secondJSON)
	}
	if got, want := []string{first.Nodes[0].Tool, first.Nodes[1].Tool, first.Nodes[2].Tool}, []string{"alpha", "middle", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("node order = %v, want %v", got, want)
	}

	first.Patterns[0].Tools[0] = "mutated"
	first.Patterns[0].ParameterClasses[0][0] = "other"
	first.Patterns[0].Agents[0].Name = "mutated"
	if snapshot.Patterns[0].Tools[0] == "mutated" || snapshot.Patterns[0].ParameterClasses[0][0] == "other" || snapshot.Patterns[0].Agents[0].Name == "mutated" {
		t.Fatal("graph patterns alias snapshot storage")
	}
}

func TestBuildSkillPatternGraphRejectsCountOverflow(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash"}, math.MaxInt64, "alice"),
		testSkillPatternSummary("pat_b", []string{"bash"}, 1, "bob"),
	})

	_, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("BuildSkillPatternGraph() error = %v, want overflow", err)
	}
}

func TestBuildSkillPatternGraphRejectsNegativeMinimumFrequency(t *testing.T) {
	snapshot := testSkillPatternSnapshot(nil)
	_, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{MinFrequency: -1})
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
}

func TestRenderSkillPatternGraphText(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash", "write"}, 2, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}

	output := RenderSkillPatternGraphText(graph)
	for _, expected := range []string{
		"Skill pattern graph\n",
		"Run: run-test\n",
		"Team: test-team\n",
		"Patterns: 1\n",
		"pat_a ×2 agents=alice draft=-",
		"bash -> write",
		"Edges\n  bash -> write ×2\n",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("output does not contain %q:\n%s", expected, output)
		}
	}
}

func TestRenderUnavailableSkillPatternGraphText(t *testing.T) {
	got := RenderSkillPatternGraphText(NewUnavailableSkillPatternGraph())
	want := "No skill-pattern snapshot is available for this workspace.\n"
	if got != want {
		t.Errorf("RenderSkillPatternGraphText() = %q, want %q", got, want)
	}
}

func testSkillPatternSnapshot(patterns []SkillPatternSummary) SkillPatternSnapshot {
	if patterns == nil {
		patterns = []SkillPatternSummary{}
	}
	return SkillPatternSnapshot{
		SchemaVersion: SkillPatternSnapshotVersion,
		RunID:         "run-test",
		TeamName:      "test-team",
		GeneratedAt:   time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC),
		Patterns:      patterns,
	}
}

func testSkillPatternSummary(id string, tools []string, count int64, agent string) SkillPatternSummary {
	classes := make([][]string, len(tools))
	for i := range classes {
		classes[i] = []string{"none"}
	}
	return SkillPatternSummary{
		ID:               id,
		Tools:            tools,
		ParameterClasses: classes,
		Count:            count,
		Agents:           []SkillPatternAgent{{Name: agent, Count: count}},
		FirstSeen:        time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC),
		LastSeen:         time.Date(2026, time.September, 13, 11, 0, 0, 0, time.UTC),
	}
}

func graphNodeCount(graph SkillPatternGraph, tool string) int64 {
	for _, node := range graph.Nodes {
		if node.Tool == tool {
			return node.Count
		}
	}
	return 0
}

func graphEdgeCount(graph SkillPatternGraph, from, to string) int64 {
	fromID := SkillPatternNodeID(from)
	toID := SkillPatternNodeID(to)
	for _, edge := range graph.Edges {
		if edge.From == fromID && edge.To == toID {
			return edge.Count
		}
	}
	return 0
}
