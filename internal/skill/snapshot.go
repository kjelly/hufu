package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	SkillPatternSnapshotVersion  = 1
	skillPatternSnapshotFileName = "patterns.json"
)

var normalizedFileClassPattern = regexp.MustCompile(`\*\.[[:alnum:]]+`)

// SkillPatternSnapshot is the non-canonical, metadata-only projection consumed
// by the skill graph command. It is never read back into pattern detection.
type SkillPatternSnapshot struct {
	SchemaVersion int                   `json:"schema_version"`
	RunID         string                `json:"run_id"`
	TeamName      string                `json:"team_name"`
	GeneratedAt   time.Time             `json:"generated_at"`
	Patterns      []SkillPatternSummary `json:"patterns"`
}

// SkillPatternSummary contains only safe aggregate data about one evaluated
// candidate. Raw parameters, task descriptions, and model output are excluded.
type SkillPatternSummary struct {
	ID               string              `json:"id"`
	Tools            []string            `json:"tools"`
	ParameterClasses [][]string          `json:"parameter_classes"`
	Count            int64               `json:"count"`
	Agents           []SkillPatternAgent `json:"agents"`
	SemanticGroupID  string              `json:"semantic_group_id,omitempty"`
	DraftName        string              `json:"draft_name,omitempty"`
	FirstSeen        time.Time           `json:"first_seen"`
	LastSeen         time.Time           `json:"last_seen"`
}

// SkillPatternAgent records an agent's contribution to a candidate count.
type SkillPatternAgent struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// SavedPatternDraft associates a generated draft with its source candidate.
// Path is retained only for the live notification and is never persisted.
type SavedPatternDraft struct {
	PatternID string
	Name      string
	Path      string
}

// SkillPatternSnapshotPath returns the fixed workspace projection path.
func SkillPatternSnapshotPath(workspace string) string {
	return filepath.Join(workspace, "skills", skillPatternSnapshotFileName)
}

// PatternCandidateID derives a stable ID from the candidate's source sequence
// hashes. Runtime names, counts, agents, and timestamps do not affect it.
func PatternCandidateID(candidate PatternCandidate) (string, error) {
	ids, err := normalizedSourcePatternIDs(candidate.SourcePatternIDs)
	if err != nil {
		return "", err
	}
	return digestPatternIdentity("pat_", ids), nil
}

// NewSkillPatternSnapshot materializes evaluated candidates into the safe
// persisted schema. A previous projection is consulted only for draft-name
// associations that were not recreated in this evaluation.
func NewSkillPatternSnapshot(
	runID string,
	teamName string,
	generatedAt time.Time,
	candidates []PatternCandidate,
	savedDrafts []SavedPatternDraft,
	previous *SkillPatternSnapshot,
) (SkillPatternSnapshot, error) {
	if strings.TrimSpace(runID) == "" {
		return SkillPatternSnapshot{}, errors.New("skill pattern snapshot run ID is required")
	}
	if strings.TrimSpace(teamName) == "" {
		return SkillPatternSnapshot{}, errors.New("skill pattern snapshot team name is required")
	}
	if generatedAt.IsZero() {
		return SkillPatternSnapshot{}, errors.New("skill pattern snapshot generation time is required")
	}

	draftNames, err := savedDraftNames(savedDrafts)
	if err != nil {
		return SkillPatternSnapshot{}, err
	}
	if previous != nil {
		for _, pattern := range previous.Patterns {
			if pattern.DraftName != "" {
				if _, exists := draftNames[pattern.ID]; !exists {
					draftNames[pattern.ID] = pattern.DraftName
				}
			}
		}
	}

	patterns := make([]SkillPatternSummary, 0, len(candidates))
	for i := range candidates {
		summary, summaryErr := summarizePatternCandidate(candidates[i], draftNames)
		if summaryErr != nil {
			return SkillPatternSnapshot{}, fmt.Errorf("summarize skill pattern candidate %d: %w", i, summaryErr)
		}
		patterns = append(patterns, summary)
	}
	slices.SortFunc(patterns, func(a, b SkillPatternSummary) int {
		return strings.Compare(a.ID, b.ID)
	})

	snapshot := SkillPatternSnapshot{
		SchemaVersion: SkillPatternSnapshotVersion,
		RunID:         runID,
		TeamName:      teamName,
		GeneratedAt:   generatedAt.UTC(),
		Patterns:      patterns,
	}
	if err := ValidateSkillPatternSnapshot(snapshot); err != nil {
		return SkillPatternSnapshot{}, err
	}
	return snapshot, nil
}

// EncodeSkillPatternSnapshot validates and deterministically encodes a
// snapshot. The trailing newline makes atomic file replacement shell-friendly.
func EncodeSkillPatternSnapshot(snapshot SkillPatternSnapshot) ([]byte, error) {
	if err := ValidateSkillPatternSnapshot(snapshot); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode skill pattern snapshot: %w", err)
	}
	return append(data, '\n'), nil
}

