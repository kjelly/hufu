package skill

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
)

// SkillPatternGraph is a deterministic projection of one persisted pattern
// snapshot. It is presentation-only and is never consumed by the detector.
type SkillPatternGraph struct {
	SchemaVersion     int                   `json:"schema_version"`
	SnapshotAvailable bool                  `json:"snapshot_available"`
	RunID             string                `json:"run_id,omitempty"`
	TeamName          string                `json:"team_name,omitempty"`
	GeneratedAt       *time.Time            `json:"generated_at,omitempty"`
	Patterns          []SkillPatternSummary `json:"patterns"`
	Nodes             []SkillPatternNode    `json:"nodes"`
	Edges             []SkillPatternEdge    `json:"edges"`
}

// SkillPatternNode aggregates occurrences of one tool across included
// patterns. Repeated occurrences within one pattern are counted separately.
type SkillPatternNode struct {
	ID         string   `json:"id"`
	Tool       string   `json:"tool"`
	Count      int64    `json:"count"`
	Agents     []string `json:"agents"`
	PatternIDs []string `json:"pattern_ids"`
}

// SkillPatternEdge aggregates adjacent tool transitions. Self-edges are
// retained because they describe repeated use of the same tool.
type SkillPatternEdge struct {
	From       string   `json:"from"`
	To         string   `json:"to"`
	Count      int64    `json:"count"`
	Agents     []string `json:"agents"`
	PatternIDs []string `json:"pattern_ids"`
}

// SkillPatternGraphFilter selects patterns before any graph counts are
// computed. Agent matching is exact and case-sensitive.
type SkillPatternGraphFilter struct {
	Agent        string
	MinFrequency int64
}

type patternNodeAggregate struct {
	node       SkillPatternNode
	agents     map[string]struct{}
	patternIDs map[string]struct{}
}

type patternEdgeKey struct {
	from string
	to   string
}

type patternEdgeAggregate struct {
	edge       SkillPatternEdge
	agents     map[string]struct{}
	patternIDs map[string]struct{}
}

// NewUnavailableSkillPatternGraph returns the stable empty projection used
// when the workspace has no snapshot yet.
func NewUnavailableSkillPatternGraph() SkillPatternGraph {
	return SkillPatternGraph{
		SchemaVersion: SkillPatternSnapshotVersion,
		Patterns:      []SkillPatternSummary{},
		Nodes:         []SkillPatternNode{},
		Edges:         []SkillPatternEdge{},
	}
}

