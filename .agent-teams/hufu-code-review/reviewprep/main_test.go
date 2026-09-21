package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestRunAcceptsCanonicalPrepareReviewWorksetAction(t *testing.T) {
	repo := newFixtureRepo(t)
	for i := 1; i <= 10; i++ {
		writeAndCommit(t, repo, filepath.Join("internal", "team", fmt.Sprintf("runtime-%02d.go", i)), "package team\n", fmt.Sprintf("runtime change %02d", i), fmt.Sprintf("2025-01-%02dT00:00:00Z", i+1))
	}
	config := fixtureConfig(repo, "out")
	requestScope := resolverScope{Kind: "last_n", Count: config.MaxCommits, History: "first_parent", Head: "HEAD"}
	payload, err := json.Marshal(wireConfig{
		Repository: config.Repository, OutputDir: config.OutputDir, ArtifactRoot: config.ArtifactRoot,
		Scope: &requestScope, MaxDiffBytes: config.MaxDiffBytes,
		MaxDiffLines: config.MaxDiffLines, MaxPaths: config.MaxPaths,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(actionRequest{Type: "prepare_review_workset", Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(t.Context(), bytes.NewReader(request), &output); err != nil {
		t.Fatalf("run canonical action: %v", err)
	}
	var result actionResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode action result: %v", err)
	}
	if result.Outputs["manifest_path"] == nil {
		t.Fatalf("action result omitted manifest_path: %#v", result)
	}
	scopeJSON, err := json.Marshal(result.Outputs["scope"])
	if err != nil {
		t.Fatal(err)
	}
	var scope scopeAttestation
	if err := json.Unmarshal(scopeJSON, &scope); err != nil {
		t.Fatal(err)
	}
	if scope.Requested.Count != 10 || scope.Resolved.SelectedCommitCount != 10 || !scope.Satisfied {
		t.Fatalf("canonical action scope = %#v, want exactly 10 selected commits", scope)
	}
}

func TestEmbeddedRuntimeExecutesDocumentationRouting(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "See the [runtime documentation](docs/architecture/runtime.md).\n")
	writeFile(t, filepath.Join(repo, "docs/architecture/runtime.md"), "# Runtime\n\n`RuntimeContract` is defined in [`runtime.go`](../../internal/team/runtime.go).\n")
	writeFile(t, filepath.Join(repo, "internal/team/runtime.go"), "package team\n\ntype RuntimeContract struct{}\n")
	commit(t, repo, "mixed runtime and documentation changes", "2025-01-02T00:00:00Z")

	scope := resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: "HEAD"}
	payload, err := json.Marshal(wireConfig{
		Scope: &scope, Routing: routingDocumentation,
		MaxTotalDiffBytes: defaultMaxTotalDiffBytes, MaxTotalDiffLines: defaultMaxTotalDiffLines,
		MaxChangedPaths: defaultMaxChangedPaths, MaxWorksetItems: defaultMaxWorksetItems,
		MaxDiffBytes: 24_000, MaxDiffLines: 600, MaxPaths: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := json.Marshal(actionRequest{Type: "prepare_review_workset", Payload: string(payload)})
	if err != nil {
		t.Fatal(err)
	}

	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	env := append(os.Environ(), "HUFU_REPOSITORY="+repo, "HUFU_WORKSPACE="+workspace)
	result, err := golangruntime.Execute(t.Context(), "", program, request, env, 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("execute embedded reviewprep runtime: %v; stderr=%s", err, result.Stderr)
	}

	var actionOutput actionResult
	if err := json.Unmarshal(result.Stdout, &actionOutput); err != nil {
		t.Fatalf("decode embedded runtime output: %v; stdout=%s", err, result.Stdout)
	}
	if actionOutput.Outputs["primary_manifest_path"] == nil ||
		actionOutput.Outputs["documentation_manifest_path"] == nil ||
		actionOutput.Outputs["documentation-escalation_manifest_path"] == nil {
		t.Fatalf("embedded runtime omitted routed manifests: %#v", actionOutput.Outputs)
	}
	if len(actionOutput.Artifacts) == 0 {
		t.Fatal("embedded runtime produced no artifacts")
	}
}

func TestPrepareSupportsTypedReviewScopeVariants(t *testing.T) {
	repo := newFixtureRepo(t)
	for index := 1; index <= 4; index++ {
		writeAndCommit(t, repo, filepath.Join("internal", fmt.Sprintf("scope-%d.go", index)), "package internal\n", fmt.Sprintf("scope %d", index), fmt.Sprintf("2025-01-0%dT00:00:00Z", index+1))
	}
	tests := []struct {
		name      string
		scope     resolverScope
		wantCount int
	}{
		{name: "last_n", scope: resolverScope{Kind: "last_n", Count: 3, History: "first_parent", Head: "HEAD"}, wantCount: 3},
		{name: "revision_range", scope: resolverScope{Kind: "revision_range", History: "first_parent", Base: "HEAD~2", Head: "HEAD"}, wantCount: 2},
		{name: "since", scope: resolverScope{Kind: "since", History: "first_parent", Head: "HEAD", Since: "2025-01-04"}, wantCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := fixtureConfig(repo, "out-"+test.name)
			config.Scope = test.scope
			config.Since = ""
			result, err := Prepare(t.Context(), config)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			manifest := readManifest(t, filepath.Join(repo, "out-"+test.name, "workset-manifest.json"))
			if manifest.Scope.Requested != test.scope || manifest.Scope.Resolved.SelectedCommitCount != test.wantCount || !manifest.Scope.Satisfied {
				t.Fatalf("scope = %#v, want requested %#v and count %d", manifest.Scope, test.scope, test.wantCount)
			}
			if manifest.Scope.ObservedBudget != manifest.Observed {
				t.Fatalf("scope observed budget = %#v, manifest observed = %#v", manifest.Scope.ObservedBudget, manifest.Observed)
			}
			if result.Outputs["scope"] != manifest.Scope {
				t.Fatalf("runtime output scope = %#v, manifest scope = %#v", result.Outputs["scope"], manifest.Scope)
			}
		})
	}
}

func TestResolveReviewScopeInputDoesNotInferNaturalLanguage(t *testing.T) {
	for _, prompt := range []string{
		"Review the last 5 commits",
		"審查最近5個的 git commit",
		"Review the current git diff",
		"審查目前未提交的修改",
	} {
		response := resolveReviewScopeInput(runInputResolverRequest{
			Type: "resolve_run_input", InputName: "review.scope", ResolverID: "review-scope-v1", Prompt: prompt,
		})
		if response.Status != "no_match" || len(response.Value) != 0 || len(response.Evidence) != 0 {
			t.Fatalf("deterministic fallback inferred prompt %q: %#v", prompt, response)
		}
	}
}

func TestRunAcceptsStrictResolverEnvelope(t *testing.T) {
	request := runInputResolverRequest{
		Type: "resolve_run_input", InputName: "review.scope", Prompt: "Review last 7 commits",
		ExplicitValue: json.RawMessage(`null`), SchemaHash: "sha256:fixture", ResolverID: "review-scope-v1",
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run(t.Context(), bytes.NewReader(encoded), &output); err != nil {
		t.Fatal(err)
	}
	var response runInputResolverResponse
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "no_match" || len(response.Value) != 0 {
		t.Fatalf("response = %#v", response)
	}
	bad := append(encoded[:len(encoded)-1], []byte(`,"unknown":true}`)...)
	if err := run(t.Context(), bytes.NewReader(bad), &bytes.Buffer{}); err == nil {
		t.Fatal("resolver accepted an unknown request field")
	}
}

func TestResolveReviewScopeInputRejectsInvalidExplicitScope(t *testing.T) {
	response := resolveReviewScopeInput(runInputResolverRequest{
		Type: "resolve_run_input", InputName: "review.scope", ResolverID: "review-scope-v1",
		ExplicitValue: json.RawMessage(`{"kind":"revision_range","history":"first_parent","head":"HEAD"}`),
	})
	if response.Status != "invalid" || !strings.Contains(response.Diagnostic, "requires a valid base") {
		t.Fatalf("response = %#v", response)
	}
}

func TestDecodeWireConfigRejectsInvalidMaxCommitsAndJSON(t *testing.T) {
	for _, value := range []string{"0", "-1", "10.0", "text", `10\",\"unexpected\":true`, "101", "999999999999999999999999999999999999"} {
		t.Run(value, func(t *testing.T) {
			payload, err := json.Marshal(wireConfig{MaxCommits: value})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeWireConfig(string(payload)); err == nil {
				t.Fatalf("decodeWireConfig accepted max_commits %q", value)
			}
		})
	}
	if _, err := decodeWireConfig(`{"max_commits":"10","unexpected":true}`); err == nil {
		t.Fatal("decodeWireConfig accepted an unknown field")
	}
	if _, err := decodeWireConfig(`{"max_commits":"10"} {"max_commits":"3"}`); err == nil {
		t.Fatal("decodeWireConfig accepted trailing JSON")
	}
}

func TestReviewerPromptDefersToRuntimeResultProtocol(t *testing.T) {
	prompt, err := os.ReadFile(filepath.Join("..", "reviewer.md"))
	if err != nil {
		t.Fatalf("read reviewer prompt: %v", err)
	}
	text := strings.Join(strings.Fields(string(prompt)), " ")
	for _, required := range []string{
		"runtime-provided result protocol",
		"active runtime schema",
		"`files_read` using exactly the representation required by the active schema",
		"cite the supplied opaque artifact identifier exactly",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("reviewer prompt omitted %q", required)
		}
	}
	for _, forbidden := range []string{
		"`submit_result`",
		"WorkerResultProposal",
		"local tool",
		"external structured result provider",
		"`files_read` object",
		"array of non-empty strings",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("reviewer prompt retained transport-specific instruction %q", forbidden)
		}
	}
}

func TestPrepareProducesGoldenManifestAndDiffs(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "cmd/hufu/main.go", "package main\n\nfunc main() {}\n", "boundary change", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, "internal/team/runtime.go", "package team\n\nfunc Run() {}\n", "runtime change", "2025-01-03T00:00:00Z")
	writeAndCommit(t, repo, "internal/tools/bash.go", "package tools\n\nfunc Bash() {}\n", "security change", "2025-01-04T00:00:00Z")

	result, err := Prepare(context.Background(), fixtureConfig(repo, "out"))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got, want := result.Outputs["item_count"], 3; got != want {
		t.Fatalf("item_count = %v, want %d", got, want)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.Scope.Requested != (requestedScope{Kind: "last_n", Count: 10, History: "first_parent", Head: "HEAD"}) {
		t.Fatalf("requested scope = %#v", manifest.Scope.Requested)
	}
	if manifest.Scope.Resolved.SelectedCommitCount != 3 || manifest.Scope.Resolved.AvailableCommitCount != 3 || !manifest.Scope.Resolved.HistoryExhausted || manifest.Scope.Resolved.RepositoryShallow || !manifest.Scope.Satisfied {
		t.Fatalf("resolved scope = %#v", manifest.Scope)
	}
	if got, want := manifest.Scope.RequestedInputHash, scopeInputDigest(manifest.Scope.Requested); got != want || !strings.HasPrefix(got, "sha256:") {
		t.Fatalf("input digest = %q, want %q", got, want)
	}
	outputScope, ok := result.Outputs["scope"].(scopeAttestation)
	if !ok || outputScope != manifest.Scope {
		t.Fatalf("runtime scope = %#v, want manifest scope %#v", result.Outputs["scope"], manifest.Scope)
	}
	if result.Outputs["total_diff_bytes"] != manifest.Observed.TotalDiffBytes || result.Outputs["total_diff_lines"] != manifest.Observed.TotalDiffLines {
		t.Fatalf("runtime totals = %#v, want manifest totals %#v", result.Outputs, manifest.Observed)
	}
	got := goldenSummary{SchemaVersion: manifest.SchemaVersion, CommitCount: manifest.Range.CommitCount, ChangedFiles: manifest.ChangedFiles}
	for _, entry := range manifest.Items {
		if len(entry.Inputs) != 1 || !strings.HasPrefix(entry.Inputs[0].ID, "sha256-") || entry.Inputs[0].Description != "bounded workset diff" {
			t.Fatalf("item %s lacks opaque diff input artifact: %#v", entry.Key, entry.Inputs)
		}
		got.Items = append(got.Items, goldenItem{Key: entry.Key, Lens: entry.Lens, Paths: entry.TouchedPaths, DiffPath: entry.DiffPath})
		patch, err := os.ReadFile(filepath.Join(repo, "out", filepath.FromSlash(entry.DiffPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(patch), "diff --git") {
			t.Fatalf("diff %s does not contain a Git patch", entry.DiffPath)
		}
	}
	want := readGoldenSummary(t)
	if string(mustJSON(t, got)) != string(mustJSON(t, want)) {
		t.Fatalf("golden summary mismatch\n got: %s\nwant: %s", mustJSON(t, got), mustJSON(t, want))
	}
}

func TestClassifyReviewPathRoutesRoutineAndRiskDocumentation(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "ordinary readme\n")
	writeFile(t, filepath.Join(repo, "docs/README.md"), "documentation index\n")
	writeFile(t, filepath.Join(repo, "docs/tutorials/start.md"), "tutorial\n")
	writeFile(t, filepath.Join(repo, "docs/releases/v1.md"), "release notes\n")
	writeFile(t, filepath.Join(repo, "docs/architecture/runtime.md"), "> Authority: reference\n")
	writeFile(t, filepath.Join(repo, "docs/reference/normative.md"), "> Authority: normative\n")
	writeFile(t, filepath.Join(repo, "docs/security-guide.md"), "security\n")
	writeFile(t, filepath.Join(repo, "internal/team/runtime.go"), "package team\n")
	commit(t, repo, "routing fixtures", "2025-01-02T00:00:00Z")

	tests := []struct {
		path string
		want string
	}{
		{path: "README.md", want: "documentation"},
		{path: "docs/README.md", want: "documentation"},
		{path: "docs/tutorials/start.md", want: "documentation"},
		{path: "docs/releases/v1.md", want: "documentation"},
		{path: "docs/architecture/runtime.md", want: "documentation-risk"},
		{path: "docs/reference/normative.md", want: "documentation-risk"},
		{path: "docs/security-guide.md", want: "documentation-risk"},
		{path: "internal/team/runtime.go", want: "primary"},
	}
	reviewedRange := reviewRange{End: "HEAD"}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := classifyReviewPath(t.Context(), repo, reviewedRange, test.path); got != test.want {
				t.Fatalf("classifyReviewPath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestRepositoryCodePathRequiresExplicitPathSyntax(t *testing.T) {
	tests := []struct {
		name   string
		doc    string
		token  string
		want   string
		wantOK bool
	}{
		{name: "bare yaml filename", doc: "README.md", token: "team.yaml"},
		{name: "bare go test filename", doc: "docs/reference/action-providers.md", token: "main_test.go"},
		{name: "bare special filename", doc: "README.md", token: "go.mod"},
		{name: "explicit root relative path", doc: "README.md", token: "./team.yaml", want: "team.yaml", wantOK: true},
		{name: "nested path", doc: "README.md", token: "docs/reference/action-providers.md", want: "docs/reference/action-providers.md", wantOK: true},
		{name: "document relative path", doc: "docs/tutorials/start.md", token: "../reference/action-providers.md", want: "docs/reference/action-providers.md", wantOK: true},
		{name: "go path with line", doc: "README.md", token: "internal/team/runtime.go:42", want: "internal/team/runtime.go", wantOK: true},
		{name: "uri", doc: "README.md", token: "https://example.com/reference.md"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := repositoryCodePath(test.doc, test.token)
			if ok != test.wantOK || got != test.want {
				t.Fatalf("repositoryCodePath(%q, %q) = %q, %t; want %q, %t", test.doc, test.token, got, ok, test.want, test.wantOK)
			}
		})
	}
}

func TestPrepareDocumentationRoutingProducesVerifiedWorksets(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "See the [tutorial](docs/tutorials/start.md).\n")
	writeFile(t, filepath.Join(repo, "docs/tutorials/start.md"), "# Start\n")
	writeFile(t, filepath.Join(repo, "docs/architecture/execution.md"), "`TaskExecutionEnvelope` is defined in [`runtime.go`](../../internal/team/runtime.go). The `crypto/rand` import, `internal/operator.ResolveWorkspacePath()`, `team.yaml`, and `main_test.go` references are prose, not repository file paths.\n")
	writeFile(t, filepath.Join(repo, "internal/team/runtime.go"), "package team\n\ntype TaskExecutionEnvelope struct{}\n")
	commit(t, repo, "mixed documentation", "2025-01-02T00:00:00Z")

	config := fixtureConfig(repo, "out")
	config.Routing = routingDocumentation
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatalf("Prepare routed worksets: %v", err)
	}
	verification, ok := result.Outputs["documentation_verification"].(documentationVerification)
	if !ok || !verification.Passed || verification.CheckedLinks != 2 || verification.CheckedPaths != 0 || verification.CheckedSymbols != 1 {
		t.Fatalf("documentation verification = %#v", result.Outputs["documentation_verification"])
	}

	primary := readManifest(t, filepath.Join(repo, "out", "primary", "workset-manifest.json"))
	documentation := readManifest(t, filepath.Join(repo, "out", "documentation", "workset-manifest.json"))
	escalation := readManifest(t, filepath.Join(repo, "out", "documentation-escalation", "workset-manifest.json"))
	assertManifestPaths(t, primary, []string{"docs/architecture/execution.md", "internal/team/runtime.go"})
	assertManifestPaths(t, documentation, []string{"README.md", "docs/tutorials/start.md"})
	assertManifestPaths(t, escalation, []string{"docs/architecture/execution.md"})
	if escalation.Items[0].Lens != "documentation-risk" || len(escalation.Items[0].Inputs) != 2 {
		t.Fatalf("escalation item = %#v, want risk lens plus diff and verifier inputs", escalation.Items[0])
	}
	for route, value := range map[string]manifest{"primary": primary, "documentation": documentation, "documentation-escalation": escalation} {
		for _, entry := range value.Items {
			if entry.Bindings["route"] != route {
				t.Fatalf("%s item route binding = %q", route, entry.Bindings["route"])
			}
			patch, err := os.ReadFile(filepath.Join(repo, "out", route, filepath.FromSlash(entry.DiffPath)))
			if err != nil {
				t.Fatal(err)
			}
			header := "# hufu-review-route: " + route + "\n"
			if !bytes.HasPrefix(patch, []byte(header+"diff --git ")) || bytes.Count(patch, []byte(header)) != 1 {
				t.Fatalf("%s diff header corrupted the Git patch: %q", route, patch[:min(len(patch), 160)])
			}
		}
	}

	descriptions := make(map[string]struct{})
	artifactIDs := make(map[string]string)
	for _, ref := range result.Artifacts {
		descriptions[ref.Description] = struct{}{}
		if previous, duplicate := artifactIDs[ref.ID]; duplicate {
			t.Fatalf("routed artifacts %q and %q share content ID %q", previous, ref.Path, ref.ID)
		}
		artifactIDs[ref.ID] = ref.Path
	}
	for _, description := range []string{"primary review workset manifest", "documentation review workset manifest", "documentation escalation workset manifest", "documentation verification report"} {
		if _, ok := descriptions[description]; !ok {
			t.Errorf("result omitted artifact %q", description)
		}
	}
}

func TestPrepareDocumentationRoutingEmitsNoopItemsForEmptyPartitions(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "README.md", "routine documentation only\n", "readme", "2025-01-02T00:00:00Z")
	config := fixtureConfig(repo, "out")
	config.Routing = routingDocumentation
	if _, err := Prepare(t.Context(), config); err != nil {
		t.Fatalf("Prepare routed worksets: %v", err)
	}
	for _, route := range []string{"primary", "documentation-escalation"} {
		value := readManifest(t, filepath.Join(repo, "out", route, "workset-manifest.json"))
		if len(value.Items) != 1 || value.Items[0].Lens != "noop" || len(value.Items[0].TouchedPaths) != 0 {
			t.Fatalf("%s manifest = %#v, want one no-op item", route, value.Items)
		}
	}
}

func TestPrepareDocumentationVerificationFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "missing link", content: "See [missing](missing.md).\n", want: "relative link target"},
		{name: "missing explicit root path", content: "Update `./missing-config.yaml` first.\n", want: "repository path"},
		{name: "missing relative path", content: "Read `../reference/missing.md` first.\n", want: "repository path"},
		{name: "missing symbol", content: "The `MissingRuntimeContract` is required.\n", want: "Go symbol"},
		{name: "noncanonical resource", content: "Use `workspace:path/...` claims.\n", want: "canonical workspace resource syntax"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := newFixtureRepo(t)
			writeAndCommit(t, repo, "docs/tutorials/start.md", test.content, test.name, "2025-01-02T00:00:00Z")
			config := fixtureConfig(repo, "out")
			config.Routing = routingDocumentation
			_, err := Prepare(t.Context(), config)
			if err == nil || !strings.Contains(err.Error(), "documentation_verification_failed") || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Prepare error = %v, want deterministic %q failure", err, test.want)
			}
			if _, statErr := os.Stat(filepath.Join(repo, "out")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed verification published output: %v", statErr)
			}
		})
	}
}

func assertManifestPaths(t *testing.T, value manifest, want []string) {
	t.Helper()
	got := make([]string, 0)
	for _, entry := range value.Items {
		got = append(got, entry.TouchedPaths...)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("manifest paths = %v, want %v", got, want)
	}
}

