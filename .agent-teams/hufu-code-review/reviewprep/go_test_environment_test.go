package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
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
	}
	var config strings.Builder
	for _, key := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
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
					t.Setenv(key, value)
				}
				want[key] = value
			}
			env, err := sanitizedGoTestEnvironment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			probe := exec.CommandContext(t.Context(), "go", "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE", "GOENV", "GOPROXY", "GOFLAGS")
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

func TestSanitizedGoTestEnvironmentFailsClosedOnResolutionError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	env, err := sanitizedGoTestEnvironment(ctx)
	if !errors.Is(err, context.Canceled) || env != nil {
		t.Fatalf("canceled cache resolution = %v, %v; want no fallback environment", env, err)
	}
}

func TestVerifyReviewTestsUsesPersistentlyConfiguredModuleCache(t *testing.T) {
	persistedGoTestPaths(t)
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/review\n\ngo 1.26.0\n\nrequire example.com/cached v1.0.0\n")
	writeFile(t, filepath.Join(repo, "calc", "calc.go"), "package calc\n\nimport \"example.com/cached\"\n\nfunc Value() int { return cached.Value }\n")
	writeFile(t, filepath.Join(repo, "calc", "calc_test.go"), "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 7 { t.Fatal(Value()) } }\n")
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
