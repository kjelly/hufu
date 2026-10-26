package inspect

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCandidateProjectionDoesNotExposeRejectedContent(t *testing.T) {
	item := &team.TodoItem{ID: "6", Status: team.TaskProtocolIncomplete, ResultCandidate: &team.ResultCandidate{Version: 2, Attempt: 1, SHA256: strings.Repeat("a", 64), ArtifactID: "sha256-" + strings.Repeat("a", 64), Issues: []team.ResultSubmissionIssue{{Code: "additional_property", Pointer: "/private-source-title"}}}}
	data := projectTask(item, InspectQuery{RunID: "run", TaskID: "6"})
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if data.ResultCandidate == nil || data.ResultCandidate.IssueCount != 1 || strings.Contains(string(raw), "private-source-title") {
		t.Fatalf("bad candidate projection: %s", raw)
	}
	if projectTask(item, InspectQuery{RunID: "run", TaskID: "6", Attempt: 2}).ResultCandidate != nil {
		t.Fatal("candidate leaked into another attempt")
	}
	changed := *item
	candidate := *item.ResultCandidate
	changed.ResultCandidate = &candidate
	candidate.SHA256 = strings.Repeat("b", 64)
	if len(compareTaskProjections([]*team.TodoItem{item}, []*team.TodoItem{&changed})) == 0 {
		t.Fatal("replay did not detect candidate drift")
	}
}