func TestPrepareLimitsRangeToMostRecentCommits(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/old.go", "package old\n", "old change", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, "internal/new.go", "package new\n", "new change", "2025-01-03T00:00:00Z")
	writeAndCommit(t, repo, "internal/latest.go", "package latest\n", "latest change", "2025-01-04T00:00:00Z")
	config := fixtureConfig(repo, "out")
	config.MaxCommits = 2

	result, err := Prepare(context.Background(), config)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.Range.CommitCount != 2 || result.Outputs["commit_count"] != 2 {
		t.Fatalf("commit count = %d / %v, want 2", manifest.Range.CommitCount, result.Outputs["commit_count"])
	}
	for _, entry := range manifest.Items {
		for _, path := range entry.TouchedPaths {
			if path == "internal/old.go" {
				t.Fatalf("oldest commit path included in limited range: %#v", manifest.Items)
			}
		}
	}
}

func TestResolveRangeCharacterizesFirstParentCardinality(t *testing.T) {
	repo := newFixtureRepo(t)
	for i := 1; i <= 10; i++ {
		writeAndCommit(t, repo, filepath.Join("internal", "history", fmt.Sprintf("commit-%02d.go", i)), "package history\n", fmt.Sprintf("change %02d", i), fmt.Sprintf("2025-01-%02dT00:00:00Z", i+1))
	}

	for _, count := range []int{1, 3, 10} {
		t.Run(fmt.Sprintf("last_%d", count), func(t *testing.T) {
			resolution, err := resolveRange(t.Context(), repo, "2025-01-01T12:00:00Z", count)
			if err != nil {
				t.Fatalf("resolveRange(%d): %v", count, err)
			}
			if resolution.Range.CommitCount != count {
				t.Fatalf("CommitCount = %d, want %d", resolution.Range.CommitCount, count)
			}
			if resolution.AvailableCommitCount != 10 || resolution.HistoryExhausted {
				t.Fatalf("resolution = %#v, want 10 available and non-exhausted", resolution)
			}
		})
	}
}