// BuildSkillPatternGraph filters a validated snapshot and derives its node and
// edge aggregates without mutating or aliasing the input.
func BuildSkillPatternGraph(
	snapshot SkillPatternSnapshot,
	filter SkillPatternGraphFilter,
) (SkillPatternGraph, error) {
	if filter.MinFrequency < 0 {
		return SkillPatternGraph{}, errors.New("minimum frequency must not be negative")
	}
	if err := ValidateSkillPatternSnapshot(snapshot); err != nil {
		return SkillPatternGraph{}, err
	}

	generatedAt := snapshot.GeneratedAt
	graph := SkillPatternGraph{
		SchemaVersion:     snapshot.SchemaVersion,
		SnapshotAvailable: true,
		RunID:             snapshot.RunID,
		TeamName:          snapshot.TeamName,
		GeneratedAt:       &generatedAt,
		Patterns:          make([]SkillPatternSummary, 0, len(snapshot.Patterns)),
		Nodes:             []SkillPatternNode{},
		Edges:             []SkillPatternEdge{},
	}
	nodes := make(map[string]*patternNodeAggregate)
	edges := make(map[patternEdgeKey]*patternEdgeAggregate)

	for _, pattern := range snapshot.Patterns {
		if pattern.Count < filter.MinFrequency || !patternIncludesAgent(pattern, filter.Agent) {
			continue
		}
		graph.Patterns = append(graph.Patterns, cloneSkillPatternSummary(pattern))

		for _, tool := range pattern.Tools {
			id := SkillPatternNodeID(tool)
			aggregate := nodes[id]
			if aggregate == nil {
				aggregate = &patternNodeAggregate{
					node:       SkillPatternNode{ID: id, Tool: tool},
					agents:     make(map[string]struct{}),
					patternIDs: make(map[string]struct{}),
				}
				nodes[id] = aggregate
			}
			if err := addPatternCount(&aggregate.node.Count, pattern.Count); err != nil {
				return SkillPatternGraph{}, fmt.Errorf("count tool %q: %w", tool, err)
			}
			addPatternMetadata(aggregate.agents, aggregate.patternIDs, pattern)
		}

		for i := 0; i+1 < len(pattern.Tools); i++ {
			key := patternEdgeKey{
				from: SkillPatternNodeID(pattern.Tools[i]),
				to:   SkillPatternNodeID(pattern.Tools[i+1]),
			}
			aggregate := edges[key]
			if aggregate == nil {
				aggregate = &patternEdgeAggregate{
					edge:       SkillPatternEdge{From: key.from, To: key.to},
					agents:     make(map[string]struct{}),
					patternIDs: make(map[string]struct{}),
				}
				edges[key] = aggregate
			}
			if err := addPatternCount(&aggregate.edge.Count, pattern.Count); err != nil {
				return SkillPatternGraph{}, fmt.Errorf("count edge %q to %q: %w", pattern.Tools[i], pattern.Tools[i+1], err)
			}
			addPatternMetadata(aggregate.agents, aggregate.patternIDs, pattern)
		}
	}

	for _, aggregate := range nodes {
		aggregate.node.Agents = sortedSet(aggregate.agents)
		aggregate.node.PatternIDs = sortedSet(aggregate.patternIDs)
		graph.Nodes = append(graph.Nodes, aggregate.node)
	}
	slices.SortFunc(graph.Nodes, func(a, b SkillPatternNode) int {
		return cmp.Or(strings.Compare(a.Tool, b.Tool), strings.Compare(a.ID, b.ID))
	})

	for _, aggregate := range edges {
		aggregate.edge.Agents = sortedSet(aggregate.agents)
		aggregate.edge.PatternIDs = sortedSet(aggregate.patternIDs)
		graph.Edges = append(graph.Edges, aggregate.edge)
	}
	slices.SortFunc(graph.Edges, func(a, b SkillPatternEdge) int {
		return cmp.Or(strings.Compare(a.From, b.From), strings.Compare(a.To, b.To))
	})

	return graph, nil
}

// SkillPatternNodeID returns a collision-resistant, renderer-safe identity for
// a tool name. The full digest is retained instead of truncating it.
func SkillPatternNodeID(tool string) string {
	digest := sha256.Sum256([]byte(tool))
	return "tool_" + hex.EncodeToString(digest[:])
}

// RenderSkillPatternGraphText returns a deterministic human-readable view.
func RenderSkillPatternGraphText(graph SkillPatternGraph) string {
	if !graph.SnapshotAvailable {
		return "No skill-pattern snapshot is available for this workspace.\n"
	}

	var output strings.Builder
	fmt.Fprintln(&output, "Skill pattern graph")
	fmt.Fprintf(&output, "Run: %s\n", graph.RunID)
	fmt.Fprintf(&output, "Team: %s\n", graph.TeamName)
	if graph.GeneratedAt != nil {
		fmt.Fprintf(&output, "Generated: %s\n", graph.GeneratedAt.UTC().Format(time.RFC3339Nano))
	}
	fmt.Fprintf(&output, "Patterns: %d\n\n", len(graph.Patterns))
	fmt.Fprintln(&output, "Patterns")
	if len(graph.Patterns) == 0 {
		fmt.Fprintln(&output, "  (none)")
	}
	for _, pattern := range graph.Patterns {
		agentNames := make([]string, len(pattern.Agents))
		for i, agent := range pattern.Agents {
			agentNames[i] = agent.Name
		}
		draftName := pattern.DraftName
		if draftName == "" {
			draftName = "-"
		}
		semanticGroup := pattern.SemanticGroupID
		if semanticGroup == "" {
			semanticGroup = "-"
		}
		fmt.Fprintf(&output, "  %s ×%d agents=%s draft=%s\n", pattern.ID, pattern.Count, strings.Join(agentNames, ","), draftName)
		fmt.Fprintf(&output, "    %s\n", strings.Join(pattern.Tools, " -> "))
		fmt.Fprintf(&output, "    parameter-classes: %s\n", formatParameterClasses(pattern.ParameterClasses))
		fmt.Fprintf(&output, "    semantic-group: %s\n", semanticGroup)
		fmt.Fprintf(&output, "    first-seen: %s\n", pattern.FirstSeen.UTC().Format(time.RFC3339Nano))
		fmt.Fprintf(&output, "    last-seen: %s\n", pattern.LastSeen.UTC().Format(time.RFC3339Nano))
	}

	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "Edges")
	if len(graph.Edges) == 0 {
		fmt.Fprintln(&output, "  (none)")
		return output.String()
	}
	toolsByID := make(map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		toolsByID[node.ID] = node.Tool
	}
	for _, edge := range graph.Edges {
		fmt.Fprintf(&output, "  %s -> %s ×%d\n", toolsByID[edge.From], toolsByID[edge.To], edge.Count)
	}
	return output.String()
}