// LoadSkillPatternSnapshot loads and validates a projection. A missing file is
// a successful unavailable result; malformed or unsupported files fail closed.
func LoadSkillPatternSnapshot(path string) (SkillPatternSnapshot, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return SkillPatternSnapshot{}, false, nil
	}
	if err != nil {
		return SkillPatternSnapshot{}, false, fmt.Errorf("open skill pattern snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()

	var snapshot SkillPatternSnapshot
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return SkillPatternSnapshot{}, false, fmt.Errorf("decode skill pattern snapshot: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return SkillPatternSnapshot{}, false, err
	}
	if err := ValidateSkillPatternSnapshot(snapshot); err != nil {
		return SkillPatternSnapshot{}, false, err
	}
	return snapshot, true, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode skill pattern snapshot: trailing JSON value")
		}
		return fmt.Errorf("decode skill pattern snapshot trailing data: %w", err)
	}
	return nil
}

// ValidateSkillPatternSnapshot enforces the persisted schema invariants.
func ValidateSkillPatternSnapshot(snapshot SkillPatternSnapshot) error {
	if snapshot.SchemaVersion != SkillPatternSnapshotVersion {
		return fmt.Errorf("unsupported skill pattern snapshot schema version %d", snapshot.SchemaVersion)
	}
	if strings.TrimSpace(snapshot.RunID) == "" {
		return errors.New("skill pattern snapshot run ID is required")
	}
	if strings.TrimSpace(snapshot.TeamName) == "" {
		return errors.New("skill pattern snapshot team name is required")
	}
	if snapshot.GeneratedAt.IsZero() {
		return errors.New("skill pattern snapshot generation time is required")
	}
	if snapshot.Patterns == nil {
		return errors.New("skill pattern snapshot patterns must be an array")
	}

	previousID := ""
	for i, pattern := range snapshot.Patterns {
		if err := validateSkillPatternSummary(pattern, snapshot.GeneratedAt); err != nil {
			return fmt.Errorf("invalid skill pattern %d: %w", i, err)
		}
		if i > 0 && strings.Compare(previousID, pattern.ID) >= 0 {
			return errors.New("skill pattern snapshot patterns must have unique sorted IDs")
		}
		previousID = pattern.ID
	}
	return nil
}

func validateSkillPatternSummary(pattern SkillPatternSummary, generatedAt time.Time) error {
	if strings.TrimSpace(pattern.ID) == "" {
		return errors.New("pattern ID is required")
	}
	if len(pattern.Tools) == 0 {
		return errors.New("pattern tools are required")
	}
	for _, tool := range pattern.Tools {
		if strings.TrimSpace(tool) == "" {
			return errors.New("pattern tool names must be non-empty")
		}
	}
	if len(pattern.ParameterClasses) != len(pattern.Tools) {
		return errors.New("parameter class count must match tool count")
	}
	for _, classes := range pattern.ParameterClasses {
		if err := validateParameterClasses(classes); err != nil {
			return err
		}
	}
	if pattern.Count <= 0 {
		return errors.New("pattern count must be positive")
	}
	if len(pattern.Agents) == 0 {
		return errors.New("pattern agents are required")
	}
	var agentTotal int64
	previousAgent := ""
	for i, agent := range pattern.Agents {
		if strings.TrimSpace(agent.Name) == "" || agent.Count <= 0 {
			return errors.New("pattern agents require a name and positive count")
		}
		if i > 0 && strings.Compare(previousAgent, agent.Name) >= 0 {
			return errors.New("pattern agents must have unique sorted names")
		}
		if agentTotal > pattern.Count-agent.Count {
			return errors.New("pattern agent count total overflows or exceeds pattern count")
		}
		agentTotal += agent.Count
		previousAgent = agent.Name
	}
	if agentTotal != pattern.Count {
		return errors.New("pattern agent counts must sum to pattern count")
	}
	if pattern.FirstSeen.IsZero() || pattern.LastSeen.IsZero() {
		return errors.New("pattern timestamps are required")
	}
	if pattern.FirstSeen.After(pattern.LastSeen) || pattern.LastSeen.After(generatedAt) {
		return errors.New("pattern timestamps must satisfy first_seen <= last_seen <= generated_at")
	}
	if pattern.DraftName != "" && !isValidSkillName(pattern.DraftName) {
		return errors.New("pattern draft name is invalid")
	}
	return nil
}

func validateParameterClasses(classes []string) error {
	if len(classes) == 0 {
		return errors.New("each tool requires at least one parameter class")
	}
	previous := -1
	for _, class := range classes {
		index := slices.Index(parameterClassOrder(), class)
		if index < 0 {
			return fmt.Errorf("unknown parameter class %q", class)
		}
		if index <= previous {
			return errors.New("parameter classes must be unique and in canonical order")
		}
		previous = index
	}
	if len(classes) > 1 && classes[0] == "none" {
		return errors.New("parameter class none cannot be combined with another class")
	}
	return nil
}