func TestResolveRangeCharacterizesExhaustedHistory(t *testing.T) {
	repo := newFixtureRepo(t)
	for i := 1; i <= 3; i++ {
		writeAndCommit(t, repo, filepath.Join("internal", fmt.Sprintf("commit-%02d.go", i)), "package internal\n", fmt.Sprintf("change %02d", i), fmt.Sprintf("2025-01-%02dT00:00:00Z", i+1))
	}

	resolution, err := resolveRange(t.Context(), repo, "2025-01-01T12:00:00Z", 10)
	if err != nil {
		t.Fatalf("resolveRange: %v", err)
	}
	if resolution.Range.CommitCount != 3 || resolution.AvailableCommitCount != 3 || !resolution.HistoryExhausted {
		t.Fatalf("resolution = %#v, want all 3 available commits and exhausted history", resolution)
	}
}

func TestResolveRangeIncludesRootCommitWhenHistoryIsExhausted(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/one.go", "package internal\n", "one", "2025-01-02T00:00:00Z")
	resolution, err := resolveRange(t.Context(), repo, "1970-01-01", 10)
	if err != nil {
		t.Fatalf("resolveRange including root: %v", err)
	}
	if resolution.Range.CommitCount != 2 || resolution.AvailableCommitCount != 2 || !resolution.HistoryExhausted {
		t.Fatalf("resolution = %#v, want root plus one commit and exhausted history", resolution)
	}
	paths, err := changedPaths(t.Context(), repo, resolution.Range)
	if err != nil {
		t.Fatalf("changedPaths from empty tree: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("changed paths = %#v, want root and later paths", paths)
	}
}

func TestResolveRangeCharacterizesShallowHistoryFailure(t *testing.T) {
	source := newFixtureRepo(t)
	for i := 1; i <= 3; i++ {
		writeAndCommit(t, source, filepath.Join("internal", fmt.Sprintf("commit-%02d.go", i)), "package internal\n", fmt.Sprintf("change %02d", i), fmt.Sprintf("2025-01-%02dT00:00:00Z", i+1))
	}
	cloneRoot := t.TempDir()
	shallow := filepath.Join(cloneRoot, "shallow")
	cmd := exec.Command("git", "clone", "--depth", "2", "file://"+filepath.ToSlash(source), shallow)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git clone --depth 2: %v\n%s", err, output)
	}

	if _, err := resolveRange(t.Context(), shallow, "1970-01-01", 10); err == nil {
		t.Fatal("resolveRange on incomplete shallow history succeeded; current behavior must fail closed")
	}
}

