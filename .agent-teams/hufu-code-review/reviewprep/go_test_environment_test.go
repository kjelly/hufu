package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/team"
)

func persistedGoTestPaths(t *testing.T) map[string]string {
	t.Helper()
	cache, err := exec.CommandContext(t.Context(), "go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	paths := map[string]string{
		"GOPATH":     filepath.Join(root, "operator go path"),
		"GOMODCACHE": filepath.Join(root, "operator module cache"),
		"GOCACHE":    strings.TrimSpace(string(cache)),
		"GOTMPDIR":   filepath.Join(root, "operator build scratch"),
	}
	if err := os.MkdirAll(paths["GOTMPDIR"], 0o700); err != nil {
		t.Fatal(err)
	}
	var config strings.Builder
	for _, key := range []string{"GOPATH", "GOMODCACHE", "GOCACHE", "GOTMPDIR"} {
		t.Setenv(key, "")
		config.WriteString(key + "=" + paths[key] + "\n")
	}
	config.WriteString("GOFLAGS=-tags=operator_setting\nGOPROXY=https://invalid.example\n")
	filename := filepath.Join(root, "go-env")
	writeFile(t, filename, config.String())
	t.Setenv("GOENV", filename)
	t.Setenv("GOTOOLCHAIN", "local")
	return paths
}

func TestSanitizedGoTestEnvironmentPreservesEffectiveCachePaths(t *testing.T) {
	paths := persistedGoTestPaths(t)
	t.Setenv("REVIEW_TEST_SECRET", "must-not-reach-tests")
	for _, test := range []struct {
		name     string
		explicit bool
	}{{name: "persistent_config"}, {name: "explicit_environment", explicit: true}} {
		t.Run(test.name, func(t *testing.T) {
			want := make(map[string]string)
			for key, value := range paths {
				if test.explicit {
					value = filepath.Join(t.TempDir(), key)
					if key == "GOTMPDIR" {
						if err := os.MkdirAll(value, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv(key, value)
				}
				want[key] = value
			}
			env, err := sanitizedGoTestEnvironment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			probe := exec.CommandContext(t.Context(), "go", "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE", "GOTMPDIR", "GOENV", "GOPROXY", "GOFLAGS")
			probe.Env = env
			output, err := probe.Output()
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]string
			if err := json.Unmarshal(output, &got); err != nil {
				t.Fatal(err)
			}
			// go env reports an empty config filename when GOENV=off.
			want["GOENV"], want["GOPROXY"], want["GOFLAGS"] = "", "off", "-buildvcs=false"
			for key, value := range want {
				if got[key] != value {
					t.Errorf("effective %s = %q, want %q", key, got[key], value)
				}
			}
			for _, entry := range env {
				if strings.HasPrefix(entry, "REVIEW_TEST_SECRET=") {
					t.Fatal("test environment inherited an unrelated secret")
				}
			}
		})
	}
}

func TestSanitizedGoTestEnvironmentPreservesConcurrencyLimit(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
		unset             bool
	}{
		{name: "unset", unset: true, want: "2"},
		{name: "empty", want: "2"},
		{name: "one", value: "1", want: "1"},
		{name: "two", value: "2", want: "2"},
		{name: "four", value: "4", want: "4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("GOMAXPROCS", test.value)
			if test.unset {
				if err := os.Unsetenv("GOMAXPROCS"); err != nil {
					t.Fatal(err)
				}
			}
			env, err := sanitizedGoTestEnvironment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var settings []string
			for _, entry := range env {
				if strings.HasPrefix(entry, "GOMAXPROCS=") {
					settings = append(settings, entry)
				}
			}
			if len(settings) != 1 || settings[0] != "GOMAXPROCS="+test.want {
				t.Fatalf("concurrency settings = %v, want one GOMAXPROCS=%s", settings, test.want)
			}
			probe := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestGoTestEnvironmentConcurrencyChild$")
			probe.Env = append(env, "HUFU_REVIEW_TEST_CONCURRENCY_CHILD=1")
			output, err := probe.Output()
			if err != nil || string(output) != test.want {
				t.Fatalf("child GOMAXPROCS = %q, error=%v, want %s", output, err, test.want)
			}
		})
	}
}

func TestGoTestEnvironmentConcurrencyChild(t *testing.T) {
	if os.Getenv("HUFU_REVIEW_TEST_CONCURRENCY_CHILD") != "1" {
		return
	}
	fmt.Print(runtime.GOMAXPROCS(0))
	os.Exit(0)
}

