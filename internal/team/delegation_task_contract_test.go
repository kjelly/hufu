package team

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestRequireTaskContractRejectsTasksMissingConstraintsOrVerify(t *testing.T) {
	cases := []struct {
		name    string
		tasks   []TaskDef
		wantErr []string
	}{
		{name: "goal only", tasks: []TaskDef{{Agent: "implementer", Goal: "fix the banner"}}, wantErr: []string{"implementer (missing constraints and verify)"}},
		{name: "no verify", tasks: []TaskDef{{Agent: "implementer", Goal: "fix", Constraints: "rename the line"}}, wantErr: []string{"implementer (missing verify)"}},
		{name: "no constraints", tasks: []TaskDef{{Agent: "implementer", Goal: "fix", Verify: "go test ./cmd/hufu/"}}, wantErr: []string{"implementer (missing constraints)"}},
		{name: "full contract", tasks: []TaskDef{{Agent: "implementer", Goal: "fix", Constraints: "rename the line", Verify: "go test ./cmd/hufu/"}}},
		{name: "verify spec counts as verify", tasks: []TaskDef{{Agent: "implementer", Goal: "fix", Constraints: "rename", VerifySpec: &VerificationSpec{Type: agent.VerifyCommandExit, Command: "go test ./..."}}}},
		{name: "unlisted worker keeps a short task", tasks: []TaskDef{{Agent: "analyst", Goal: "look around"}}},
		{
			name:    "only the incomplete task is named",
			tasks:   []TaskDef{{Agent: "analyst", Goal: "look"}, {Agent: "Implementer", Goal: "fix", Constraints: "  "}},
			wantErr: []string{"Implementer (missing constraints and verify)", "user's explicit requirements"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coordinator{taskTracker: NewTaskTracker(), session: &TeamSession{Config: agent.TeamConfig{
				Delegation: agent.DelegationPolicy{RequireTaskContract: []string{"implementer"}},
			}}}
			err := c.validateDelegationPolicy(tc.tasks)
			if len(tc.wantErr) == 0 {
				if err != nil {
					t.Fatalf("validateDelegationPolicy() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("validateDelegationPolicy() = nil, want a rejection")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("rejection %q does not mention %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "analyst") {
				t.Fatalf("rejection names the unlisted worker: %v", err)
			}
		})
	}
}

func TestOriginalRequestForSharedWorkers(t *testing.T) {
	cases := []struct {
		name    string
		prompt  string
		entries []SessionEntry
		agent   string
		want    string
	}{
		{name: "listed worker gets the request", prompt: "Rename the banner to Memory index", agent: "verifier", want: "Rename the banner to Memory index"},
		{name: "unlisted worker gets nothing", prompt: "Rename the banner", agent: "analyst"},
		{
			name:   "resume uses the latest earlier request",
			prompt: ResumeInstruction,
			entries: []SessionEntry{
				{Role: "user", Content: "old request"},
				{Role: "user", Content: "Rename the banner to Memory index"},
				{Role: "assistant", Content: "working"},
				{Role: "user", Content: ResumeInstruction + " Tasks 2 require a materially changed plan."},
			},
			agent: "Verifier",
			want:  "Rename the banner to Memory index",
		},
		{name: "resume without an earlier request", prompt: ResumeInstruction, entries: []SessionEntry{{Role: "user", Content: ResumeInstruction}}, agent: "verifier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sd := NewSession()
			sd.Entries = tc.entries
			c := &Coordinator{initialPrompt: tc.prompt, sessionData: sd, session: &TeamSession{Config: agent.TeamConfig{
				Delegation: agent.DelegationPolicy{ShareRequestWith: []string{"implementer", "verifier"}},
			}}}
			if got := c.originalRequestFor(tc.agent); got != tc.want {
				t.Fatalf("originalRequestFor(%q) = %q, want %q", tc.agent, got, tc.want)
			}
		})
	}
}

func TestCompileWorkerContextCarriesOriginalRequestAsReference(t *testing.T) {
	long := strings.Repeat("x", maxOriginalRequestRunes+10)
	cases := []struct {
		name      string
		request   string
		want      string
		truncated bool
	}{
		{name: "shared request", request: "Rename the banner to Memory index and add a Memory learning line", want: "Memory learning line"},
		{name: "no request"},
		{name: "long request is bounded", request: long, want: "[request truncated]", truncated: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			compiled, err := CompileWorkerContext(context.Background(), WorkerContextInput{
				Goal: "fix the banner", OriginalRequest: tc.request,
				ModelContext: ModelContextSpec{ModelID: "test", ContextWindow: 32768, MaxOutputTokens: 512},
			})
			if err != nil {
				t.Fatalf("CompileWorkerContext: %v", err)
			}
			included := false
			for _, item := range compiled.IncludedItems {
				if item.ID == "original_request" {
					included = true
					if item.Authority != ContextAuthorityHistorical || !item.Required {
						t.Fatalf("original request item = %+v, want required historical reference", item)
					}
				}
			}
			if included != (tc.request != "") || (tc.want != "" && !strings.Contains(compiled.Prompt, tc.want)) {
				t.Fatalf("original request included=%v, prompt missing %q:\n%.400s", included, tc.want, compiled.Prompt)
			}
			if tc.truncated && strings.Contains(compiled.Prompt, long) {
				t.Fatal("long request was not truncated")
			}
		})
	}
}