func TestPrepareLimitsRangeAcrossMergeHistory(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/base.go", "package base\n", "base change", "2025-01-02T00:00:00Z")
	gitRun(t, repo, "checkout", "-b", "side")
	writeAndCommit(t, repo, "internal/side.go", "package side\n", "side change", "2025-01-03T00:00:00Z")
	gitRun(t, repo, "checkout", "main")
	writeAndCommit(t, repo, "internal/main.go", "package main\n", "main change", "2025-01-04T00:00:00Z")

	merge := exec.Command("git", "-c", "user.name=Review Prep Test", "-c", "user.email=reviewprep@example.test", "merge", "--no-ff", "side", "-m", "merge side")
	merge.Dir = repo
	merge.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2025-01-05T00:00:00Z", "GIT_COMMITTER_DATE=2025-01-05T00:00:00Z")
	if output, err := merge.CombinedOutput(); err != nil {
		t.Fatalf("git merge: %v\n%s", err, output)
	}

	config := fixtureConfig(repo, "out")
	config.MaxCommits = 2
	if _, err := Prepare(context.Background(), config); err != nil {
		t.Fatalf("Prepare() across merge history: %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.Range.CommitCount != 2 {
		t.Fatalf("commit count = %d, want 2", manifest.Range.CommitCount)
	}
}

func TestPrepareDoesNotIncludeDirtyWorkingTree(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/committed.go", "package team\n", "committed change", "2025-01-02T00:00:00Z")
	writeFile(t, filepath.Join(repo, "internal/team/uncommitted.go"), "package team\n")

	if _, err := Prepare(context.Background(), fixtureConfig(repo, "out")); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.ChangedFiles != 1 || manifest.Items[0].TouchedPaths[0] != "internal/team/committed.go" {
		t.Fatalf("manifest included dirty working tree: %#v", manifest)
	}
}

func TestGenericWorksetShadowProjectionPreservesKeysDigestsAndOrdering(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/one.go", "package team\n\nfunc One() {}\n", "one", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, "internal/tools/two.go", "package tools\n\nfunc Two() {}\n", "two", "2025-01-03T00:00:00Z")
	if _, err := Prepare(context.Background(), fixtureConfig(repo, "out")); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))

	// Phase B's shadow comparison is deliberately an adapter characterization:
	// it compares the old row-shaped view with the normalized generic view,
	// without making either presentation a runtime acceptance input.
	type shadowRow struct {
		Key    string
		Lens   string
		Digest string
	}
	rows := make([]shadowRow, 0, len(manifest.Items))
	for _, entry := range manifest.Items {
		if len(entry.Inputs) != 1 {
			t.Fatalf("item %s has %d inputs, want one", entry.Key, len(entry.Inputs))
		}
		rows = append(rows, shadowRow{Key: entry.Key, Lens: entry.Lens, Digest: entry.Inputs[0].SHA256})
	}
	if len(rows) != len(manifest.Items) {
		t.Fatalf("shadow item count = %d, generic count = %d", len(rows), len(manifest.Items))
	}
	for index, row := range rows {
		entry := manifest.Items[index]
		if row.Key != entry.Key || row.Lens != entry.Bindings["lens"] || row.Digest != entry.Inputs[0].SHA256 {
			t.Fatalf("shadow mismatch at %d: row=%#v generic=%#v", index, row, entry)
		}
	}
}

