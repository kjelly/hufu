package team

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func learningPolicy(mode agent.MemoryLearningMode) agent.MemoryLearningPolicy {
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = mode
	return policy
}

// A fresh session withholds the prior session's archive in every case, but a
// team with learning on still reads canonical context.
func TestCanonicalMemoryDisabledSeparatesArchiveFromKnowledge(t *testing.T) {
	tests := []struct {
		name           string
		mode           agent.MemoryLearningMode
		newSession     bool
		freshProfile   bool
		wantArchiveOff bool
		wantCanonOff   bool
	}{
		{name: "resumed session, learning off", mode: agent.MemoryLearningOff},
		{name: "resumed session, learning on", mode: agent.MemoryLearningObserve},
		{name: "--new, learning off", mode: agent.MemoryLearningOff, newSession: true, wantArchiveOff: true, wantCanonOff: true},
		{name: "--new, learning on", mode: agent.MemoryLearningObserve, newSession: true, wantArchiveOff: true},
		{name: "fresh-session profile, learning off", mode: agent.MemoryLearningOff, freshProfile: true, wantArchiveOff: true, wantCanonOff: true},
		{name: "fresh-session profile, learning on", mode: agent.MemoryLearningShadow, freshProfile: true, wantArchiveOff: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{MemoryLearning: learningPolicy(tt.mode)}}, taskTracker: NewTaskTracker()}
			if tt.freshProfile {
				c.SetExecutionProfile(BuiltinProfiles()[ProfileFreshSession])
			}
			c.SetFreshSession(tt.newSession)
			if got := c.historicalMemoryDisabled(); got != tt.wantArchiveOff {
				t.Fatalf("historicalMemoryDisabled() = %v, want %v", got, tt.wantArchiveOff)
			}
			if got := c.canonicalMemoryDisabled(); got != tt.wantCanonOff {
				t.Fatalf("canonicalMemoryDisabled() = %v, want %v", got, tt.wantCanonOff)
			}
		})
	}
}

// DisableMemory withholds only the archive sources; DisableCanonicalMemory
// withholds canonical context. Both compilers honor the split.
func TestCompileContextSplitsArchiveFromCanonicalMemory(t *testing.T) {
	bundle := &CanonicalContextBundle{SharedPersistent: []contextstore.ContextItem{{
		ID: "persistent-1", Kind: contextstore.ContextPattern, Content: "canon-marker procedure",
		ContentHash: "canon-hash", Lifecycle: contextstore.LifecycleConfirmed, Confidence: 1,
	}}}
	spec := ModelContextSpec{ModelID: "test", ContextWindow: 16000, MaxOutputTokens: 256, SafetyMarginTokens: 64}
	tests := []struct {
		name           string
		disableArchive bool
		disableCanon   bool
		wantCanon      bool
		wantArchive    bool
	}{
		{name: "fresh with learning: canonical only", disableArchive: true, wantCanon: true},
		{name: "fresh without learning: nothing", disableArchive: true, disableCanon: true},
		{name: "resumed: canonical takes precedence over raw files", wantCanon: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			worker, err := CompileWorkerContext(t.Context(), WorkerContextInput{
				Goal: "apply the procedure", CanonicalMemory: bundle,
				RawSTM: "# Findings\n- raw-stm-marker", RawLTM: "# Patterns\n- raw-ltm-marker",
				DisableMemory: tt.disableArchive, DisableCanonicalMemory: tt.disableCanon, ModelContext: spec,
			})
			if err != nil {
				t.Fatal(err)
			}
			coordinator, err := CompileCoordinatorContext(t.Context(), CoordinatorContextInput{
				Goal: "apply the procedure", CanonicalMemory: bundle, SessionContext: "session-marker",
				RawSTM: "# Progress\n- raw-stm-marker", RawLTM: "# Patterns\n- raw-ltm-marker",
				DisableMemory: tt.disableArchive, DisableCanonicalMemory: tt.disableCanon, ModelContext: spec,
			})
			if err != nil {
				t.Fatal(err)
			}
			for label, prompt := range map[string]string{"worker": worker.Prompt, "coordinator": coordinator.Prompt} {
				if got := strings.Contains(prompt, "canon-marker"); got != tt.wantCanon {
					t.Fatalf("%s canonical present = %v, want %v:\n%s", label, got, tt.wantCanon, prompt)
				}
				for _, marker := range []string{"raw-stm-marker", "raw-ltm-marker"} {
					if got := strings.Contains(prompt, marker); got != tt.wantArchive {
						t.Fatalf("%s %s present = %v, want %v:\n%s", label, marker, got, tt.wantArchive, prompt)
					}
				}
			}
			if got := strings.Contains(coordinator.Prompt, "session-marker"); got == tt.disableArchive {
				t.Fatalf("coordinator session context present = %v with DisableMemory=%v", got, tt.disableArchive)
			}
		})
	}
}

