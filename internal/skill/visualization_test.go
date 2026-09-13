package skill

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSkillGraphAggregatesNodeCountsPerOccurrence(t *testing.T) {
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

func TestSkillGraphAggregatesRepeatedEdgeCounts(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash", "write", "bash", "write"}, 2, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	if got, want := graphEdgeCount(graph, "bash", "write"), int64(4); got != want {
		t.Errorf("repeated edge count = %d, want %d", got, want)
	}
}

func TestSkillGraphPreservesSelfEdges(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"write", "write"}, 3, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	if got, want := graphEdgeCount(graph, "write", "write"), int64(3); got != want {
		t.Errorf("self-edge count = %d, want %d", got, want)
	}
}

func TestSkillGraphFiltersByAgentBeforeAggregation(t *testing.T) {
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

func TestSkillGraphFiltersByMinimumFrequency(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"view"}, 2, "alice"),
		testSkillPatternSummary("pat_b", []string{"write"}, 3, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{MinFrequency: 3})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	if got, want := len(graph.Patterns), 1; got != want || graph.Patterns[0].ID != "pat_b" {
		t.Errorf("filtered patterns = %#v, want only pat_b", graph.Patterns)
	}
}

func TestSkillGraphMissingAgentReturnsEmptySuccess(t *testing.T) {
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

func TestSkillGraphDeterministicOrdering(t *testing.T) {
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

func TestSkillGraphJSONStable(t *testing.T) {
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{"bash", "write"}, 2, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	var first bytes.Buffer
	if err := json.NewEncoder(&first).Encode(graph); err != nil {
		t.Fatalf("first Encode() error = %v", err)
	}
	var second bytes.Buffer
	if err := json.NewEncoder(&second).Encode(graph); err != nil {
		t.Fatalf("second Encode() error = %v", err)
	}
	if first.String() != second.String() || !strings.HasSuffix(first.String(), "\n") {
		t.Errorf("JSON output is not stable and newline-terminated:\nfirst:  %s\nsecond: %s", first.String(), second.String())
	}
}

func TestSkillGraphRejectsCountOverflow(t *testing.T) {
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

func TestSkillGraphMermaidEscapesNames(t *testing.T) {
	toolName := "read & \"<tag>\r\n\tnext"
	snapshot := testSkillPatternSnapshot([]SkillPatternSummary{
		testSkillPatternSummary("pat_a", []string{toolName, "write"}, 2, "alice"),
	})
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}

	output := RenderSkillPatternGraphMermaid(graph)
	expectedNode := SkillPatternNodeID(toolName) + `["read &amp; &quot;&lt;tag&gt;   next"]`
	if !strings.Contains(output, expectedNode) {
		t.Errorf("Mermaid output does not contain escaped node %q:\n%s", expectedNode, output)
	}
	expectedEdge := SkillPatternNodeID(toolName) + " -->|×2| " + SkillPatternNodeID("write")
	if !strings.Contains(output, expectedEdge) {
		t.Errorf("Mermaid output does not contain edge %q:\n%s", expectedEdge, output)
	}
	if strings.Contains(output, toolName) {
		t.Errorf("Mermaid output contains unescaped tool name:\n%s", output)
	}
}

func TestSkillGraphMermaidKeepsMetadataOnCommentLines(t *testing.T) {
	graph := SkillPatternGraph{
		SnapshotAvailable: true,
		RunID:             "run-safe\ngraph TD\r\u2028",
		TeamName:          "team-safe\tA --> B\u2029",
		Patterns:          []SkillPatternSummary{},
		Nodes:             []SkillPatternNode{},
		Edges:             []SkillPatternEdge{},
	}

	output := RenderSkillPatternGraphMermaid(graph)
	for _, expected := range []string{
		"  %% Run: run-safe graph TD  \n",
		"  %% Team: team-safe A --> B \n",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("Mermaid output does not contain sanitized metadata %q:\n%s", expected, output)
		}
	}
	if strings.Contains(output, "\ngraph TD\n") || strings.Contains(output, "\nA --> B\n") {
		t.Errorf("Mermaid metadata introduced a statement line:\n%s", output)
	}
}

func TestRenderSkillPatternGraphMermaidEmptyStates(t *testing.T) {
	missingWant := "graph LR\n  %% No skill-pattern snapshot is available for this workspace.\n"
	if got := RenderSkillPatternGraphMermaid(NewUnavailableSkillPatternGraph()); got != missingWant {
		t.Errorf("missing Mermaid output = %q, want %q", got, missingWant)
	}

	snapshot := testSkillPatternSnapshot(nil)
	graph, err := BuildSkillPatternGraph(snapshot, SkillPatternGraphFilter{})
	if err != nil {
		t.Fatalf("BuildSkillPatternGraph() error = %v", err)
	}
	existingOutput := RenderSkillPatternGraphMermaid(graph)
	for _, expected := range []string{
		"graph LR\n",
		"  %% Run: run-test\n",
		"  %% Team: test-team\n",
		"  %% Generated: 2026-09-13T12:00:00Z\n",
		"  %% No patterns matched.\n",
	} {
		if !strings.Contains(existingOutput, expected) {
			t.Errorf("existing-empty Mermaid output does not contain %q:\n%s", expected, existingOutput)
		}
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
