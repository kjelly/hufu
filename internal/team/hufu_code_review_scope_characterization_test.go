package team

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
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

func TestHufuCodeReviewUsesTenCommitCompatibilityDefault(t *testing.T) {
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
		MaxCommits string `json:"max_commits"`
	}
	if err := json.Unmarshal([]byte(producer.Action.Payload), &payload); err != nil {
		t.Fatalf("decode produce-workset payload: %v", err)
	}
	if payload.MaxCommits != "10" {
		t.Fatalf("max_commits = %q, want compatibility default 10", payload.MaxCommits)
	}
	if coordinator := session.Agents["coordinator"]; coordinator == nil || !strings.Contains(coordinator.System, "last 10 first-parent") {
		t.Fatalf("coordinator prompt does not carry configured scope: %#v", coordinator)
	}
}

func TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	requested := []TaskDef{{
		Agent:  "reviewer",
		Goal:   "Produce workset for the last 7 commits.",
		Action: &Action{Payload: `{"max_commits":7}`},
	}}
	bound, _, err := CompileTaskGoalContracts(session, requested)
	if err != nil {
		t.Fatalf("CompileTaskGoalContracts: %v", err)
	}
	if len(bound) != 1 || bound[0].Action == nil {
		t.Fatalf("bound tasks = %#v", bound)
	}
	var payload struct {
		MaxCommits string `json:"max_commits"`
	}
	if err := json.Unmarshal([]byte(bound[0].Action.Payload), &payload); err != nil {
		t.Fatalf("decode bound payload: %v", err)
	}
	if payload.MaxCommits != "10" {
		t.Fatalf("prompt/model payload changed static scope to %q, want configured default 10", payload.MaxCommits)
	}
}

func TestHufuCodeReviewCompatibilityScopeCanBeOverridden(t *testing.T) {
	session, err := LoadTeam(hufuCodeReviewTeamDir(t), map[string]string{"review.scope.max_commits": "3"}, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-code-review): %v", err)
	}
	for _, task := range session.ContractTasks {
		if task.ID != "produce-workset" || task.Action == nil {
			continue
		}
		var payload struct {
			MaxCommits string `json:"max_commits"`
		}
		if err := json.Unmarshal([]byte(task.Action.Payload), &payload); err != nil {
			t.Fatalf("decode produce-workset payload: %v", err)
		}
		if payload.MaxCommits != "3" {
			t.Fatalf("max_commits = %q, want override 3", payload.MaxCommits)
		}
		if coordinator := session.Agents["coordinator"]; coordinator == nil || !strings.Contains(coordinator.System, "last 3 first-parent") {
			t.Fatalf("coordinator prompt does not carry overridden scope: %#v", coordinator)
		}
		return
	}
	t.Fatal("hufu-code-review produce-workset static action is missing")
}
