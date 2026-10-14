package inspect

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func TestDecisionAndTerminalAdaptersExposeOnlySafeAddressingFacts(t *testing.T) {
	workspace := t.TempDir()
	index, err := team.OpenDecisionIndex(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Append(team.DecisionIndexEntry{
		DecisionID: "decision-1", RunID: "run-1", TaskID: "task-1", Question: "secret question",
		RecordRef: team.ArtifactRef{ID: "record-1", SHA256: "digest-1", Path: "/secret/record"},
		IndexedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	addresses, err := LoadDecisionAddresses(InspectQuery{Workspace: workspace, RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(addresses) != 1 || addresses[0].RecordRef.ID != "record-1" {
		t.Fatalf("decision addresses = %#v", addresses)
	}

	sessions := []team.TerminalSession{{
		ID: "terminal-1", RunID: "run-1", OwnerTaskID: "task-1", Agent: "worker",
		Command: []string{"sh", "-c", "echo secret-command"}, WorkingDir: "/secret/workdir",
		State: team.TerminalSessionExited, OutputRefs: []team.ArtifactRef{{ID: "output-1", SHA256: "digest-output", Path: "/secret/output"}},
	}}
	encoded, err := json.Marshal(sessions)
	if err != nil {
		t.Fatal(err)
	}
	writeTerminalSessions(t, workspace, encoded)
	facts, err := LoadTerminalFacts(InspectQuery{Workspace: workspace, RunID: "run-1", TaskID: "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	safe, err := json.Marshal(struct {
		Addresses []DecisionAddress `json:"addresses"`
		Facts     []TerminalFact    `json:"facts"`
	}{addresses, facts})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret question", "secret-command", "/secret/"} {
		if strings.Contains(string(safe), forbidden) {
			t.Fatalf("adapter output exposed %q: %s", forbidden, safe)
		}
	}
}
