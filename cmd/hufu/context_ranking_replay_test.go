package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

func rankingReplayWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	var items []contextstore.ContextItem
	for i := range 40 {
		items = append(items, contextstore.ContextItem{ID: fmt.Sprintf("filler-%02d", i), Kind: contextstore.ContextPattern, Content: fmt.Sprintf("filler note %d", i), Scope: contextstore.Scope{ProjectID: "proj1", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed})
	}
	for i, content := range []string{"rollback deploy procedure", "rollback deploy notes", "rollback deploy on canary failure"} {
		items = append(items, contextstore.ContextItem{ID: fmt.Sprintf("hit-%d", i), Kind: contextstore.ContextPattern, Content: content, Scope: contextstore.Scope{ProjectID: "proj1", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed})
	}
	if err := repo.Append(context.Background(), items...); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func runRankingReplayCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	contextWorkspace, contextProject, contextTeam, contextQueryJSON, contextPolicyVersion = "", "", "", false, "memory-policy-v1"
	rankingReplayQueries, rankingReplayQueriesFile, rankingReplayFromSession, rankingReplayFusion = nil, "", false, string(contextstore.FusionRRFNormalized)
	contextRankingReplayCmd.Flags().Lookup("policy-version").Changed = false
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(new(bytes.Buffer))
	root.SetArgs(append([]string{"context", "ranking-replay"}, args...))
	err := root.Execute()
	return out.String(), err
}

func workspaceSnapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		lines = append(lines, fmt.Sprintf("%s %x", rel, sha256.Sum256(data)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestContextRankingReplayIsReadOnlyAndContentFree(t *testing.T) {
	workspace := rankingReplayWorkspace(t)
	queriesFile := filepath.Join(t.TempDir(), "queries.txt")
	if err := os.WriteFile(queriesFile, []byte("# comment\n\nrollback deploy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{{ID: "task-1", Goal: "rollback deploy after the failed canary"}}
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	before := workspaceSnapshot(t, workspace)
	out, err := runRankingReplayCLI(t, "--workspace", workspace, "--project", "proj1", "--team", "demo", "--query", "rollback deploy", "--queries-file", queriesFile, "--from-session", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if after := workspaceSnapshot(t, workspace); after != before {
		t.Fatalf("ranking replay changed the workspace:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var report team.RankingReplayReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	sources := map[string]int{}
	for _, query := range report.Queries {
		sources[query.Source]++
	}
	if report.QueryCount != 3 || sources["flag"] != 1 || sources["file"] != 1 || sources["session_task"] != 1 || report.EligibleItems != 43 || report.OutcomeMetricsAvailable {
		t.Fatalf("report = %+v sources=%v", report, sources)
	}
	for _, secret := range []string{"rollback deploy", "canary", "filler note"} {
		if strings.Contains(out, secret) {
			t.Fatalf("ranking replay leaked %q: %s", secret, out)
		}
	}
	text, err := runRankingReplayCLI(t, "--workspace", workspace, "--project", "proj1", "--team", "demo", "--query", "rollback deploy")
	if err != nil || !strings.Contains(text, "baseline=legacy candidate=rrf_normalized") || !strings.Contains(text, "relevance: set_changed=") || !strings.Contains(text, "outcome_metrics=unavailable") {
		t.Fatalf("text output = %q err=%v", text, err)
	}
}

func TestContextRankingReplayRejectsInvalidInvocations(t *testing.T) {
	workspace := rankingReplayWorkspace(t)
	cases := []struct {
		args []string
		want string
	}{
		{args: []string{"--workspace", workspace, "--project", "proj1", "--query", "x"}, want: "--project and --team"},
		{args: []string{"--workspace", workspace, "--project", "proj1", "--team", "demo"}, want: "no queries"},
		{args: []string{"--workspace", workspace, "--project", "proj1", "--team", "demo", "--query", "x", "--candidate-fusion", "rrf"}, want: "unknown candidate fusion"},
	}
	for _, tc := range cases {
		if _, err := runRankingReplayCLI(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v err = %v, want %q", tc.args, err, tc.want)
		}
	}
}