func TestSanitizedGoTestEnvironmentFailsClosedOnResolutionError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	env, err := sanitizedGoTestEnvironment(ctx)
	if !errors.Is(err, context.Canceled) || env != nil {
		t.Fatalf("canceled cache resolution = %v, %v; want no fallback environment", env, err)
	}
}

func TestVerifyReviewTestsSealsCachePreflightEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, gap, kind string
		hasGo           bool
	}{
		{"empty_scope", "test_scope_empty", "none", false},
		{"cache_probe_timeout", "test_timeout_gap", "preflight", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFixtureRepo(t)
			if tc.hasGo {
				writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/review\n\ngo 1.27\n")
				writeFile(t, filepath.Join(repo, "code.go"), "package review\n")
			} else {
				writeFile(t, filepath.Join(repo, "README.md"), "Documentation change\n")
			}
			commit(t, repo, "Review source", "2025-01-02T00:00:00Z")
			config := fixtureConfig(repo, "verification")
			config.Scope, config.Since = resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: "HEAD"}, ""
			if !tc.hasGo {
				config = verificationWithFrozenSnapshot(t, repo)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "probe-ran")
			t.Setenv("CACHE_PROBE_TEST_MARKER", marker)
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\n: > \"$CACHE_PROBE_TEST_MARKER\"\n[ \"$1\" = env ] || exit 4\nexec sleep 30\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), defaultGoTestLimits().Reserve+2*time.Second)
			defer cancel()
			result, err := VerifyReviewTests(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); tc.hasGo && err != nil || !tc.hasGo && !os.IsNotExist(err) {
				t.Fatalf("unexpected Go probe marker, Go files=%v: %v", tc.hasGo, err)
			}
			receipt := result.Outputs["targeted_go_tests"].(goTestReceipt)
			if receipt.Passed || receipt.Completed || receipt.TimedOut != tc.hasGo || receipt.TimeoutKind != tc.kind ||
				receipt.Gap != tc.gap || (len(receipt.Packages) > 0) != tc.hasGo ||
				len(receipt.Commands) != 0 || result.Outputs["test_evidence_sealed"] != true ||
				len(result.Artifacts) != 2 || receipt.ArtifactID != result.Artifacts[0].ID || receipt.OutputArtifactID != result.Artifacts[1].ID {
				t.Fatalf("unexpected preflight receipt: %+v, artifacts %+v", receipt, result.Artifacts)
			}
		})
	}
}

func verificationWithFrozenSnapshot(t *testing.T, repo string) Config {
	t.Helper()
	root := t.TempDir()
	producer := fixtureConfig(repo, filepath.Join(root, "producer"))
	producer.ArtifactRoot = root
	producer.Scope, producer.Since = resolverScope{Kind: "repository_snapshot", Head: "HEAD", History: "first_parent"}, ""
	prepared, err := Prepare(t.Context(), producer)
	if err != nil {
		t.Fatal(err)
	}
	scope := prepared.Outputs["scope"].(scopeAttestation)
	inventory := scope.RepositorySnapshot.Inventory
	frozen, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(inventory.Path)))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(runtimestore.ArtifactRoot(root), "data", inventory.ID), string(frozen))
	var plain any
	if err := json.Unmarshal(mustJSON(t, scope), &plain); err != nil {
		t.Fatal(err)
	}
	outputs, digest, err := team.CanonicalizeRuntimeOutputs(map[string]any{"scope": plain})
	if err != nil {
		t.Fatal(err)
	}
	producerTask := map[string]any{
		"ID": "1", "Status": "done", "contract_id": "produce-workset", "run_input_snapshot_id": "frozen-inputs",
		"execution_receipt": map[string]any{"run_id": "verify-run", "task_id": "1", "attempt": 1, "action_invocation_id": "produce", "runtime_outputs_hash": digest},
		"VerifyResult":      map[string]any{"exit_code": 0, "fingerprint": "verified"},
		"typed_result":      map[string]any{"status": "success", "runtime_outputs": outputs, "runtime_outputs_hash": digest},
	}
	verifierTask := map[string]any{"ID": "2", "Status": "in_progress", "run_input_snapshot_id": "frozen-inputs"}
	store, err := runtimestore.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	checkpoint := mustJSON(t, map[string]any{"active_run_input_snapshot_id": "frozen-inputs", "tasks": []any{producerTask, verifierTask}})
	if err := store.PutDocument(t.Context(), runtimestore.DocumentSession, checkpoint); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUFU_WORKSPACE", filepath.Join(root, "runtime", "runs", "verify-run", "actions", "verify"))
	t.Setenv("HUFU_RUN_ID", "verify-run")
	t.Setenv("HUFU_ACTION_INVOCATION_ID", "verify")
	t.Setenv("HUFU_TASK_ID", "2")
	t.Setenv("HUFU_RECOVERED_TASK_RUNS", "{}")
	producer.OutputDir = filepath.Join(root, "verification")
	return producer
}

