package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestRelatedGoPathsFollowPackageDependencies(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.test/review\n\ngo 1.26\n")
	for name, content := range map[string]string{
		"service/api.go":             "package service\ntype Handle struct{}\nfunc (*Handle) Context() {}\nfunc (*Handle) Close() {}\n",
		"service/local.go":           "package service\nfunc local(h *Handle) { h.Close() }\n",
		"service/api_test.go":        "package service_test\nimport api \"example.test/review/service\"\nfunc test() { _ = api.Handle{} }\n",
		"direct/caller.go":           "package direct\nimport renamed \"example.test/review/service\"\nfunc call(h *renamed.Handle) { h.Close() }\n",
		"dot/caller.go":              "package dot\nimport . \"example.test/review/service\"\nfunc call(h *Handle) { h.Context() }\n",
		"service/cycle.go":           "package service\nimport \"example.test/review/bridge\"\nfunc cyclic() { _ = bridge.Bridge() }\n",
		"bridge/bridge.go":           "package bridge\nimport \"example.test/review/service\"\nfunc Bridge() *service.Handle { return nil }\n",
		"indirect/caller.go":         "package indirect\nimport \"example.test/review/bridge\"\nfunc call() { bridge.Bridge().Context() }\n",
		"unrelated/context.go":       "package unrelated\nimport \"context\"\nfunc Close(ctx context.Context) {}\n",
		"other/service/same_name.go": "package service\nfunc Context() {}\n",
	} {
		writeFile(t, filepath.Join(root, name), content)
	}
	snapshot := &reviewSourceSnapshot{root: root}
	got, err := snapshot.relatedGoPaths([]string{"service/api.go"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"service/api_test.go", "service/local.go", "direct/caller.go", "dot/caller.go", "bridge/bridge.go", "indirect/caller.go"} {
		if !slices.Contains(got, name) {
			t.Errorf("omitted relevant dependency %s: %v", name, got)
		}
	}
	for _, name := range []string{"unrelated/context.go", "other/service/same_name.go"} {
		if slices.Contains(got, name) {
			t.Errorf("unrelated name match selected %s", name)
		}
	}
}

func TestPrepareBoundsDependenciesWithCommonMethodNames(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.test/review\n\ngo 1.26\n")
	primary := "package main\ntype handle struct{}\nfunc (*handle) Context() {}\nfunc (*handle) Close() {}\n"
	writeFile(t, filepath.Join(repo, "cmd/app/handle.go"), primary)
	writeFile(t, filepath.Join(repo, "cmd/app/handle_test.go"), "package main\nfunc test(h *handle) { h.Close() }\n")
	for i := range 1850 {
		writeFile(t, filepath.Join(repo, "unrelated", fmt.Sprintf("context_%04d.go", i)), "package unrelated\nimport \"context\"\nfunc use(ctx context.Context) {}\n")
	}
	commit(t, repo, "common method baseline", "2025-01-02T00:00:00Z")
	writeAndCommit(t, repo, "cmd/app/handle.go", primary+"// inspect the changed implementation\n", "change handle", "2025-01-03T00:00:00Z")
	config := sourceBudgetFixtureConfig(repo, "out")
	config.MaxSourceSnapshotBytes = defaultMaxSourceSnapshotBytes
	config.MaxReviewInputTokens = defaultMaxReviewInputTokens
	config.MaxWorksetItems = 32
	if _, err := Prepare(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out/workset-manifest.json"))
	if len(manifest.Items) != 1 {
		t.Fatalf("unexpected source partitions: %d", len(manifest.Items))
	}
	assertReviewInputBudget(t, repo, manifest.Items[0], config)
	assertRequiredArtifactInputs(t, repo, manifest.Items[0])
}

func TestGoDependencyOwnershipPreservesNestedModulesAndUnknownScopes(t *testing.T) {
	for _, tc := range []struct {
		name, module, sourceDir, importPath string
		wantUnrelated                       bool
	}{
		{name: "nested module", module: "module example.test/nested\n", sourceDir: "nested/api", importPath: "example.test/nested/api"},
		{name: "vendored import", module: "module example.test/unused\n", sourceDir: "vendor/example.test/vendored/api", importPath: "example.test/vendored/api"},
		{name: "unknown module", module: "", sourceDir: "api", importPath: "example.test/unknown/api", wantUnrelated: true},
		{name: "invalid nearest module", module: "invalid module declaration\n", sourceDir: "nested/api", importPath: "example.test/unknown/api", wantUnrelated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.module != "" {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.test/root\n")
				writeFile(t, filepath.Join(root, "nested/go.mod"), tc.module)
			}
			primary := tc.sourceDir + "/target.go"
			writeFile(t, filepath.Join(root, primary), "package api\nfunc Target() {}\n")
			writeFile(t, filepath.Join(root, "caller/caller.go"), fmt.Sprintf("package caller\nimport ref %q\nfunc Call() {ref.Target()}\n", tc.importPath))
			writeFile(t, filepath.Join(root, "unrelated/target.go"), "package unrelated\nfunc Target() {}\n")
			snapshot := &reviewSourceSnapshot{root: root}
			got, err := snapshot.relatedGoPaths([]string{primary})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(got, "caller/caller.go") || slices.Contains(got, "unrelated/target.go") != tc.wantUnrelated {
				t.Fatalf("dependency ownership=%v", got)
			}
		})
	}
}

func TestGoModulePathDoesNotGuessAmbiguousOwnership(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"ordinary", "module example.test/review\n", "example.test/review"},
		{"quoted", "module \"example.test/review\" // comment\n", "example.test/review"},
		{"dependency block", "module example.test/review\nrequire (\nmodule v1.0.0\n)\n", "example.test/review"},
		{"duplicate", "module example.test/first\nmodule example.test/second\n", ""},
		{"missing value", "module\n", ""},
		{"relative", "module ../outside\n", ""},
		{"malformed quote", "module \"unfinished\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := goModulePath([]byte(tc.data)); got != tc.want {
				t.Fatalf("module ownership=%q want %q", got, tc.want)
			}
		})
	}
}
