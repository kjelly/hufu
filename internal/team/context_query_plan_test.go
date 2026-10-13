package team

import (
	json "encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContextQueryPlanPreservesPrimaryIdentityAndBoundsSources(t *testing.T) {
	request := ContextRequest{SchemaVersion: 1, RunID: "run", TaskID: "1", Attempt: 1, Goal: "修復路由", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch, Constraints: strings.Repeat("約束", 300), VerificationCriteria: strings.Repeat("檢查", 200)}
	request.AssignRequestID()
	before := request
	hints := ContextQueryHints{Paths: []string{"ledger/State.go", "ledger/state.go", strings.Repeat("a", 129) + "/long.go"}, PathSourceID: "workset:item", PathSourceHash: "source-hash", Dependencies: []ContextQueryDependencyHint{{TaskID: "accepted", Summary: strings.Repeat("摘要", 200)}, {TaskID: "other", Summary: "other summary"}}}
	plan := BuildContextQueryPlan(request, hints)
	if !reflect.DeepEqual(request, before) || request.Fingerprint() != before.Fingerprint() || plan.Primary != request.RetrievalQuery() {
		t.Fatal("query plan changed v1 identity/primary")
	}
	if utf8.RuneCountInString(plan.Supplemental) > 1024 || utf8.RuneCountInString(plan.Primary)+utf8.RuneCountInString(plan.Supplemental) > 4096 {
		t.Fatal("query text exceeded its invocation bound")
	}
	if len(plan.Paths) != 2 || plan.Paths[0] != "ledger/State.go" || plan.Paths[1] != "ledger/state.go" || plan.TruncatedPaths != 1 {
		t.Fatalf("path bounds/case: %+v", plan.Paths)
	}
	for _, source := range plan.Provenance {
		if source.Kind == "constraints" && source.ContentHash != contextQueryDigest(strings.Repeat("約束", 192)) {
			t.Fatal("constraints did not use a 384-rune bound")
		}
	}
	again := BuildContextQueryPlan(request, hints)
	if plan.PlanHash != again.PlanHash {
		t.Fatal("query plan is nondeterministic")
	}
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{request.Goal, "約束", "摘要", "ledger/State.go"} {
		if strings.Contains(string(data), raw) {
			t.Fatalf("raw hint persisted: %q", raw)
		}
	}
}

func TestContextQueryPlanWholePathsAndLegacyFallback(t *testing.T) {
	for _, test := range []struct {
		goal string
		want ContextQueryFallback
	}{{strings.Repeat("x", 4097), QueryFallbackInputOverLimit}, {"normal", QueryFallbackNoHints}} {
		request := ContextRequest{Goal: test.goal, Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
		plan := BuildContextQueryPlan(request, ContextQueryHints{})
		if plan.Fallback != test.want || plan.Primary != request.RetrievalQuery() || plan.Supplemental != "" {
			t.Fatalf("fallback changed primary: %+v", plan)
		}
	}
	request := ContextRequest{Goal: strings.Repeat("x", 4040), Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
	plan := BuildContextQueryPlan(request, ContextQueryHints{Paths: []string{"long_directory_name/CanonicalState.go"}})
	if len(plan.Paths) != 0 || plan.TruncatedPaths != 1 {
		t.Fatal("path was truncated to fit remaining budget")
	}
}

func TestContextQueryPlanRedactsBeforeHashingAndTruncation(t *testing.T) {
	request := ContextRequest{Goal: "normal", Constraints: "api_key=sk-test-secret-value-never-persist", Phase: PhaseExecute, Trigger: ContextTriggerTaskDispatch}
	plan := BuildContextQueryPlan(request, ContextQueryHints{})
	if strings.Contains(plan.Supplemental, "sk-test-secret-value") {
		t.Fatal("supplemental query leaked secret")
	}
	data, _ := json.Marshal(plan)
	if strings.Contains(string(data), "sk-test-secret-value") {
		t.Fatal("query provenance leaked secret")
	}
}
