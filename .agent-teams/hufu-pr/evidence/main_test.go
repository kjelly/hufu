package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == golangruntime.ChildArg {
		os.Exit(golangruntime.RunChild(os.Args[2], os.Args[3], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestEmbeddedPullRequestEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake GitHub CLI uses a Unix shell script")
	}
	bin, workspace := t.TempDir(), t.TempDir()
	fakeGH := `#!/bin/sh
case "$1" in
pr)
  printf '%s\n' '{"number":53,"url":"https://github.com/example/project/pull/53","baseRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","headRefOid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","changedFiles":1,"state":"OPEN","body":"Fixture","statusCheckRollup":[{"name":"CI","conclusion":"FAILURE","detailsUrl":"https://github.com/example/project/actions/runs/100/job/123"}]}'
  ;;
api)
  case "$*" in
    *graphql*) printf '%s\n' '{"data":{"repository":{"pullRequest":{"number":53,"baseRefOid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","headRefOid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","reviewThreads":{"totalCount":0,"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}' ;;
    *jobs/123/logs) printf '%s\n' 'FAIL: fixture-test' ;;
    *jobs/123) printf '%s\n' '{"id":123,"html_url":"https://github.com/example/project/actions/runs/100/job/123","name":"CI","conclusion":"failure"}' ;;
    */files*) printf '%s\n' '[[{"filename":"source.go","status":"added","additions":1,"patch":"@@ -0,0 +1 @@\n+package example"}]]' ;;
    *) exit 1 ;;
  esac
  ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGH), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HUFU_WORKSPACE", workspace)
	program, err := golangruntime.Prepare("..", "./evidence")
	if err != nil {
		t.Fatal(err)
	}
	for action, wanted := range map[string]string{
		operationReview: "+package example", operationSummary: "Author-provided description",
		operationCI: "FAIL: fixture-test", operationReadiness: "Review threads: 0 read out of 0",
	} {
		t.Run(action, func(t *testing.T) {
			payload := []byte(`{"type":"fetch_github_pr","payload":"{\"target\":{\"action\":\"ACTION\",\"repository\":\"example/project\",\"number\":53}}"}`)
			payload = []byte(strings.ReplaceAll(string(payload), "ACTION", action))
			child, err := golangruntime.Execute(t.Context(), "", program, payload, os.Environ(), 1<<20, 16<<10)
			if err != nil {
				t.Fatalf("execute embedded provider: %v: %s", err, child.Stderr)
			}
			var published result
			if err := json.Unmarshal(child.Stdout, &published); err != nil || len(published.Artifacts) != 2 {
				t.Fatalf("publication: %s, %v", child.Stdout, err)
			}
			evidence, manifest := published.Artifacts[0], published.Artifacts[1]
			raw, err := os.ReadFile(filepath.Join(workspace, manifest.Path))
			if err != nil || !strings.Contains(string(raw), evidence.ID) ||
				!strings.Contains(string(raw), `"required_reads":[`) {
				t.Fatalf("workset did not bind the evidence: %s, %v", raw, err)
			}
			data, err := os.ReadFile(filepath.Join(workspace, evidence.Path))
			if err != nil || !strings.Contains(string(data), wanted) {
				t.Fatalf("embedded adapter evidence lacks %q: %s, %v", wanted, data, err)
			}
		})
	}
}

func TestResolvePullRequestTarget(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, want string
	}{
		{"qualified target", `{"action":"review","repository":"kjelly/hufu","number":53}`, "matched"},
		{"ambiguous target", "null", "no_match"},
		{"write action", `{"action":"merge","repository":"kjelly/hufu","number":53}`, "invalid"},
		{"missing repository", `{"number":53}`, "invalid"},
		{"arbitrary host", `{"action":"review","repository":"evil.example/kjelly/hufu","number":53}`, "invalid"},
		{"path traversal", `{"action":"review","repository":"kjelly/..","number":53}`, "invalid"},
		{"flag-shaped owner", `{"action":"review","repository":"--help/hufu","number":53}`, "invalid"},
		{"invalid PR number", `{"action":"review","repository":"kjelly/hufu","number":0}`, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(request{InputName: "pr.target", ResolverID: "github-pr-v2", CandidateValue: json.RawMessage(tc.candidate)})
			if got.Status != tc.want || got.Status == "matched" && string(got.Value) != tc.candidate {
				t.Fatalf("resolve = %+v", got)
			}
		})
	}
}

func TestCompletePatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		file changedFile
		want bool
	}{
		{"complete hunk", changedFile{Additions: 1, Deletions: 1, Patch: "@@ -1,2 +1,2 @@\n-old\n+new\n context"}, true},
		{"truncated context", changedFile{Additions: 1, Deletions: 1, Patch: "@@ -1,2 +1,2 @@\n-old\n+new"}, false},
		{"rename without a patch", changedFile{Status: "renamed"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if completePatch(tc.file) != tc.want {
				t.Fatalf("completePatch = %t, want %t", !tc.want, tc.want)
			}
		})
	}
}

func TestFetchPullRequestEvidence(t *testing.T) {
	pr := target{Action: operationReview, Repository: "example/project", Number: 53}
	metadata := pullRequest{
		Number: 53, URL: "https://github.com/example/project/pull/53",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		ChangedFiles: 2, State: "OPEN",
		Checks: []githubCheck{{Name: "lint", Conclusion: "SUCCESS"}, {Name: "tests", Status: "IN_PROGRESS"}},
	}
	files := `[[{"filename":"a.go","status":"modified","additions":1,"deletions":1,"patch":"@@ -1 +1 @@\n-old\n+new"}],[{"filename":"truncated.go","status":"modified","additions":2,"deletions":0,"patch":"@@ -0,0 +1,2 @@\n+only one"}]]`
	for _, tc := range []struct {
		name, filePages, movedRef, wantErr string
	}{
		{name: "multiple pages and incomplete patch", filePages: files},
		{name: "head moved", filePages: files, movedRef: "head", wantErr: "changed during"},
		{name: "base moved", filePages: files, movedRef: "base", wantErr: "changed during"},
		{name: "incomplete inventory", filePages: `[[]]`, wantErr: "delivered 0"},
		{name: "duplicate path", filePages: `[[{"filename":"a.go"}],[{"filename":"a.go"}]]`, wantErr: "duplicate"},
		{name: "malformed pages", filePages: `{"files":[]}`, wantErr: "invalid PR file pages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			calls := 0
			runner := func(_ context.Context, dir string, args []string) ([]byte, error) {
				t.Helper()
				calls++
				if dir != workspace {
					t.Fatalf("command ran in %q, want action workspace", dir)
				}
				if calls == 2 {
					want := []string{"api", "--hostname", "github.com", "--method", "GET", "--paginate", "--slurp", "repos/example/project/pulls/53/files?per_page=100"}
					if !reflect.DeepEqual(args, want) {
						t.Fatalf("files command = %q, want %q", args, want)
					}
					return []byte(tc.filePages), nil
				}
				if calls > 3 || !reflect.DeepEqual(args[:5], []string{"pr", "view", "53", "--repo", "github.com/example/project"}) {
					t.Fatalf("unexpected metadata command %q", args)
				}
				observed := metadata
				if calls == 3 && tc.movedRef == "head" {
					observed.HeadSHA = strings.Repeat("c", 40)
				}
				if calls == 3 && tc.movedRef == "base" {
					observed.BaseSHA = strings.Repeat("c", 40)
				}
				return json.Marshal(observed)
			}
			got, err := fetchPR(t.Context(), pr, workspace, runner)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected %q, got %v", tc.wantErr, err)
				}
				entries, readErr := os.ReadDir(workspace)
				if readErr != nil || len(entries) != 0 {
					t.Fatalf("failed acquisition published evidence: %v, %v", entries, readErr)
				}
				return
			}
			if err != nil || calls != 3 || len(got.Artifacts) != 2 {
				t.Fatalf("fetch = %+v, %v, calls=%d", got, err, calls)
			}
			scope, ok := got.Outputs["scope"].(prScope)
			if !ok || scope.Requested != pr || scope.HeadSHA != metadata.HeadSHA ||
				!reflect.DeepEqual(scope.UnavailableFiles, []string{"truncated.go"}) ||
				scope.Checks != (checkCounts{Passed: 1, Pending: 1}) {
				t.Fatalf("incorrect frozen scope: %+v", scope)
			}
			ref := got.Artifacts[0]
			data, err := os.ReadFile(filepath.Join(workspace, ref.Path))
			sum := sha256.Sum256(data)
			if err != nil || ref.Kind != "github_pr_evidence" || ref.Bytes != len(data) ||
				ref.SHA256 != hex.EncodeToString(sum[:]) || !strings.Contains(string(data), "\n-old\n+new\n") ||
				strings.Contains(string(data), "+only one") {
				t.Fatalf("bad evidence artifact: %+v, %v\n%s", ref, err, data)
			}
		})
	}
}

func TestReviewPublishesEvidenceWhenPatchExceedsBudget(t *testing.T) {
	pr := target{Action: operationReview, Repository: "example/project", Number: 53}
	metadata := pullRequest{
		Number: 53, URL: "https://github.com/example/project/pull/53",
		BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), ChangedFiles: 3, State: "OPEN",
		Body:   strings.Repeat("description\n", maxDescriptionBytes/12+1),
		Checks: []githubCheck{{Status: "COMPLETED", Conclusion: "SUCCESS", Name: strings.Repeat("x", maxCheckDetail)}},
	}
	files := [][]changedFile{{
		{Path: "big.json", Status: "added", Additions: 1, Patch: "@@ -0,0 +1 @@\n+" + strings.Repeat("large", maxEvidenceBytes/5)},
		{Path: "small.go", Status: "added", Additions: 1, Patch: "@@ -0,0 +1 @@\n+package example"},
		{Path: "binary.dat", Status: "modified"},
	}}
	run := func(_ context.Context, _ string, args []string) ([]byte, error) {
		if args[0] == "api" {
			return json.Marshal(files)
		}
		return json.Marshal(metadata)
	}
	workspace := t.TempDir()
	got, err := fetchPR(t.Context(), pr, workspace, run)
	if err != nil {
		t.Fatal(err)
	}
	scope := got.Outputs["scope"].(prScope)
	if !reflect.DeepEqual(scope.UnavailableFiles, []string{"big.json", "binary.dat"}) || len(scope.Gaps) != 4 || scope.Checks != (checkCounts{Passed: 1}) {
		t.Fatalf("unexpected evidence gaps: %+v", scope)
	}
	data, err := os.ReadFile(filepath.Join(workspace, got.Artifacts[0].Path))
	if err != nil || len(data) > maxEvidenceBytes || !strings.Contains(string(data), `File "big.json"`) ||
		!strings.Contains(string(data), `File "binary.dat"`) || !strings.Contains(string(data), "+package example") ||
		!strings.Contains(string(data), "Recorded GitHub checks: 1 passed, 0 failed, 0 pending, 0 other") ||
		strings.Contains(string(data), "largelargelarge") {
		t.Fatalf("bounded review evidence = %d bytes, read error = %v", len(data), err)
	}
}