func summarizePatternCandidate(candidate PatternCandidate, draftNames map[string]string) (SkillPatternSummary, error) {
	if candidate.Sequence == nil {
		return SkillPatternSummary{}, errors.New("candidate sequence is required")
	}
	id, err := PatternCandidateID(candidate)
	if err != nil {
		return SkillPatternSummary{}, err
	}
	if candidate.Sequence.Count <= 0 {
		return SkillPatternSummary{}, errors.New("candidate count must be positive")
	}

	agentCounts := maps.Clone(candidate.Sequence.AgentCounts)
	if len(agentCounts) == 0 && candidate.Sequence.Agent != "" {
		agentCounts = map[string]int{candidate.Sequence.Agent: candidate.Sequence.Count}
	}
	agentNames := slices.Sorted(maps.Keys(agentCounts))
	agents := make([]SkillPatternAgent, 0, len(agentNames))
	for _, name := range agentNames {
		agents = append(agents, SkillPatternAgent{Name: name, Count: int64(agentCounts[name])})
	}

	parameterClasses := make([][]string, len(candidate.Sequence.Tools))
	for i := range candidate.Sequence.Tools {
		param := ""
		if i < len(candidate.Sequence.Params) {
			param = candidate.Sequence.Params[i]
		}
		parameterClasses[i] = classifyNormalizedParameter(param)
	}

	sourceIDs, err := normalizedSourcePatternIDs(candidate.SourcePatternIDs)
	if err != nil {
		return SkillPatternSummary{}, err
	}
	semanticGroupID := ""
	if len(sourceIDs) > 1 {
		semanticGroupID = digestPatternIdentity("sg_", sourceIDs)
	}

	return SkillPatternSummary{
		ID:               id,
		Tools:            slices.Clone(candidate.Sequence.Tools),
		ParameterClasses: parameterClasses,
		Count:            int64(candidate.Sequence.Count),
		Agents:           agents,
		SemanticGroupID:  semanticGroupID,
		DraftName:        draftNames[id],
		FirstSeen:        candidate.Sequence.FirstSeen.UTC(),
		LastSeen:         candidate.Sequence.LastSeen.UTC(),
	}, nil
}

func normalizedSourcePatternIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errors.New("candidate source pattern IDs are required")
	}
	normalized := slices.Clone(ids)
	for i, id := range normalized {
		id = strings.ToLower(id)
		normalized[i] = id
		if len(id) != sha256.Size*2 {
			return nil, errors.New("candidate source pattern ID must be a full SHA-256 hex digest")
		}
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("candidate source pattern ID must be a full SHA-256 hex digest")
		}
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	return normalized, nil
}

func digestPatternIdentity(prefix string, ids []string) string {
	digest := sha256.Sum256([]byte(strings.Join(ids, "\x00")))
	return prefix + hex.EncodeToString(digest[:])
}

func savedDraftNames(saved []SavedPatternDraft) (map[string]string, error) {
	names := make(map[string]string, len(saved))
	for _, draft := range saved {
		if strings.TrimSpace(draft.PatternID) == "" {
			return nil, errors.New("saved pattern draft ID is required")
		}
		if !isValidSkillName(draft.Name) {
			return nil, fmt.Errorf("saved pattern draft name %q is invalid", draft.Name)
		}
		if existing, ok := names[draft.PatternID]; ok && existing != draft.Name {
			return nil, fmt.Errorf("saved pattern draft %s has conflicting names", draft.PatternID)
		}
		names[draft.PatternID] = draft.Name
	}
	return names, nil
}

func classifyNormalizedParameter(param string) []string {
	if strings.TrimSpace(param) == "" {
		return []string{"none"}
	}

	remaining := param
	classes := make([]string, 0, 6)
	if normalizedFileClassPattern.MatchString(remaining) {
		classes = append(classes, "file")
		remaining = normalizedFileClassPattern.ReplaceAllString(remaining, "")
	}
	for _, placeholder := range []struct {
		value string
		class string
	}{
		{"<url>", "url"},
		{"<num>", "number"},
		{"<hash>", "hash"},
		{"<str>", "string"},
	} {
		if strings.Contains(remaining, placeholder.value) {
			classes = append(classes, placeholder.class)
			remaining = strings.ReplaceAll(remaining, placeholder.value, "")
		}
	}
	if strings.Trim(remaining, " \t\r\n{}[](),:=\"") != "" {
		classes = append(classes, "other")
	}
	if len(classes) == 0 {
		return []string{"other"}
	}
	return classes
}

func parameterClassOrder() []string {
	return []string{"none", "file", "url", "number", "hash", "string", "other"}
}
