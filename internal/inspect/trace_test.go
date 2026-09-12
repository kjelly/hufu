package inspect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestInspectTraceOrdersAnchoredEntriesAndPlacesUnanchoredLast(t *testing.T) {
	fixture := buildRunFixture(t)
	index, err := team.OpenDecisionIndex(fixture.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Append(team.DecisionIndexEntry{
		DecisionID: "decision-unanchored", RunID: fixture.runID, TaskID: fixture.taskID,
		RecordRef: team.ArtifactRef{ID: "decision-record", SHA256: "decision-digest", Path: "/private/decision"},
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(fixture.workspace, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	terminalJSON, err := json.Marshal([]team.TerminalSession{{
		ID: "terminal-unanchored", RunID: fixture.runID, OwnerTaskID: fixture.taskID,
		Agent: "worker", Command: []string{"echo", "private-terminal-command"}, State: team.TerminalSessionExited,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.workspace, "logs", "terminal_sessions.json"), terminalJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	query := InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID}
	first, err := InspectTrace(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	second, err := InspectTrace(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, secondJSON) {
		t.Fatalf("trace output is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	data := first.Data.(TraceData)
	seenUnanchored := false
	var lastOrdinal int64
	sources := map[string]bool{}
	for _, entry := range data.Entries {
		sources[entry.Ref.Source] = true
		if entry.Ref.ParentEventID != "" {
			t.Fatalf("trace invented a parent event: %#v", entry)
		}
		if entry.AnchorEventOrdinal == 0 {
			seenUnanchored = true
			if entry.Ref.Source != "event_store" && entry.ReasonCode != ReasonMissingAnchor {
				t.Fatalf("unanchored entry lacks reason: %#v", entry)
			}
			continue
		}
		if seenUnanchored {
			t.Fatalf("anchored entry appeared after unanchored entries: %#v", entry)
		}
		if entry.AnchorEventOrdinal < lastOrdinal {
			t.Fatalf("anchor ordinals regressed: %d after %d", entry.AnchorEventOrdinal, lastOrdinal)
		}
		lastOrdinal = entry.AnchorEventOrdinal
	}
	for _, source := range []string{"event_store", "execution_receipt", "context_manifest", "memory_manifest", "evidence_manifest", "decision_index", "terminal_projection"} {
		if !sources[source] {
			t.Fatalf("trace missing source %q: %#v", source, data.Entries)
		}
	}
	for _, forbidden := range [][]byte{[]byte("secret task output"), []byte("true --with-secret"), []byte("private-terminal-command"), []byte("/private/")} {
		if bytes.Contains(firstJSON, forbidden) {
			t.Fatalf("trace exposed %q: %s", forbidden, firstJSON)
		}
	}
}

func TestTraceSupplementalEntriesKeepZeroEventOrdinal(t *testing.T) {
	fixture := buildRunFixture(t)
	envelope, err := InspectTrace(t.Context(), InspectQuery{Workspace: fixture.workspace, RunID: fixture.runID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range envelope.Data.(TraceData).Entries {
		if entry.Ref.Source != "event_store" && entry.Ref.EventOrdinal != 0 {
			t.Fatalf("supplemental entry claims durable ordinal: %#v", entry)
		}
	}
}
