package team

import (
	"context"
	"slices"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestImplicitToolsTheArtifactPolicyRefusesAreHidden(t *testing.T) {
	c := &Coordinator{}
	candidate := []fantasy.AgentTool{
		tools.NewViewTool(),
		&canonicalMemoryQueryTool{coordinator: c},
		&todoTool{coordinator: c},
		tools.NewRandomTool(),
	}
	cases := []struct {
		name     string
		declared string
		bound    bool
		readOnly bool
		want     []string
	}{
		// The 2026-10-01 critic: unbound, declares only view, and was shown
		// memory_query, which every call refused. memory_query is now
		// scoped to the calling worker and allowed; other team tools stay
		// hidden.
		{name: "unbound keeps scoped memory_query and hides other team tools", declared: "view", want: []string{"view", "memory_query", "random"}},
		{name: "bound hides every implicit untrusted tool", declared: "view", bound: true, want: []string{"view"}},
		{name: "unbound keeps a declared tool for the contract error", declared: "view,memory_query", want: []string{"view", "memory_query", "random"}},
		{name: "bound keeps a declared tool for the preflight error", declared: "view,random", bound: true, want: []string{"view", "random"}},
		{name: "readonly hides implicit memory query rejected by policy", declared: "view", readOnly: true, want: []string{"view", "random"}},
		{name: "readonly preserves explicitly declared capabilities", declared: "view,memory_query", readOnly: true, want: []string{"view", "memory_query", "random"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := &agent.AgentDef{Name: "critic", Tools: tc.declared}
			kept := filterImplicitArtifactPolicyDeniedTools(def, candidate, tc.bound, tc.readOnly)
			if got := agentToolNames(kept); !slices.Equal(got, tc.want) {
				t.Fatalf("surface = %v, want %v", got, tc.want)
			}
			policyCtx := context.WithValue(context.Background(), tools.ArtifactPathPolicyKey, workerArtifactPathPolicy(def, tc.bound, nil))
			for _, tool := range kept {
				name := tool.Info().Name
				if agentDeclaresToolOrAlias(def.Tools, name) {
					continue
				}
				if denial := artifactScopeToolDenial(policyCtx, name, tool); denial != "" {
					t.Fatalf("implicit tool %q stayed visible although the attempt policy refuses it: %s", name, denial)
				}
				if tc.readOnly && readOnlyToolMutation(name, "") {
					t.Fatalf("implicit tool %q is always denied by readonly policy", name)
				}
			}
		})
	}
}