// End to end through the coordinator prompt: a fresh session reads confirmed
// persistent knowledge only when learning is on, and never the legacy archive
// files that fresh start fills from the prior session.
func TestFreshSessionCoordinatorPromptReadsPersistentKnowledgeOnlyWithLearning(t *testing.T) {
	tests := []struct {
		name          string
		mode          agent.MemoryLearningMode
		fresh         bool
		wantKnowledge bool
	}{
		{name: "fresh session, learning on", mode: agent.MemoryLearningObserve, fresh: true, wantKnowledge: true},
		{name: "fresh session, learning off", mode: agent.MemoryLearningOff, fresh: true},
		{name: "resumed session, learning on", mode: agent.MemoryLearningObserve, wantKnowledge: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			workspace := t.TempDir()
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			const teamName = "fresh-knowledge"
			if err := repo.Append(ctx, contextstore.ContextItem{
				ID: "persistent-1", Kind: contextstore.ContextPattern,
				Content:   "Calibrate the beacon with the persistent-knowledge-marker procedure",
				Scope:     contextstore.Scope{ProjectID: "project", TeamID: teamName},
				Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
				Confidence: 1, MustKeep: true, Lifecycle: contextstore.LifecycleConfirmed,
				Metadata: map[string]string{"visibility": "shared", "memory_lifetime": "persistent"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := SaveSTM(workspace, "# Progress\n- stale-archive-only: prior session progress"); err != nil {
				t.Fatal(err)
			}
			if err := SaveLTM(workspace, teamName, "# Prior run\n- stale-archive-only: prior session archive"); err != nil {
				t.Fatal(err)
			}
			orch := &agent.AgentDef{Name: "coordinator", Role: "coordinator", System: "coordinate safely", Generation: agent.GenerationParams{Model: "test"}}
			c := &Coordinator{
				session: &TeamSession{
					Workspace: workspace,
					Scope:     WorkspaceScope{ContextScopeID: "project"},
					Config:    agent.TeamConfig{Name: teamName, MemoryLearning: learningPolicy(tt.mode)},
					Agents:    map[string]*agent.AgentDef{"coordinator": orch},
				},
				contextRepo: repo,
				taskTracker: NewTaskTracker(),
				sessionData: NewSession(),
			}
			c.SetFreshSession(tt.fresh)
			prompt, err := c.buildSystemPrompt(ctx, orch, "Calibrate the beacon", false)
			if err != nil {
				t.Fatalf("buildSystemPrompt: %v", err)
			}
			if got := strings.Contains(prompt, "persistent-knowledge-marker"); got != tt.wantKnowledge {
				t.Fatalf("persistent knowledge present = %v, want %v:\n%s", got, tt.wantKnowledge, prompt)
			}
			if strings.Contains(prompt, "stale-archive-only") {
				t.Fatalf("prompt leaked the legacy archive:\n%s", prompt)
			}
		})
	}
}

// Worker, direct-agent, and sub-agent dispatch all route through
// canonicalContextBundleForRequest, so its gate decides whether a fresh worker
// sees persistent knowledge.
func TestFreshSessionWorkerBundleCarriesPersistentKnowledgeOnlyWithLearning(t *testing.T) {
	tests := []struct {
		name          string
		mode          agent.MemoryLearningMode
		wantKnowledge bool
	}{
		{name: "learning on", mode: agent.MemoryLearningObserve, wantKnowledge: true},
		{name: "learning off", mode: agent.MemoryLearningOff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			workspace := t.TempDir()
			repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.Close() })
			const teamName = "fresh-knowledge"
			if err := repo.Append(ctx, contextstore.ContextItem{
				ID: "persistent-1", Kind: contextstore.ContextPattern,
				Content:   "Calibrate the beacon with the persistent-knowledge-marker procedure",
				Scope:     contextstore.Scope{ProjectID: "project", TeamID: teamName},
				Authority: contextstore.AuthorityAgent, TrustLevel: contextstore.TrustInternal,
				Confidence: 1, MustKeep: true, Lifecycle: contextstore.LifecycleConfirmed,
				Metadata: map[string]string{"visibility": "shared", "memory_lifetime": "persistent"},
			}); err != nil {
				t.Fatal(err)
			}
			worker := &agent.AgentDef{Name: "worker", Role: "worker"}
			c := &Coordinator{
				session: &TeamSession{
					Workspace: workspace,
					Scope:     WorkspaceScope{ContextScopeID: "project"},
					Config:    agent.TeamConfig{Name: teamName, MemoryLearning: learningPolicy(tt.mode)},
					Agents:    map[string]*agent.AgentDef{"worker": worker},
				},
				contextRepo: repo,
				taskTracker: NewTaskTracker(),
				sessionData: NewSession(),
			}
			c.SetFreshSession(true)
			request := c.newTaskContextRequest(TaskDef{Agent: "worker", Goal: "Calibrate the beacon"}, "1", 1, ContextTriggerTaskDispatch, "worker", worker.Role, nil)
			bundle, _, canonical, err := c.canonicalContextBundleForRequest(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			got := canonical && bundle != nil && len(bundle.SharedPersistent) == 1 && bundle.SharedPersistent[0].ID == "persistent-1"
			if got != tt.wantKnowledge {
				t.Fatalf("worker bundle carries persistent knowledge = %v, want %v (canonical=%v bundle=%+v)", got, tt.wantKnowledge, canonical, bundle)
			}
		})
	}
}
