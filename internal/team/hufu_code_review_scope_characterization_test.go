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

func TestHufuCodeReviewDeclaresTypedScopeAndBindsProducer(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	if len(session.RunInputDefinitions) != 1 {
		t.Fatalf("run input definitions = %#v", session.RunInputDefinitions)
	}
	definition := session.RunInputDefinitions[0]
	if definition.Name != "review.scope" || definition.Resolver == nil || definition.Resolver.ID != "review-scope-v1" {
		t.Fatalf("review scope definition = %#v", definition)
	}
	if string(definition.Default) != `{"count":10,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("review scope default = %s", definition.Default)
	}
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
		Scope map[string]any `json:"scope"`
	}
	if err := json.Unmarshal([]byte(producer.Action.Payload), &payload); err != nil {
		t.Fatalf("decode produce-workset payload: %v", err)
	}
	if payload.Scope["kind"] != "last_n" || payload.Scope["count"] != float64(10) {
		t.Fatalf("static scope = %#v, want typed last_n 10", payload.Scope)
	}
	if len(producer.Action.InputBindings) != 1 || producer.Action.InputBindings[0] != (ActionInputBinding{Input: "review.scope", Target: "/scope"}) {
		t.Fatalf("producer input bindings = %#v", producer.Action.InputBindings)
	}
	acceptance := session.Config.AcceptanceSpec
	if acceptance == nil || len(acceptance.Verifications) != 2 || acceptance.Verifications[0].Type != VerifyTaskOutputAssert || acceptance.Verifications[1].Type != VerifyWorksetComplete {
		t.Fatalf("acceptance wiring = %#v", acceptance)
	}
	if coordinator := session.Agents["coordinator"]; coordinator == nil || strings.Contains(coordinator.System, "Natural-language scope text cannot override") {
		t.Fatalf("coordinator retained compatibility scope warning: %#v", coordinator)
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
		Scope map[string]any `json:"scope"`
	}
	if err := json.Unmarshal([]byte(bound[0].Action.Payload), &payload); err != nil {
		t.Fatalf("decode bound payload: %v", err)
	}
	if payload.Scope["count"] != float64(10) {
		t.Fatalf("prompt/model payload changed static scope to %#v, want configured default 10", payload.Scope)
	}
}

func TestHufuCodeReviewCompatibilityVarNoLongerControlsScope(t *testing.T) {
	session, err := LoadTeam(hufuCodeReviewTeamDir(t), map[string]string{"review.scope.max_commits": "3"}, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-code-review): %v", err)
	}
	for _, task := range session.ContractTasks {
		if task.ID != "produce-workset" || task.Action == nil {
			continue
		}
		var payload struct {
			Scope map[string]any `json:"scope"`
		}
		if err := json.Unmarshal([]byte(task.Action.Payload), &payload); err != nil {
			t.Fatalf("decode produce-workset payload: %v", err)
		}
		if payload.Scope["count"] != float64(10) {
			t.Fatalf("deprecated compatibility var changed typed scope: %#v", payload.Scope)
		}
		if coordinator := session.Agents["coordinator"]; coordinator == nil || strings.Contains(coordinator.System, "last 3 first-parent") {
			t.Fatalf("deprecated compatibility var leaked into coordinator prompt: %#v", coordinator)
		}
		return
	}
	t.Fatal("hufu-code-review produce-workset static action is missing")
}