func TestPrepareArtifactRootMatchesRuntimeWorkspace(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/runtime.go", "package team\n\nfunc Run() {}\n", "runtime change", "2025-01-02T00:00:00Z")
	config := fixtureConfig(repo, "workspace/hufu-code-review/workset")
	config.ArtifactRoot = "workspace"
	result, err := Prepare(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath, ok := result.Outputs["manifest_path"].(string)
	if !ok || manifestPath != "hufu-code-review/workset/workset-manifest.json" {
		t.Fatalf("manifest_path = %v, want runtime-workspace-relative path", result.Outputs["manifest_path"])
	}
	manifest := readManifest(t, filepath.Join(repo, "workspace", filepath.FromSlash(manifestPath)))
	if len(manifest.Items) != 1 || len(manifest.Items[0].Inputs) != 1 {
		t.Fatalf("manifest items = %#v, want one opaque input", manifest.Items)
	}
	if got := manifest.Items[0].Inputs[0].Path; got != "hufu-code-review/workset/batches/unit-0000/diff.patch" {
		t.Fatalf("input artifact path = %q, want path relative to runtime workspace", got)
	}
}

func TestPrepareRejectsEmptyRangeWithoutPublishingManifest(t *testing.T) {
	repo := newFixtureRepo(t)
	outputDir := filepath.Join(repo, "review-output")
	_, err := Prepare(context.Background(), Config{
		Repository:   repo,
		OutputDir:    outputDir,
		Since:        "2025-01-01T12:00:00Z",
		MaxDiffBytes: 1024,
		MaxDiffLines: 100,
		MaxPaths:     5,
	})
	if err == nil || !strings.Contains(err.Error(), "scope_empty") {
		t.Fatalf("Prepare() error = %v, want scope_empty", err)
	}
	if _, statErr := os.Stat(outputDir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("empty range published output directory: %v", statErr)
	}
}

func TestPrepareSplitsWholeHunks(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "internal/team/hunks.go"), "package team\n\nfunc One() int {\n\treturn 1\n}\n\nfunc Two() int {\n\treturn 2\n}\n")
	commit(t, repo, "base hunks", "2025-01-01T00:00:00Z")
	writeFile(t, filepath.Join(repo, "internal/team/hunks.go"), "package team\n\nfunc One() int {\n\treturn 10\n}\n\nfunc Two() int {\n\treturn 20\n}\n")
	commit(t, repo, "change two hunks", "2025-01-02T00:00:00Z")

	config := fixtureConfig(repo, "out")
	config.MaxDiffBytes = 1
	config.MaxDiffLines = 1
	result, err := Prepare(context.Background(), config)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got := result.Outputs["item_count"]; got != 2 {
		t.Fatalf("item_count = %v, want 2", got)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	var patches string
	for _, entry := range manifest.Items {
		patch, err := os.ReadFile(filepath.Join(repo, "out", filepath.FromSlash(entry.DiffPath)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(patch), "@@") {
			t.Fatalf("split patch %q omitted its hunk header", entry.DiffPath)
		}
		patches += string(patch)
	}
	for _, want := range []string{"+\treturn 10", "+\treturn 20"} {
		if !strings.Contains(patches, want) {
			t.Fatalf("split patches omitted %q", want)
		}
	}
}

func TestPrepareHandlesDeletedAndBinaryPaths(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "internal/team/deleted.go"), "package team\n")
	writeFile(t, filepath.Join(repo, "asset.bin"), string([]byte{0, 1, 2, 3}))
	commit(t, repo, "base artifacts", "2025-01-01T00:00:00Z")
	if err := os.Remove(filepath.Join(repo, "internal/team/deleted.go")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "asset.bin"), string([]byte{0, 4, 5, 6}))
	commit(t, repo, "delete and update binary", "2025-01-02T00:00:00Z")

	if _, err := Prepare(context.Background(), fixtureConfig(repo, "out")); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.ChangedFiles != 2 {
		t.Fatalf("changed_files = %d, want 2", manifest.ChangedFiles)
	}
}

func TestPrepareRejectsInvalidConfigAndNonEmptyOutput(t *testing.T) {
	repo := newFixtureRepo(t)
	config := fixtureConfig(repo, "out")
	config.MaxPaths = 0
	if _, err := Prepare(context.Background(), config); err == nil {
		t.Fatal("Prepare() succeeded with invalid limits")
	}
	if err := os.MkdirAll(filepath.Join(repo, "out"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "out", "previous-run"), "keep")
	if _, err := Prepare(context.Background(), fixtureConfig(repo, "out")); err == nil {
		t.Fatal("Prepare() replaced a non-empty output directory")
	}
}

func TestPrepareRejectsEveryTotalCapWithoutPublishingOutput(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/one.go", "package team\n\nfunc One() int { return 1 }\n", "one", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, "internal/team/two.go", "package team\n\nfunc Two() int { return 2 }\n", "two", "2025-01-03T00:00:00Z")

	tests := []struct {
		name string
		edit func(*Config)
	}{
		{name: "bytes", edit: func(config *Config) { config.MaxTotalDiffBytes = 1 }},
		{name: "lines", edit: func(config *Config) { config.MaxTotalDiffLines = 1 }},
		{name: "paths", edit: func(config *Config) { config.MaxChangedPaths = 1 }},
		{name: "items", edit: func(config *Config) {
			config.MaxPaths = 1
			config.MaxWorksetItems = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := "out-" + test.name
			config := fixtureConfig(repo, output)
			test.edit(&config)
			_, err := Prepare(t.Context(), config)
			if err == nil || !strings.Contains(err.Error(), "scope_too_large") || !strings.Contains(err.Error(), "remediation:") {
				t.Fatalf("Prepare() error = %v, want scope_too_large with remediation", err)
			}
			if _, statErr := os.Stat(filepath.Join(repo, output)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("cap failure published output directory: %v", statErr)
			}
		})
	}
}

func TestPrepareUsesHufuEnvironmentVariables(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/foo.go", "package team\n", "foo change", "2025-01-02T00:00:00Z")
	ws := filepath.Join(repo, "workspace", "hufu-code-review")
	t.Setenv("HUFU_REPOSITORY", repo)
	t.Setenv("HUFU_WORKSPACE", ws)

	result, err := Prepare(context.Background(), Config{Since: "2025-01-01T12:00:00Z"})
	if err != nil {
		t.Fatalf("Prepare with env vars error = %v", err)
	}
	if result.Outputs["item_count"] != 1 {
		t.Fatalf("item_count = %v, want 1", result.Outputs["item_count"])
	}
	if len(result.Artifacts) == 0 {
		t.Fatalf("no artifacts produced")
	}
	// Artifact paths must be relative to HUFU_WORKSPACE (e.g. "workset/workset-manifest.json")
	if result.Artifacts[0].Path != "workset/workset-manifest.json" {
		t.Fatalf("manifest artifact path = %q, want %q", result.Artifacts[0].Path, "workset/workset-manifest.json")
	}
	manifestFile := filepath.Join(ws, "workset", "workset-manifest.json")
	if _, err := os.Stat(manifestFile); err != nil {
		t.Fatalf("manifest file does not exist at %s: %v", manifestFile, err)
	}
}

