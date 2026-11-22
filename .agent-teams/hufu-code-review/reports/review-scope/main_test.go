package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRunPresentsRequestedAndSelectedReviewScope(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		requested string
		resolved  string
		snapshot  string
		want      []string
	}{
		{"last_n", `"count":10,"commit_type":"feat"`, `"head":"new","selected_commit_count":2,"available_commit_count":2,"history_exhausted":true`, "null",
			[]string{"last 10 commits", "commit type `feat`", "`old..new`", "2 of 2 available", "Available history was exhausted"}},
		{"revision_range", `"base":"origin/main"`, `"head":"new","selected_commit_count":3,"available_commit_count":3`, "null",
			[]string{"revision range `origin/main..HEAD`", "`old..new`", "3 of 3 available"}},
		{"since", `"since":"2026-10-01"`, `"head":"new","selected_commit_count":3,"available_commit_count":3`, "null",
			[]string{"commits since `2026-10-01` ending at `HEAD`", "`old..new`"}},
		{"working_tree", `"count":0`, `"head":"WORKTREE","selected_commit_count":0`, "null",
			[]string{"working-tree changes against `HEAD`", "working-tree changes against `old`"}},
		{"repository_snapshot", `"batch":2`, `"head":"new"`, `{"batch":2,"batch_count":4,"file_count":50}`,
			[]string{"repository snapshot at `HEAD` (batch 2)", "snapshot at `new` (batch 2 of 4) across 50 tracked files"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			input := fmt.Sprintf(`{
				"version":1,"output":{
					"requested":{"kind":%q,"head":"HEAD","history":"first_parent",%s},
					"resolved":{"base":"old",%s},
					"repository_snapshot":%s,
					"observed_budget":{"changed_paths":7,"workset_items":2},
					"satisfied":true,"requested_input_hash":"sha256:verified"
				}
			}`, tc.kind, tc.requested, tc.resolved, tc.snapshot)
			var out bytes.Buffer
			if err := Run(t.Context(), strings.NewReader(input), &out); err != nil {
				t.Fatal(err)
			}
			var response struct {
				Markdown string `json:"markdown"`
			}
			if err := json.Unmarshal(out.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			for _, want := range append(tc.want, "7 across 2 workset items", "`sha256:verified`") {
				if !strings.Contains(response.Markdown, want) {
					t.Fatalf("report missing %q:\n%s", want, response.Markdown)
				}
			}
		})
	}
}