func TestVerifyReviewTestsUsesPersistentlyConfiguredModuleCache(t *testing.T) {
	persistedGoTestPaths(t)
	t.Setenv("GOMAXPROCS", "1")
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/review\n\ngo 1.26.0\n\nrequire example.com/cached v1.0.0\n")
	writeFile(t, filepath.Join(repo, "calc", "calc.go"), "package calc\n\nimport \"example.com/cached\"\n\nfunc Value() int { return cached.Value }\n")
	writeFile(t, filepath.Join(repo, "calc", "calc_test.go"), "package calc\n\nimport (\"runtime\"; \"testing\")\n\nfunc TestValue(t *testing.T) { if Value() != 7 { t.Fatal(Value()) }; if got := runtime.GOMAXPROCS(0); got != 1 { t.Fatalf(\"GOMAXPROCS = %d, want 1\", got) } }\n")
	seedCachedGoTestDependency(t, repo)
	commit(t, repo, "feat: use cached dependency", "2025-01-02T00:00:00Z")

	scope := resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: "HEAD"}
	config := fixtureConfig(repo, "verification")
	config.Scope, config.Since = scope, ""
	result, err := VerifyReviewTests(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	assertCachedGoTestPassed(t, result)

	// Production executes reviewprep through Yaegi, so exercise the same path
	// with cache locations present only in the persistent Go configuration.
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	request := mustJSON(t, actionRequest{Type: "verify_review_tests", Payload: string(mustJSON(t, wireConfig{
		Repository: repo, ArtifactRoot: workspace, OutputDir: filepath.Join(workspace, "verification"), Scope: &scope,
	}))})
	child, err := golangruntime.Execute(t.Context(), "", program, request, os.Environ(), 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("embedded verifier: %v; stderr=%s", err, child.Stderr)
	}
	if err := json.Unmarshal(child.Stdout, &result); err != nil {
		t.Fatal(err)
	}
	assertCachedGoTestPassed(t, result)
}

func seedCachedGoTestDependency(t *testing.T, repo string) {
	t.Helper()
	proxy := t.TempDir()
	versionDir := filepath.Join(proxy, "example.com", "cached", "@v")
	module := "module example.com/cached\n\ngo 1.21\n"
	writeFile(t, filepath.Join(versionDir, "v1.0.0.mod"), module)
	writeFile(t, filepath.Join(versionDir, "v1.0.0.info"), `{"Version":"v1.0.0","Time":"2025-01-01T00:00:00Z"}`)
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range map[string]string{"go.mod": module, "cached.go": "package cached\n\nconst Value = 7\n"} {
		file, err := writer.Create("example.com/cached@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionDir, "v1.0.0.zip"), archive.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	download := exec.CommandContext(t.Context(), "go", "mod", "download", "example.com/cached")
	download.Dir = repo
	download.Env = replaceEnvironmentValue(offlineGoEnvironment(), "GOPROXY", (&url.URL{Scheme: "file", Path: filepath.ToSlash(proxy)}).String())
	download.Env = replaceEnvironmentValue(download.Env, "GOFLAGS", "-modcacherw")
	if output, err := download.CombinedOutput(); err != nil {
		t.Fatalf("seed local module cache: %v; output=%s", err, output)
	}
}

func assertCachedGoTestPassed(t *testing.T, result actionResult) {
	t.Helper()
	var receipt goTestReceipt
	if err := json.Unmarshal(mustJSON(t, result.Outputs["targeted_go_tests"]), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Passed || receipt.ExitCode != 0 {
		t.Fatalf("offline cached-dependency verification failed: %#v", receipt)
	}
}
