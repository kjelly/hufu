package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	hulog "github.com/kjelly/hufu/internal/log"
	"github.com/kjelly/hufu/internal/team"
)

// seedAdoptedPolicy writes an active memory-policy snapshot to the workspace's
// canonical store. The snapshot's learning mode controls the value the banner
// reports when the team has no adopted policy of its own.
func seedAdoptedPolicy(t *testing.T, workspace string, mode agent.MemoryLearningMode) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = mode
	snapshot, err := json.Marshal(map[string]any{
		"id":            policy.PolicyVersion,
		"revision_hash": "revision-banner",
		"learning":      policy,
		"retrieval": map[string]any{
			"top_k": 20, "minimum_relevance": 0.05, "utility_weight": 0.5, "freshness_weight": 1.0,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(context.Background(), policy.PolicyVersion, snapshot, "revision-banner", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
}

// captureBannerOutput replaces the stderr writer used by stderrLog for the
// duration of fn and returns whatever was emitted. The writer is restored
// through t.Cleanup so concurrent tests cannot observe each other's output.
func captureBannerOutput(t *testing.T, fn func()) string {
	t.Helper()
	var output bytes.Buffer
	hulog.SetWriter(&output)
	t.Cleanup(func() { hulog.SetWriter(nil) })
	fn()
	return output.String()
}

// withRunOptions saves the current opts, replaces it with the supplied value,
// calls syncLogState so the central logger picks up the new flag state, and
// restores the original through t.Cleanup so concurrent tests do not
// observe each other's CLI state.
func withRunOptions(t *testing.T, next runOptions) {
	t.Helper()
	previous := opts
	opts = next
	syncLogState()
	t.Cleanup(func() {
		opts = previous
		syncLogState()
	})
}

// TestEffectiveMemoryLearningModeBanner covers the banner contract: the
// disabled branch prints `Memory index: disabled`, the enabled branch prints
// `Memory index: enabled (model: …)`, and the optional `Memory learning: …`
// line is emitted only when the effective mode differs from the off sentinel.
// The effective mode matches `hufu context learning`: adopted snapshots win
// over team configuration, and an unadopted `active` is downgraded to
// `shadow`.
func TestEffectiveMemoryLearningModeBanner(t *testing.T) {
	withRunOptions(t, runOptions{memoryEnabled: true, memoryModel: "ollama/nomic-embed-text:latest"})

	cases := []struct {
		name       string
		configure  func(t *testing.T, session *team.TeamSession, workspace string)
		wantLine   string // substring expected in stderr; empty means no banner line about learning
		wantNoLine string // substring that must NOT appear in stderr
	}{
		{
			name:       "empty team mode omits the learning line",
			configure:  func(t *testing.T, session *team.TeamSession, workspace string) {},
			wantNoLine: "Memory learning:",
		},
		{
			name: "team mode off omits the learning line",
			configure: func(t *testing.T, session *team.TeamSession, workspace string) {
				session.Config.MemoryLearning.Mode = agent.MemoryLearningOff
			},
			wantNoLine: "Memory learning:",
		},
		{
			name: "team mode observe prints observe",
			configure: func(t *testing.T, session *team.TeamSession, workspace string) {
				session.Config.MemoryLearning.Mode = agent.MemoryLearningObserve
			},
			wantLine: "Memory learning: observe",
		},
		{
			name: "team mode active is downgraded to shadow",
			configure: func(t *testing.T, session *team.TeamSession, workspace string) {
				session.Config.MemoryLearning.Mode = agent.MemoryLearningActive
			},
			wantLine: "Memory learning: shadow",
		},
		{
			name: "adopted shadow snapshot overrides team observe",
			configure: func(t *testing.T, session *team.TeamSession, workspace string) {
				session.Config.MemoryLearning.Mode = agent.MemoryLearningObserve
				seedAdoptedPolicy(t, workspace, agent.MemoryLearningShadow)
			},
			wantLine: "Memory learning: shadow",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			session := &team.TeamSession{
				Config: agent.TeamConfig{
					Name: "hufu-banner-test",
					MemoryLearning: agent.MemoryLearningPolicy{
						Mode:          "",
						PolicyVersion: "memory-policy-v1",
					},
				},
				Workspace: workspace,
			}
			if tc.configure != nil {
				tc.configure(t, session, workspace)
			}

			out := captureBannerOutput(t, func() {
				if buildMemoryStore("", session) != nil {
					t.Fatalf("buildMemoryStore returned a non-nil store; legacy path should be a no-op seam")
				}
			})

			if !strings.Contains(out, "Memory index: enabled (model: ollama/nomic-embed-text:latest)") {
				t.Fatalf("enabled banner = %q, want the canonical-context-index line", out)
			}
			if strings.Contains(out, "Memory: canonical context index enabled") || strings.Contains(out, "Memory: disabled") {
				t.Fatalf("banner reused legacy wording: %q", out)
			}
			if tc.wantLine != "" && !strings.Contains(out, tc.wantLine) {
				t.Fatalf("banner = %q, want substring %q", out, tc.wantLine)
			}
			if tc.wantNoLine != "" && strings.Contains(out, tc.wantNoLine) {
				t.Fatalf("banner = %q must not contain %q", out, tc.wantNoLine)
			}
		})
	}
}

// TestMemoryLearningBannerDoesNotDependOnTheIndex pins the case the banner
// fix is for: a team learning in observe mode without --memory must still
// say so. The index line keeps its old conditions, including printing
// nothing when a --temp workspace overrides --memory.
func TestMemoryLearningBannerDoesNotDependOnTheIndex(t *testing.T) {
	cases := []struct {
		name      string
		options   runOptions
		mode      agent.MemoryLearningMode
		want      []string
		forbidden []string
	}{
		{
			name:      "index off, learning observe",
			options:   runOptions{},
			mode:      agent.MemoryLearningObserve,
			want:      []string{"Memory index: disabled", "Memory learning: observe"},
			forbidden: []string{"Memory: disabled"},
		},
		{
			name:      "index off, learning off",
			options:   runOptions{},
			mode:      agent.MemoryLearningOff,
			want:      []string{"Memory index: disabled"},
			forbidden: []string{"Memory learning:"},
		},
		{
			name:      "temp workspace overrides the index, learning observe",
			options:   runOptions{memoryEnabled: true, tempWorkspace: true},
			mode:      agent.MemoryLearningObserve,
			want:      []string{"Memory learning: observe"},
			forbidden: []string{"Memory index:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withRunOptions(t, tc.options)
			session := &team.TeamSession{
				Config:    agent.TeamConfig{Name: "hufu-banner-test", MemoryLearning: agent.MemoryLearningPolicy{Mode: tc.mode, PolicyVersion: "memory-policy-v1"}},
				Workspace: t.TempDir(),
			}
			out := captureBannerOutput(t, func() {
				if buildMemoryStore("", session) != nil {
					t.Fatalf("buildMemoryStore returned a non-nil store")
				}
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("banner = %q, want %q", out, want)
				}
			}
			for _, forbidden := range tc.forbidden {
				if strings.Contains(out, forbidden) {
					t.Fatalf("banner = %q must not contain %q", out, forbidden)
				}
			}
		})
	}
}
