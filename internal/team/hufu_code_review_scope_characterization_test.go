package team

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"
)

func hufuCodeReviewTeamDir(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(sourceFile), "..", "..", ".agent-teams", "hufu-code-review")
}

func loadHufuCodeReviewTeam(t *testing.T) *TeamSession {
	t.Helper()
	session, err := LoadTeam(hufuCodeReviewTeamDir(t), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-code-review): %v", err)
	}
	return session
}

func TestHufuCodeReviewCharacterizesSingleCommitStaticScope(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	var producer *TaskDef
	for i := range session.ContractTasks {
		if session.ContractTasks[i].ID == "produce-workset" {
			producer = &session.ContractTasks[i]
			break
		}
	}
	if producer == nil || producer.Action == nil {
		t.Fatal("hufu-code-review produce-workset static action is missing")
	}
	var payload struct {
		MaxCommits int `json:"max_commits"`
	}
	if err := json.Unmarshal([]byte(producer.Action.Payload), &payload); err != nil {
		t.Fatalf("decode produce-workset payload: %v", err)
	}
	if payload.MaxCommits != 1 {
		t.Fatalf("max_commits = %d, want current compatibility baseline 1", payload.MaxCommits)
	}
}

func TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	requested := []TaskDef{{
		Agent:  "reviewer",
		Goal:   "Produce workset for the last 10 commits.",
		Action: &Action{Payload: `{"max_commits":10}`},
	}}
	bound, _, err := CompileTaskGoalContracts(session, requested)
	if err != nil {
		t.Fatalf("CompileTaskGoalContracts: %v", err)
	}
	if len(bound) != 1 || bound[0].Action == nil {
		t.Fatalf("bound tasks = %#v", bound)
	}
	var payload struct {
		MaxCommits int `json:"max_commits"`
	}
	if err := json.Unmarshal([]byte(bound[0].Action.Payload), &payload); err != nil {
		t.Fatalf("decode bound payload: %v", err)
	}
	if payload.MaxCommits != 1 {
		t.Fatalf("prompt/model payload changed static scope to %d, want current baseline 1", payload.MaxCommits)
	}
}
