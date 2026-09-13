package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSkillPatternSnapshotRoundTrip(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	candidate := snapshotTestCandidate("source-a", now)
	patternID, err := PatternCandidateID(candidate)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSkillPatternSnapshot("run-1", "team-1", now, []PatternCandidate{candidate}, []SavedPatternDraft{{
		PatternID: patternID,
		Name:      "draft-edit-test",
		Path:      "/not/persisted/draft-edit-test/SKILL.md",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeSkillPatternSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "patterns.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, available, err := LoadSkillPatternSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !available {
		t.Fatal("snapshot is unavailable")
	}
	loadedData, err := EncodeSkillPatternSnapshot(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, loadedData) {
		t.Fatalf("round trip changed snapshot:\n%s\n%s", data, loadedData)
	}
	if bytes.Contains(data, []byte("/not/persisted")) {
		t.Fatal("runtime draft path leaked into snapshot")
	}
}

func TestSkillPatternSnapshotDeterministicOrdering(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	first := snapshotTestCandidate("z-source", now)
	second := snapshotTestCandidate("a-source", now)
	second.Sequence.AgentCounts = map[string]int{"writer": 2, "coder": 2}
	second.Sequence.Count = 4

	one, err := NewSkillPatternSnapshot("run-1", "team-1", now, []PatternCandidate{first, second}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewSkillPatternSnapshot("run-1", "team-1", now, []PatternCandidate{second, first}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oneData, err := EncodeSkillPatternSnapshot(one)
	if err != nil {
		t.Fatal(err)
	}
	twoData, err := EncodeSkillPatternSnapshot(two)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oneData, twoData) {
		t.Fatalf("candidate order changed snapshot encoding:\n%s\n%s", oneData, twoData)
	}
	if got := two.Patterns[0].Agents; len(got) > 1 && got[0].Name != "coder" {
		t.Fatalf("agents are not sorted: %#v", got)
	}
}

func TestSkillPatternSnapshotRejectsUnknownVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "patterns.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"run_id":"run","team_name":"team","generated_at":"2026-09-13T10:00:00Z","patterns":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSkillPatternSnapshot(path); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("LoadSkillPatternSnapshot() error = %v", err)
	}
}

func TestSkillPatternSnapshotRejectsInvalidInvariant(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	valid := SkillPatternSnapshot{
		SchemaVersion: SkillPatternSnapshotVersion,
		RunID:         "run",
		TeamName:      "team",
		GeneratedAt:   now,
		Patterns: []SkillPatternSummary{{
			ID:               "pattern",
			Tools:            []string{"edit"},
			ParameterClasses: [][]string{{"file"}},
			Count:            2,
			Agents:           []SkillPatternAgent{{Name: "coder", Count: 2}},
			FirstSeen:        now.Add(-time.Minute),
			LastSeen:         now,
		}},
	}
	tests := map[string]func(*SkillPatternSnapshot){
		"nil patterns":     func(s *SkillPatternSnapshot) { s.Patterns = nil },
		"agent count":      func(s *SkillPatternSnapshot) { s.Patterns[0].Agents[0].Count = 1 },
		"parameter length": func(s *SkillPatternSnapshot) { s.Patterns[0].ParameterClasses = nil },
		"future timestamp": func(s *SkillPatternSnapshot) { s.Patterns[0].LastSeen = now.Add(time.Second) },
		"invalid draft":    func(s *SkillPatternSnapshot) { s.Patterns[0].DraftName = "../draft" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Patterns = []SkillPatternSummary{valid.Patterns[0]}
			candidate.Patterns[0].Tools = slices.Clone(valid.Patterns[0].Tools)
			candidate.Patterns[0].ParameterClasses = cloneStringMatrix(valid.Patterns[0].ParameterClasses)
			candidate.Patterns[0].Agents = slices.Clone(valid.Patterns[0].Agents)
			mutate(&candidate)
			if err := ValidateSkillPatternSnapshot(candidate); err == nil {
				t.Fatal("ValidateSkillPatternSnapshot() error = nil")
			}
		})
	}
}