// RenderSkillPatternGraphMermaid returns raw Mermaid source with stable node
// IDs and labels escaped for Mermaid's quoted-node syntax.
func RenderSkillPatternGraphMermaid(graph SkillPatternGraph) string {
	var output strings.Builder
	fmt.Fprintln(&output, "graph LR")
	if !graph.SnapshotAvailable {
		fmt.Fprintln(&output, "  %% No skill-pattern snapshot is available for this workspace.")
		return output.String()
	}

	fmt.Fprintf(&output, "  %%%% Run: %s\n", sanitizeMermaidComment(graph.RunID))
	fmt.Fprintf(&output, "  %%%% Team: %s\n", sanitizeMermaidComment(graph.TeamName))
	if graph.GeneratedAt != nil {
		fmt.Fprintf(&output, "  %%%% Generated: %s\n", graph.GeneratedAt.UTC().Format(time.RFC3339Nano))
	}
	if len(graph.Patterns) == 0 {
		fmt.Fprintln(&output, "  %% No patterns matched.")
		return output.String()
	}

	for _, node := range graph.Nodes {
		fmt.Fprintf(&output, "  %s[\"%s\"]\n", node.ID, escapeMermaidLabel(node.Tool))
	}
	for _, edge := range graph.Edges {
		fmt.Fprintf(&output, "  %s -->|×%d| %s\n", edge.From, edge.Count, edge.To)
	}
	return output.String()
}

// sanitizeMermaidComment keeps snapshot metadata on one Mermaid comment line.
// Snapshot files are local input and team names are user-authored, so control
// characters must not be allowed to introduce additional Mermaid statements.
func sanitizeMermaidComment(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, value)
}

func patternIncludesAgent(pattern SkillPatternSummary, agentName string) bool {
	if agentName == "" {
		return true
	}
	return slices.ContainsFunc(pattern.Agents, func(agent SkillPatternAgent) bool {
		return agent.Name == agentName
	})
}

func addPatternCount(total *int64, count int64) error {
	if count <= 0 {
		return errors.New("pattern count must be positive")
	}
	if *total > math.MaxInt64-count {
		return errors.New("aggregate count overflows int64")
	}
	*total += count
	return nil
}

func addPatternMetadata(agentSet, patternSet map[string]struct{}, pattern SkillPatternSummary) {
	for _, agent := range pattern.Agents {
		agentSet[agent.Name] = struct{}{}
	}
	patternSet[pattern.ID] = struct{}{}
}

func sortedSet(set map[string]struct{}) []string {
	values := make([]string, 0, len(set))
	for value := range set {
		values = append(values, value)
	}
	slices.Sort(values)
	return values
}

func cloneSkillPatternSummary(pattern SkillPatternSummary) SkillPatternSummary {
	clone := pattern
	clone.Tools = slices.Clone(pattern.Tools)
	clone.ParameterClasses = make([][]string, len(pattern.ParameterClasses))
	for i := range pattern.ParameterClasses {
		clone.ParameterClasses[i] = slices.Clone(pattern.ParameterClasses[i])
	}
	clone.Agents = slices.Clone(pattern.Agents)
	return clone
}

func formatParameterClasses(classes [][]string) string {
	formatted := make([]string, len(classes))
	for i := range classes {
		formatted[i] = "[" + strings.Join(classes[i], ",") + "]"
	}
	return strings.Join(formatted, " ")
}

func escapeMermaidLabel(label string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"\"", "&quot;",
		"<", "&lt;",
		">", "&gt;",
		"\r", " ",
		"\n", " ",
		"\t", " ",
	).Replace(label)
}