func TestPrepareSupportsDisjointArtifactRoot(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/foo.go", "package team\n", "foo change", "2025-01-02T00:00:00Z")
	artifactRoot := filepath.Join(t.TempDir(), "action")
	outputDir := filepath.Join(artifactRoot, "workset")
	config := fixtureConfig(repo, outputDir)
	config.ArtifactRoot = artifactRoot

	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatalf("Prepare with disjoint artifact root: %v", err)
	}
	if got, want := result.Artifacts[0].Path, "workset/workset-manifest.json"; got != want {
		t.Fatalf("manifest artifact path = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(repo, "out")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("producer wrote repository output: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "workset-manifest.json")); err != nil {
		t.Fatalf("manifest missing from disjoint artifact root: %v", err)
	}
}

func TestPrepareRejectsSymlinkEscapeFromArtifactRoot(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/foo.go", "package team\n", "foo change", "2025-01-02T00:00:00Z")
	artifactRoot := t.TempDir()
	outside := t.TempDir()
	outputDir := filepath.Join(artifactRoot, "workset")
	if err := os.Symlink(outside, outputDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	config := fixtureConfig(repo, outputDir)
	config.ArtifactRoot = artifactRoot
	if _, err := Prepare(t.Context(), config); err == nil || !strings.Contains(err.Error(), "output_dir") {
		t.Fatalf("Prepare accepted output symlink escape: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "workset-manifest.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlink escape produced output: %v", err)
	}
}

func TestPreparePreservesDirtyWorktreeWhileProducingCommitDiff(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/committed.go", "package team\n\nfunc Committed() int { return 1 }\n", "committed change", "2025-01-02T00:00:00Z")
	writeFile(t, filepath.Join(repo, "README.md"), "dirty tracked content\n")
	writeFile(t, filepath.Join(repo, "notes", "untracked.txt"), "dirty untracked content\n")

	outputDir := filepath.Join(repo, "out")
	before, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatalf("capture before snapshot: %v", err)
	}
	result, err := Prepare(t.Context(), fixtureConfig(repo, "out"))
	if err != nil {
		t.Fatalf("Prepare with dirty worktree: %v", err)
	}
	after, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatalf("capture after snapshot: %v", err)
	}
	if err := compareRepositorySnapshots(before, after); err != nil {
		t.Fatalf("Prepare changed dirty worktree: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "README.md")); err != nil || string(got) != "dirty tracked content\n" {
		t.Fatalf("tracked dirty file changed: content=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(repo, "notes", "untracked.txt")); err != nil || string(got) != "dirty untracked content\n" {
		t.Fatalf("untracked dirty file changed: content=%q err=%v", got, err)
	}
	if len(result.Artifacts) < 2 {
		t.Fatalf("Prepare produced no review diff artifact: %#v", result.Artifacts)
	}
	manifest := readManifest(t, filepath.Join(outputDir, "workset-manifest.json"))
	if len(manifest.Items) == 0 {
		t.Fatal("Prepare produced no workset items")
	}
	diff, err := os.ReadFile(filepath.Join(outputDir, filepath.FromSlash(manifest.Items[0].DiffPath)))
	if err != nil {
		t.Fatalf("read review diff: %v", err)
	}
	if !bytes.Contains(diff, []byte("internal/team/committed.go")) {
		t.Fatalf("review diff omitted committed change:\n%s", diff)
	}
	if bytes.Contains(diff, []byte("dirty tracked content")) || bytes.Contains(diff, []byte("dirty untracked content")) {
		t.Fatalf("commit review diff included uncommitted content:\n%s", diff)
	}
}

func TestPrepareReviewsStagedUnstagedAndUntrackedWorkingTreeChanges(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(repo, "internal/team/staged.go"), "package team\n\nfunc Staged() int { return 1 }\n")
	writeFile(t, filepath.Join(repo, "internal/team/unstaged.go"), "package team\n\nfunc Unstaged() int { return 1 }\n")
	commit(t, repo, "working tree baseline", "2025-01-02T00:00:00Z")

	writeFile(t, filepath.Join(repo, "internal/team/staged.go"), "package team\n\nfunc Staged() int { return 2 }\n")
	gitRun(t, repo, "add", "internal/team/staged.go")
	writeFile(t, filepath.Join(repo, "internal/team/unstaged.go"), "package team\n\nfunc Unstaged() int { return 2 }\n")
	writeFile(t, filepath.Join(repo, "internal/team/untracked.go"), "package team\n\nfunc Untracked() int { return 3 }\n")
	writeFile(t, filepath.Join(repo, "README.md"), "working tree documentation\n")
	writeFile(t, filepath.Join(repo, "ignored.txt"), "must not be reviewed\n")

	outputDir := filepath.Join(repo, "out")
	before, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	config := fixtureConfig(repo, "out")
	config.Scope = resolverScope{Kind: "working_tree", History: "first_parent", Head: "HEAD"}
	config.Since = ""
	config.MaxCommits = 0
	config.Routing = routingDocumentation
	result, err := Prepare(t.Context(), config)
	if err != nil {
		t.Fatalf("Prepare working tree review: %v", err)
	}
	after, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareRepositorySnapshots(before, after); err != nil {
		t.Fatalf("working tree review changed repository: %v", err)
	}

	primary := readManifest(t, filepath.Join(outputDir, "primary", "workset-manifest.json"))
	gotPaths := make([]string, 0)
	for _, item := range primary.Items {
		gotPaths = append(gotPaths, item.TouchedPaths...)
	}
	for _, want := range []string{"internal/team/staged.go", "internal/team/unstaged.go", "internal/team/untracked.go"} {
		if !slices.Contains(gotPaths, want) {
			t.Fatalf("primary working tree paths = %q, missing %q", gotPaths, want)
		}
	}
	if slices.Contains(gotPaths, "ignored.txt") {
		t.Fatalf("working tree review included ignored file: %q", gotPaths)
	}
	documentation := readManifest(t, filepath.Join(outputDir, "documentation", "workset-manifest.json"))
	if len(documentation.Items) == 0 || !slices.Contains(documentation.Items[0].TouchedPaths, "README.md") {
		t.Fatalf("documentation working tree paths = %#v, want README.md", documentation.Items)
	}
	if result.Outputs["commit_count"] != 0 || result.Outputs["range_end"] != "WORKTREE" {
		t.Fatalf("working tree outputs = %#v, want commit_count 0 and WORKTREE range", result.Outputs)
	}
	if primary.Scope.Requested.Kind != "working_tree" || !primary.Scope.Satisfied {
		t.Fatalf("working tree scope attestation = %#v", primary.Scope)
	}
}

func TestPrepareRejectsEmptyWorkingTreeScope(t *testing.T) {
	repo := newFixtureRepo(t)
	config := fixtureConfig(repo, "out")
	config.Scope = resolverScope{Kind: "working_tree", History: "first_parent", Head: "HEAD"}
	config.Since = ""
	config.MaxCommits = 0
	if _, err := Prepare(t.Context(), config); err == nil || !strings.Contains(err.Error(), "scope_empty") {
		t.Fatalf("Prepare empty working tree error = %v, want scope_empty", err)
	}
}

func TestPrepareIgnoresInjectedGitEnvironment(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/target.go", "package team\n", "target change", "2025-01-02T00:00:00Z")
	other := newFixtureRepo(t)
	writeAndCommit(t, other, "internal/team/wrong.go", "package team\n", "wrong change", "2025-01-02T00:00:00Z")

	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other, ".git", "objects"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(other, "hooks"))

	if _, err := Prepare(t.Context(), fixtureConfig(repo, "out")); err != nil {
		t.Fatalf("Prepare with injected Git environment: %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if len(manifest.Items) == 0 || !slices.Contains(manifest.Items[0].TouchedPaths, "internal/team/target.go") {
		t.Fatalf("Prepare reviewed the wrong repository: %#v", manifest.Items)
	}
}

func TestSanitizedGitEnvironmentDisablesOptionalLocks(t *testing.T) {
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	t.Setenv("GIT_DIR", "/tmp/injected-git-dir")
	env := sanitizedGitEnvironment()
	if !slices.Contains(env, "GIT_OPTIONAL_LOCKS=0") {
		t.Fatalf("sanitized environment does not disable optional locks: %q", env)
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_DIR=") || entry == "GIT_OPTIONAL_LOCKS=1" {
			t.Fatalf("sanitized environment retained unsafe Git setting %q", entry)
		}
	}
}

