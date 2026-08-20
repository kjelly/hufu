package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	got := goldenSummary{SchemaVersion: manifest.SchemaVersion, CommitCount: manifest.Range.CommitCount, ChangedFiles: manifest.ChangedFiles}
	for _, entry := range manifest.Items {
		got.Items = append(got.Items, goldenItem{Key: entry.Key, Lens: entry.Lens, Paths: entry.Paths, DiffPath: entry.DiffPath})
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

func TestPrepareDoesNotIncludeDirtyWorkingTree(t *testing.T) {
	repo := newFixtureRepo(t)
	writeAndCommit(t, repo, "internal/team/committed.go", "package team\n", "committed change", "2025-01-02T00:00:00Z")
	writeFile(t, filepath.Join(repo, "internal/team/uncommitted.go"), "package team\n")

	if _, err := Prepare(context.Background(), fixtureConfig(repo, "out")); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	manifest := readManifest(t, filepath.Join(repo, "out", "workset-manifest.json"))
	if manifest.ChangedFiles != 1 || manifest.Items[0].Paths[0] != "internal/team/committed.go" {
		t.Fatalf("manifest included dirty working tree: %#v", manifest)
	}
}

func TestPrepareHandlesEmptyRange(t *testing.T) {
	repo := newFixtureRepo(t)
	outputDir := filepath.Join(repo, "review-output")
	result, err := Prepare(context.Background(), Config{
		Repository:   repo,
		OutputDir:    outputDir,
		Since:        "2025-01-01T12:00:00Z",
		MaxDiffBytes: 1024,
		MaxDiffLines: 100,
		MaxPaths:     5,
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	manifest := readManifest(t, filepath.Join(outputDir, "workset-manifest.json"))
	if manifest.ChangedFiles != 0 || len(manifest.Items) != 0 {
		t.Fatalf("empty range manifest = %+v, want no files or items", manifest)
	}
	if result.Outputs["item_count"] != 0 || result.Outputs["changed_files"] != 0 {
		t.Fatalf("result outputs = %+v, want no files or items", result.Outputs)
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
	return Config{Repository: repo, OutputDir: output, Since: "2025-01-01T12:00:00Z", MaxDiffBytes: 24_000, MaxDiffLines: 600, MaxPaths: 16}
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
