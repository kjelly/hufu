package main

import (
	"bytes"
	"encoding/json"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

// TestContextExplainMemoryUsesAdoptedPolicyAndRetrievalID is the CLI/JSON
// regression for spec §7 HF-MEM4-005: explain-memory must report the final
// score computed with the adopted runtime policy (not the default weights) and
// must include the retrieval ID from the durable injection manifest.
func TestContextExplainMemoryUsesAdoptedPolicyAndRetrievalID(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	item := contextstore.ContextItem{
		ID: "explain-cli", Kind: contextstore.ContextPattern, Content: "goal: deploy the service",
		Scope:     contextstore.Scope{ProjectID: "project-cli", TeamID: "team-cli"},
		Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted,
		Priority: 10, Confidence: 1, UpdatedAt: time.Unix(100, 0),
	}
	if err := repo.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	// Adopt a non-default active policy: utility and freshness are ignored, so
	// the explained final score must be pure base relevance * applicability *
	// trust instead of the default-policy weighted score. The minimum relevance
	// is lowered below the item's lexical score so applicability is 1.
	learning := agent.DefaultMemoryLearningPolicy()
	learning.Mode = agent.MemoryLearningActive
	learning.PolicyVersion = "memory-policy-v2"
	snapshot := map[string]any{
		"id": learning.PolicyVersion, "revision_hash": "rev-2", "learning": learning,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.01, "utility_weight": 0.0, "freshness_weight": 0.0},
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(t.Context(), learning.PolicyVersion, data, "rev-2", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Durable manifest binding the item to a retrieval ID under the exact
	// policy version and query being explained.
	manifest := team.MemoryInjectionManifest{
		RetrievalID: "retrieval-cli-123", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker",
		PolicyVersion: learning.PolicyVersion, QueryHash: team.QueryHash("goal"),
		Items:     []team.MemoryInjectionItem{{ContextItemID: item.ID, Source: "shared_persistent", Rank: 1}},
		CreatedAt: time.Now().UTC(),
	}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{{ID: "task-1", MemoryManifests: []team.MemoryInjectionManifest{manifest}}}
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "explain-memory", item.ID, "--workspace", workspace, "--project", "project-cli", "--team", "team-cli", "--query", "goal", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var explanation team.MemoryScoreExplanation
	if err := json.Unmarshal(out.Bytes(), &explanation); err != nil {
		t.Fatalf("invalid JSON output %q: %v", out.String(), err)
	}
	if explanation.ContextItemID != item.ID {
		t.Fatalf("context item id = %q, want %q", explanation.ContextItemID, item.ID)
	}
	if explanation.PolicyVersion != learning.PolicyVersion {
		t.Fatalf("policy version = %q, want %q", explanation.PolicyVersion, learning.PolicyVersion)
	}
	if explanation.RetrievalID != manifest.RetrievalID {
		t.Fatalf("retrieval id = %q, want %q", explanation.RetrievalID, manifest.RetrievalID)
	}
	// The final score must be computed with the adopted runtime weights
	// (utility and freshness ignored), not the default policy.
	parts := explanation.ScoreParts
	want := parts.BaseRelevance * parts.Applicability * (0.75 + 0*parts.UtilityLowerBound) * math.Pow(parts.Freshness, 0) * parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
	if math.Abs(explanation.FinalScore-want) > 1e-9 {
		t.Fatalf("final score = %f, want %f (base=%f)", explanation.FinalScore, want, parts.BaseRelevance)
	}
	// Sanity: the default policy would produce a different score, proving the
	// adopted policy actually changed the explained result.
	defaultScore := parts.BaseRelevance * parts.Applicability * (0.75 + 0.5*parts.UtilityLowerBound) * math.Pow(parts.Freshness, 1) * parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
	if math.Abs(defaultScore-want) < 1e-9 {
		t.Fatalf("test setup: default and adopted policies should differ (both %f)", want)
	}
}

// TestContextExplainMemoryBindsPolicyAndRetrievalID is the CLI/JSON regression
// for the review finding that explain-memory could mix the requested policy
// aggregate, the active weights, and an unrelated manifest retrieval ID. With
// two recorded policies (v1 active with utility=0/freshness=0, v2 recorded with
// utility=0.5/freshness=1) and two manifests bound to different policy/query
// pairs, an explicit --policy-version must resolve to that version's own
// snapshot and weights, and the retrieval ID must come only from a manifest
// matching the exact policy version and query being explained.
func TestContextExplainMemoryBindsPolicyAndRetrievalID(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	item := contextstore.ContextItem{
		ID: "explain-cli", Kind: contextstore.ContextPattern, Content: "goal v1 v2 deploy",
		Scope:     contextstore.Scope{ProjectID: "project-cli", TeamID: "team-cli"},
		Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted,
		Priority: 10, Confidence: 1, UpdatedAt: time.Unix(100, 0),
	}
	if err := repo.Append(t.Context(), item); err != nil {
		t.Fatal(err)
	}
	// v1 is active with utility=0/freshness=0; v2 is recorded (not active) with
	// utility=0.5/freshness=1. Both lower minimum relevance below the item's
	// lexical score so applicability is 1.
	learningV1 := agent.DefaultMemoryLearningPolicy()
	learningV1.Mode = agent.MemoryLearningActive
	learningV1.PolicyVersion = "memory-policy-v1"
	snapshotV1 := map[string]any{
		"id": learningV1.PolicyVersion, "revision_hash": "rev-1", "learning": learningV1,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.01, "utility_weight": 0.0, "freshness_weight": 0.0},
	}
	dataV1, err := json.Marshal(snapshotV1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(t.Context(), "memory-policy-v1", dataV1, "rev-1", "active", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	learningV2 := agent.DefaultMemoryLearningPolicy()
	learningV2.Mode = agent.MemoryLearningActive
	learningV2.PolicyVersion = "memory-policy-v2"
	snapshotV2 := map[string]any{
		"id": learningV2.PolicyVersion, "revision_hash": "rev-2", "learning": learningV2,
		"retrieval": map[string]any{"top_k": 20, "minimum_relevance": 0.01, "utility_weight": 0.5, "freshness_weight": 1},
	}
	dataV2, err := json.Marshal(snapshotV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveMemoryPolicyVersion(t.Context(), "memory-policy-v2", dataV2, "rev-2", "candidate", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// Two durable manifests: v1 bound to query "goal v1", v2 bound to query
	// "goal v2". v2 is newer, so a lookup that ignored policy/query would
	// wrongly return retrieval-v2 for a v1 request.
	now := time.Now().UTC()
	manifestV1 := team.MemoryInjectionManifest{
		RetrievalID: "retrieval-v1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker",
		PolicyVersion: "memory-policy-v1", QueryHash: team.QueryHash("goal v1"),
		Items:     []team.MemoryInjectionItem{{ContextItemID: item.ID, Source: "shared_persistent", Rank: 1}},
		CreatedAt: now,
	}
	manifestV2 := team.MemoryInjectionManifest{
		RetrievalID: "retrieval-v2", RunID: "run-2", TaskID: "task-2", Attempt: 1, Agent: "worker",
		PolicyVersion: "memory-policy-v2", QueryHash: team.QueryHash("goal v2"),
		Items:     []team.MemoryInjectionItem{{ContextItemID: item.ID, Source: "shared_persistent", Rank: 1}},
		CreatedAt: now.Add(time.Second),
	}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{
		{ID: "task-1", MemoryManifests: []team.MemoryInjectionManifest{manifestV1}},
		{ID: "task-2", MemoryManifests: []team.MemoryInjectionManifest{manifestV2}},
	}
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}

	// --policy-version v1 --query "goal v1": must use v1's own weights and
	// return v1's retrieval ID, never v2's.
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "explain-memory", item.ID, "--workspace", workspace, "--project", "project-cli", "--team", "team-cli", "--query", "goal v1", "--policy-version", "memory-policy-v1", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var explanation team.MemoryScoreExplanation
	if err := json.Unmarshal(out.Bytes(), &explanation); err != nil {
		t.Fatalf("invalid JSON output %q: %v", out.String(), err)
	}
	if explanation.PolicyVersion != "memory-policy-v1" {
		t.Fatalf("v1 policy version = %q, want memory-policy-v1", explanation.PolicyVersion)
	}
	if explanation.RetrievalID != "retrieval-v1" {
		t.Fatalf("v1 retrieval id = %q, want retrieval-v1", explanation.RetrievalID)
	}
	parts := explanation.ScoreParts
	wantV1 := parts.BaseRelevance * parts.Applicability * (0.75 + 0*parts.UtilityLowerBound) * math.Pow(parts.Freshness, 0) * parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
	if math.Abs(explanation.FinalScore-wantV1) > 1e-9 {
		t.Fatalf("v1 final score = %f, want %f (base=%f)", explanation.FinalScore, wantV1, parts.BaseRelevance)
	}
	// The v2 weights on the same parts must produce a different score, proving
	// the v1 request did not silently use the active/v2 weights.
	wrongV1 := parts.BaseRelevance * parts.Applicability * (0.75 + 0.5*parts.UtilityLowerBound) * math.Pow(parts.Freshness, 1) * parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
	if math.Abs(wantV1-wrongV1) < 1e-9 {
		t.Fatalf("test setup: v1 and v2 weights should differ on the same parts (both %f)", wantV1)
	}

	// --policy-version v2 --query "goal v2": must use v2's own weights and
	// return v2's retrieval ID.
	root = newRootCommand()
	out = new(bytes.Buffer)
	root.SetOut(out)
	root.SetArgs([]string{"context", "explain-memory", item.ID, "--workspace", workspace, "--project", "project-cli", "--team", "team-cli", "--query", "goal v2", "--policy-version", "memory-policy-v2", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &explanation); err != nil {
		t.Fatalf("invalid JSON output %q: %v", out.String(), err)
	}
	if explanation.PolicyVersion != "memory-policy-v2" {
		t.Fatalf("v2 policy version = %q, want memory-policy-v2", explanation.PolicyVersion)
	}
	if explanation.RetrievalID != "retrieval-v2" {
		t.Fatalf("v2 retrieval id = %q, want retrieval-v2", explanation.RetrievalID)
	}
	parts = explanation.ScoreParts
	wantV2 := parts.BaseRelevance * parts.Applicability * (0.75 + 0.5*parts.UtilityLowerBound) * math.Pow(parts.Freshness, 1) * parts.TrustFactor - parts.HarmfulUsePenalty - parts.StaleEnvironmentPenalty
	if math.Abs(explanation.FinalScore-wantV2) > 1e-9 {
		t.Fatalf("v2 final score = %f, want %f (base=%f)", explanation.FinalScore, wantV2, parts.BaseRelevance)
	}
}