func TestMutationCanaryDetectsRepositoryChanges(t *testing.T) {
	repo := newFixtureRepo(t)
	outputDir := filepath.Join(t.TempDir(), "workset")
	before, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "README.md"), "mutated\n")
	after, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareRepositorySnapshots(before, after); err == nil || !strings.Contains(err.Error(), "repository mutation detected") {
		t.Fatalf("mutation canary error = %v, want repository mutation detection", err)
	}
}

func TestMutationCanaryDetectsContentChangeWhenDirtyStatusIsUnchanged(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "README.md"), "first dirty value\n")
	outputDir := filepath.Join(t.TempDir(), "workset")
	before, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(repo, "README.md"), "second dirty value\n")
	after, err := captureRepositorySnapshot(t.Context(), repo, outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if before.status != after.status {
		t.Fatalf("fixture changed porcelain status: before=%q after=%q", before.status, after.status)
	}
	if err := compareRepositorySnapshots(before, after); err == nil || !strings.Contains(err.Error(), "dirty tracked or untracked file content changed") {
		t.Fatalf("mutation canary error = %v, want dirty content detection", err)
	}
}

func TestPrepareRejectsOutputInsideGitAdministrativeDirectory(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/foo.go", "package team\n", "foo change", "2025-01-02T00:00:00Z")
	config := fixtureConfig(repo, filepath.Join(repo, ".git", "review-output"))
	config.ArtifactRoot = filepath.Join(repo, ".git")
	if _, err := Prepare(t.Context(), config); err == nil || !strings.Contains(err.Error(), "Git administrative directory") {
		t.Fatalf("Prepare accepted output inside .git: %v", err)
	}
}

type goldenSummary struct {
	SchemaVersion int          `json:"schema_version"`
	CommitCount   int          `json:"commit_count"`
	ChangedFiles  int          `json:"changed_files"`
	Items         []goldenItem `json:"items"`
}

type goldenItem struct {
	Key      string   `json:"key"`
	Lens     string   `json:"lens"`
	Paths    []string `json:"paths"`
	DiffPath string   `json:"diff_path"`
}

func fixtureConfig(repo, output string) Config {
	return Config{
		Repository: repo, OutputDir: output, Since: "2025-01-01T12:00:00Z", MaxCommits: 10,
		MaxTotalDiffBytes: defaultMaxTotalDiffBytes, MaxTotalDiffLines: defaultMaxTotalDiffLines,
		MaxChangedPaths: defaultMaxChangedPaths, MaxWorksetItems: defaultMaxWorksetItems,
		MaxDiffBytes: 24_000, MaxDiffLines: 600, MaxPaths: 16,
	}
}

func newFixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	gitRun(t, repo, "init", "--initial-branch=main")
	writeFile(t, filepath.Join(repo, "README.md"), "base\n")
	commit(t, repo, "base", "2025-01-01T00:00:00Z")
	return repo
}

func writeAndCommit(t *testing.T, repo, path, content, message, date string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, path), content)
	commit(t, repo, message, date)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo, message, date string) {
	t.Helper()
	gitRun(t, repo, "add", "-A")
	cmd := exec.Command("git", "-c", "user.name=Review Prep Test", "-c", "user.email=reviewprep@example.test", "commit", "-m", message)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, output)
	}
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func readManifest(t *testing.T, path string) manifest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value manifest
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func readGoldenSummary(t *testing.T) goldenSummary {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", "small-workset.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value goldenSummary
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