func TestSkillPatternSnapshotClassifiesParamsWithoutContent(t *testing.T) {
	got := classifyNormalizedParameter(`*.go <url> <num> <hash> <str> literal`)
	want := []string{"file", "url", "number", "hash", "string", "other"}
	if !slices.Equal(got, want) {
		t.Fatalf("classifyNormalizedParameter() = %v, want %v", got, want)
	}
	if got := classifyNormalizedParameter(""); !slices.Equal(got, []string{"none"}) {
		t.Fatalf("empty parameter classes = %v", got)
	}
}

func TestSkillPatternSnapshotContainsNoRawArguments(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	secrets := []string{
		"TOKEN=super-secret-value",
		"https://example.com/run?token=hidden-query",
		"internal-hostname.local",
		"/private/absolute/path.go",
		`"quoted-secret"`,
		"first-line\nsecond-line",
	}
	candidate := snapshotTestCandidate("safe-source", now)
	candidate.Sequence.Tools = make([]string, len(secrets))
	candidate.Sequence.Params = slices.Clone(secrets)
	for i := range candidate.Sequence.Tools {
		candidate.Sequence.Tools[i] = "tool"
	}

	snapshot, err := NewSkillPatternSnapshot("run", "team", now, []PatternCandidate{candidate}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeSkillPatternSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("snapshot leaked raw parameter %q: %s", secret, data)
		}
	}
}

func TestSkillPatternSnapshotStoresDraftNameWithoutPath(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	candidate := snapshotTestCandidate("draft-source", now)
	id, err := PatternCandidateID(candidate)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSkillPatternSnapshot("run", "team", now, []PatternCandidate{candidate}, []SavedPatternDraft{{
		PatternID: id,
		Name:      "draft-safe-name",
		Path:      "/sensitive/team/path/draft-safe-name/SKILL.md",
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"draft_name":"draft-safe-name"`)) {
		t.Fatalf("snapshot lacks draft name: %s", data)
	}
	if bytes.Contains(data, []byte("sensitive/team/path")) {
		t.Fatalf("snapshot contains draft path: %s", data)
	}
}

func TestSkillPatternSnapshotReusesDraftNameByPatternID(t *testing.T) {
	now := time.Date(2026, time.September, 13, 10, 0, 0, 0, time.UTC)
	candidate := snapshotTestCandidate("reuse-source", now)
	id, err := PatternCandidateID(candidate)
	if err != nil {
		t.Fatal(err)
	}
	previous := &SkillPatternSnapshot{Patterns: []SkillPatternSummary{{ID: id, DraftName: "draft-reused"}}}
	snapshot, err := NewSkillPatternSnapshot("run-2", "team", now, []PatternCandidate{candidate}, nil, previous)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Patterns[0].DraftName; got != "draft-reused" {
		t.Fatalf("draft name = %q", got)
	}
}

func TestLoadSkillPatternSnapshotMissing(t *testing.T) {
	_, available, err := LoadSkillPatternSnapshot(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if available {
		t.Fatal("missing snapshot reported available")
	}
}

func snapshotTestCandidate(seed string, generatedAt time.Time) PatternCandidate {
	sourceID := sha256Hex(seed)
	return PatternCandidate{
		Sequence: &ToolSequence{
			Tools:       []string{"view", "edit", "bash"},
			Params:      []string{"*.go", "*.go", "<str>"},
			Hash:        sourceID,
			Count:       3,
			FirstSeen:   generatedAt.Add(-2 * time.Minute),
			LastSeen:    generatedAt.Add(-time.Minute),
			Agent:       "coder",
			AgentCounts: map[string]int{"coder": 3},
		},
		SourcePatternIDs: []string{sourceID},
	}
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func cloneStringMatrix(values [][]string) [][]string {
	cloned := make([][]string, len(values))
	for i := range values {
		cloned[i] = slices.Clone(values[i])
	}
	return cloned
}
