package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// scopedConflictLookup answers per (project, team) so tests can prove each
// item is checked in its own team scope.
type scopedConflictLookup struct {
	byScope map[string]map[string][]string
	err     error
	calls   []string
}

func (f *scopedConflictLookup) OpenConflictsForItems(_ context.Context, projectID, teamID string, ids []string) (map[string][]string, error) {
	f.calls = append(f.calls, projectID+"/"+teamID)
	if f.err != nil {
		return nil, f.err
	}
	result := map[string][]string{}
	for _, id := range ids {
		if found := f.byScope[projectID+"/"+teamID][id]; len(found) > 0 {
			result[id] = found
		}
	}
	return result, nil
}

func TestValidateConsolidationConflicts(t *testing.T) {
	sources := []contextstore.ContextItem{consolidationTestItem("a", "project-a", nil), consolidationTestItem("b", "project-a", nil)}
	cases := []struct {
		name    string
		lookup  *scopedConflictLookup
		wantErr string
	}{
		{name: "no conflicts", lookup: &scopedConflictLookup{}},
		{name: "conflicted source", lookup: &scopedConflictLookup{byScope: map[string]map[string][]string{"project-a/team": {"b": {"conflict-1"}}}}, wantErr: `source "b" has an unresolved memory conflict (conflict-1)`},
		{name: "lookup failure fails closed", lookup: &scopedConflictLookup{err: errors.New("table missing")}, wantErr: "table missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateConsolidationConflicts(context.Background(), tc.lookup, sources)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestConflictedClusterIDsChecksEachItemTeam(t *testing.T) {
	builder := newConsolidationClusterBuilder()
	for _, item := range []contextstore.ContextItem{
		{ID: "a", Scope: contextstore.Scope{ProjectID: "p", TeamID: "alpha"}},
		{ID: "b", Scope: contextstore.Scope{ProjectID: "p", TeamID: "alpha"}},
		{ID: "c", Scope: contextstore.Scope{ProjectID: "p", TeamID: "beta"}},
		{ID: "d", Scope: contextstore.Scope{ProjectID: "p", TeamID: "beta"}},
	} {
		builder.scopes[item.ID] = item.Scope
	}
	lookup := &scopedConflictLookup{byScope: map[string]map[string][]string{
		"p/alpha": {"b": {"conflict-alpha"}},
		"p/beta":  {"c": {"conflict-beta"}},
	}}
	got, err := conflictedClusterIDs(context.Background(), lookup, builder, [][]string{{"a", "b"}, {"c", "d"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"b", "c"}) {
		t.Fatalf("conflicted IDs = %v, want [b c]", got)
	}
	if len(lookup.calls) != 2 {
		t.Fatalf("lookup calls = %v, want one per team scope", lookup.calls)
	}
}

func TestConsolidateDryRunReportsConflictedIDs(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Same first six words, so both land in one deterministic cluster.
	for id, content := range map[string]string{"one": "run go test before every commit please", "two": "run go test before every commit always"} {
		if err = repo.Append(ctx, contextstore.ContextItem{ID: id, Kind: contextstore.ContextPattern, Content: content, Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}); err != nil {
			t.Fatal(err)
		}
	}
	one, _ := repo.Get(ctx, "one")
	two, _ := repo.Get(ctx, "two")
	j := contextstore.PairJudgment{ProjectID: "p", TeamID: "demo", ItemAID: one.ID, ItemBID: two.ID, ItemAContentHash: one.ContentHash, ItemBContentHash: two.ContentHash, Verdict: contextstore.PairVerdictContradicts, JudgePolicyVersion: contextstore.CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
	contextstore.NormalizePairJudgment(&j)
	if _, _, err = repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}

	contextWorkspace, contextProject, contextTeam, contextAgent, contextTier, contextLifecycle = workspace, "", "", "", "", ""
	contextQueryJSON, contextApplyProposal = false, false
	t.Cleanup(func() { contextWorkspace, contextQueryJSON = "", false })
	root := newRootCommand()
	out := new(bytes.Buffer)
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs([]string{"context", "consolidate", "--workspace", workspace, "--project", "p", "--team", "demo", "--json"})
	if err = root.Execute(); err != nil {
		t.Fatalf("consolidate dry-run: %v\n%s", err, out)
	}
	var got struct {
		Clusters      [][]string `json:"clusters"`
		ConflictedIDs []string   `json:"conflicted_ids"`
	}
	if err = json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", out, err)
	}
	if len(got.Clusters) != 1 || !reflect.DeepEqual(got.ConflictedIDs, []string{"one", "two"}) {
		t.Fatalf("dry-run = %+v", got)
	}
}
