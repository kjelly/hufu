package team

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func TestDeclaredShellTools(t *testing.T) {
	cases := []struct {
		name string
		def  *agent.AgentDef
		want []string
	}{
		{name: "nil agent", def: nil, want: nil},
		{name: "empty grant inherits every tool", def: &agent.AgentDef{Tools: ""}, want: nil},
		{name: "all grant inherits every tool", def: &agent.AgentDef{Tools: "all"}, want: nil},
		{name: "explicit shell tools", def: &agent.AgentDef{Tools: "bash,sudo,ssh,view,write,wait_for"}, want: []string{"bash", "sudo", "ssh", "wait_for"}},
		{name: "case and spacing are normalized", def: &agent.AgentDef{Tools: " View , BASH ,lua"}, want: []string{"bash", "lua"}},
		{name: "no shell tools declared", def: &agent.AgentDef{Tools: "view,grep,ls"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaredShellTools(tc.def); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("declaredShellTools() = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestArtifactScopeShellToolsFollowTheAgentGrant(t *testing.T) {
	root := t.TempDir()
	bash := tools.NewBashTool(tools.WithWorkDir(root), tools.WithAllowedPaths([]string{root}))
	lua := tools.NewLuaTool(tools.WithWorkDir(root), tools.WithAllowedPaths([]string{root}))
	external := &recordingTool{name: "external_shell"}
	blocked := []string{root + "/logs/artifacts/data", root + "/logs/artifacts/meta"}
	cases := []struct {
		name       string
		tool       fantasy.AgentTool
		policy     tools.ArtifactPathPolicy
		wantDenied bool
	}{
		{
			name:   "unbound task may run a declared bash",
			tool:   bash,
			policy: tools.ArtifactPathPolicy{BlockedPaths: blocked, DenyUnsupportedDeclaredTools: true, DeclaredShellTools: []string{"bash"}},
		},
		{
			name:       "unbound task may not run an undeclared bash",
			tool:       bash,
			policy:     tools.ArtifactPathPolicy{BlockedPaths: blocked, DenyUnsupportedDeclaredTools: true},
			wantDenied: true,
		},
		{
			name:       "declaring bash does not grant lua",
			tool:       lua,
			policy:     tools.ArtifactPathPolicy{BlockedPaths: blocked, DenyUnsupportedDeclaredTools: true, DeclaredShellTools: []string{"bash"}},
			wantDenied: true,
		},
		{
			name:   "unbound task may run a declared lua",
			tool:   lua,
			policy: tools.ArtifactPathPolicy{BlockedPaths: blocked, DenyUnsupportedDeclaredTools: true, DeclaredShellTools: []string{"lua"}},
		},
		{
			name:       "bound workset task never runs shell tools",
			tool:       bash,
			policy:     tools.ArtifactPathPolicy{BlockedPaths: blocked, FailClosedForUnsupported: true, DeclaredShellTools: []string{"bash"}},
			wantDenied: true,
		},
		{
			name:       "external adapters stay denied for unbound tasks",
			tool:       external,
			policy:     tools.ArtifactPathPolicy{BlockedPaths: blocked, DenyUnsupportedDeclaredTools: true, DeclaredShellTools: []string{"external_shell"}},
			wantDenied: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), tools.ArtifactPathPolicyKey, tc.policy)
			denial := artifactScopeToolDenial(ctx, tc.tool.Info().Name, tc.tool)
			if gotDenied := denial != ""; gotDenied != tc.wantDenied {
				t.Fatalf("artifactScopeToolDenial(%q) = %q, want denied=%v", tc.tool.Info().Name, denial, tc.wantDenied)
			}
			if tc.wantDenied && tc.tool == bash && tc.policy.DenyUnsupportedDeclaredTools && !strings.Contains(denial, "tools:") {
				t.Fatalf("undeclared shell denial %q should tell the author how to grant it", denial)
			}
		})
	}
}
