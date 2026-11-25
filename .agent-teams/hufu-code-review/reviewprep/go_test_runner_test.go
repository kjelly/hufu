package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/golangruntime"
	"go.yaml.in/yaml/v3"
)

func TestSnapshotGoTestsPreserveFullOutputAndSummarize(t *testing.T) {
	for _, code := range []string{"0", "1"} {
		t.Run("exit_"+code, func(t *testing.T) {
			root := t.TempDir()
			// Output exceeds the historical 128KiB cap. Keep both stdout/stderr
			// and an ending diagnostic that the old prefix-only buffer discarded.
			body := "BEGIN\n" + strings.Repeat("diagnostic line\n", 12000) + "END\n"
			writeFile(t, filepath.Join(root, "source"), body)
			writeFile(t, filepath.Join(root, "go"), "#!/bin/sh\n/bin/cat source\nprintf 'STDERR-END\\n' >&2\nexit "+code+"\n")
			if err := os.Chmod(filepath.Join(root, "go"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root)
			env := os.Environ()
			outputPath := filepath.Join(root, "full.log")
			got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, snapshotGoTestTargets{packages: []string{"./target"}}, env, outputPath, defaultGoTestLimits())
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			want := []byte(body + "STDERR-END\n")
			if !bytes.Equal(data, want) || got.OutputSHA256 != sha256Hex(want) || got.OutputBytes != len(want) {
				t.Fatal("complete diagnostic bytes or hash changed")
			}
			if !got.Completed || got.Passed != (code == "0") || got.OutputTruncated || !got.SummaryTruncated || len(got.Diagnostic) > 8500 || !strings.Contains(got.Diagnostic, "BEGIN") || !strings.Contains(got.Diagnostic, "STDERR-END") {
				t.Fatalf("summary metadata = %+v", got)
			}
		})
	}
}

func TestSnapshotGoTestsSelectActiveDirectImportersAcrossModules(t *testing.T) {
	for _, mode := range []string{"module_replacement", "workspace_override", "workspace_version_override"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			replacement := "./nested"
			work := "go 1.22\n\nuse .\n"
			switch mode {
			case "workspace_override":
				replacement = "../unavailable"
				work += "replace example.test/alias => ./nested\n"
			case "workspace_version_override":
				replacement = "../unavailable"
				work += "replace example.test/alias v0.0.0 => ./nested\n"
			}
			writeFile(t, filepath.Join(root, "go.mod"), "module example.test/root\n\ngo 1.22\n\nrequire example.test/alias v0.0.0\nreplace example.test/alias => "+replacement+"\n")
			writeFile(t, filepath.Join(root, "go.work"), work)
			writeFile(t, filepath.Join(root, "nested/go.mod"), "module example.test/nested\n\ngo 1.22\n")
			writeFile(t, filepath.Join(root, "nested/calc/calc.go"), "package calc\nconst Value = 42\n")
			writeFile(t, filepath.Join(root, "caller/caller.go"), "package caller\nimport \"example.test/alias/calc\"\nconst Value = calc.Value\n")
			writeFile(t, filepath.Join(root, "indirect/indirect.go"), "package indirect\nimport \"example.test/root/caller\"\nconst Value = caller.Value\n")
			writeFile(t, filepath.Join(root, "ignored/ignored.go"), "//go:build ignore\n\npackage ignored\nimport _ \"example.test/alias/calc\"\n")
			writeFile(t, filepath.Join(root, "decoy/go.mod"), "module example.test/decoy\n\ngo 1.22\n")
			writeFile(t, filepath.Join(root, "decoy/calc/calc.go"), "package calc\nconst Value = 0\n")
			writeFile(t, filepath.Join(root, "other/go.mod"), "module example.test/other\n\ngo 1.22\n\nrequire example.test/alias v0.0.0\nreplace example.test/alias => ../decoy\n")
			writeFile(t, filepath.Join(root, "other/caller.go"), "//go:build broken &&\n\npackage other\nimport _ \"example.test/alias/calc\"\n")
			writeFile(t, filepath.Join(root, "irrelevant/go.mod"), "module example.test/irrelevant\n\ngo 1.22\n\nreplace unrelated.test/dependency => ../../unavailable\n")
			writeFile(t, filepath.Join(root, "irrelevant/caller.go"), "//go:build broken &&\n\npackage irrelevant\nimport _ \"fmt\"\n")
			otherOS := "windows"
			if runtime.GOOS == otherOS {
				otherOS = "linux"
			}
			writeFile(t, filepath.Join(root, "platform/only_"+otherOS+".go"), "package platform\nimport _ \"example.test/alias/calc\"\n")
			snapshot := &reviewSourceSnapshot{root: root, identity: &repositorySnapshotManifest{}}
			plan, err := snapshot.planSnapshotGoTests([]string{"nested/calc/calc.go"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := runSnapshotGoTests(t.Context(), snapshot, snapshotGoTestTargets{plan: plan}, nil, filepath.Join(t.TempDir(), "full.log"), defaultGoTestLimits())
			if err != nil || !got.Completed || !got.Passed || !slices.Equal(got.Packages, []string{"./caller", "./nested/calc"}) ||
				len(got.Commands) != 2 || !slices.Equal(got.Commands[1][:3], []string{"go", "-C", "nested"}) ||
				got.Commands[1][len(got.Commands[1])-1] != "./calc" {
				t.Fatalf("Go test verification = %+v, error = %v", got, err)
			}
			if mode == "workspace_version_override" {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.test/root\n\ngo 1.22\n\nrequire example.test/alias v0.0.1\nreplace example.test/alias => "+replacement+"\n")
				got, err := runSnapshotGoTests(t.Context(), snapshot, snapshotGoTestTargets{plan: plan}, nil, filepath.Join(t.TempDir(), "version-gap.log"), defaultGoTestLimits())
				if err != nil || got.Gap != "test_dependency_outside_snapshot" || len(got.Command) != 0 {
					t.Fatalf("effective outside wildcard = %+v, %v", got, err)
				}
			}
			writeFile(t, filepath.Join(root, "nested/go.mod"), "module example.test/nested\n\ngo 1.22\n\nreplace required.test/dependency => ../../unavailable\n")
			plan, err = snapshot.planSnapshotGoTests([]string{"nested/calc/calc.go"})
			if err != nil {
				t.Fatal(err)
			}
			got, err = runSnapshotGoTests(t.Context(), snapshot, snapshotGoTestTargets{plan: plan}, nil, filepath.Join(t.TempDir(), "dependency-gap.log"), defaultGoTestLimits())
			if err != nil || got.Gap != "test_dependency_outside_snapshot" || got.Passed {
				t.Fatalf("missing dependency in selected module = %+v, %v", got, err)
			}
		})
	}
}

func TestSnapshotGoTestsKeepDirectImportersInStandaloneModulesWithSamePath(t *testing.T) {
	root := t.TempDir()
	for _, module := range []string{"first", "second"} {
		writeFile(t, filepath.Join(root, module, "go.mod"), "module example.test/shared\n\ngo 1.22\n")
		writeFile(t, filepath.Join(root, module, "calc/calc.go"), "package calc\n")
		writeFile(t, filepath.Join(root, module, "caller/caller.go"), "package caller\nimport _ \"example.test/shared/calc\"\n")
	}
	snapshot := &reviewSourceSnapshot{root: root, identity: &repositorySnapshotManifest{}}
	plan, err := snapshot.planSnapshotGoTests([]string{"first/calc/calc.go", "second/calc/calc.go"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSnapshotGoTests(t.Context(), snapshot, snapshotGoTestTargets{plan: plan}, nil, filepath.Join(t.TempDir(), "full.log"), defaultGoTestLimits())
	if want := []string{"./first/calc", "./first/caller", "./second/calc", "./second/caller"}; err != nil || !got.Passed || !slices.Equal(got.Packages, want) {
		t.Fatalf("standalone verification = %+v, error = %v", got, err)
	}
}

func TestGoTestDeadlinesFitActionBudget(t *testing.T) {
	data, err := os.ReadFile("../team.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Spec struct {
			Timeout   int `yaml:"timeout"`
			Providers map[string]struct {
				Timeout int `yaml:"timeout"`
			} `yaml:"action-providers"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	limits := defaultGoTestLimits()
	action := time.Duration(manifest.Spec.Providers["verify-review-tests"].Timeout) * time.Second
	if limits.Package != 15*time.Minute || limits.Command != 20*time.Minute || action != 25*time.Minute || time.Duration(manifest.Spec.Timeout)*time.Second < action {
		t.Fatalf("inconsistent test/command/action/worker deadlines: %+v, %s, %d", limits, action, manifest.Spec.Timeout)
	}
}

func TestEmbeddedVerifierPreservesVerboseFailedTestEvidence(t *testing.T) {
	repo := newFixtureRepo(t)
	writeFile(t, filepath.Join(repo, "go.mod"), "module example.com/verbose\n\ngo 1.27.2\n")
	writeFile(t, filepath.Join(repo, "verbose_test.go"), `package verbose
import ("fmt"; "strings"; "testing")
func TestVerbose(t *testing.T) { fmt.Print("BEGIN-DIAGNOSTIC\n"+strings.Repeat("evidence line\n", 14000)); t.Fatal("FINAL-FAILURE") }
`)
	commit(t, repo, "add verbose failing test", "2025-01-02T00:00:00Z")
	source, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	program, err := golangruntime.Prepare(source, ".")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	scope := resolverScope{Kind: "last_n", Count: 1, History: "first_parent", Head: "HEAD"}
	request := mustJSON(t, actionRequest{Type: "verify_review_tests", Payload: string(mustJSON(t, wireConfig{
		Repository: repo, ArtifactRoot: workspace, OutputDir: filepath.Join(workspace, "verification"), Scope: &scope,
	}))})
	child, err := golangruntime.Execute(t.Context(), "", program, request, os.Environ(), 1<<20, 16<<10)
	if err != nil {
		t.Fatalf("embedded verifier: %v; %s", err, child.Stderr)
	}
	var result actionResult
	if err := json.Unmarshal(child.Stdout, &result); err != nil {
		t.Fatal(err)
	}
	var receipt goTestReceipt
	if err := json.Unmarshal(mustJSON(t, result.Outputs["targeted_go_tests"]), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Completed || receipt.Passed || receipt.TimedOut || receipt.OutputTruncated || !receipt.SummaryTruncated || len(result.Artifacts) != 2 || len(receipt.Diagnostic) > 8500 || !strings.Contains(receipt.Diagnostic, "FINAL-FAILURE") {
		t.Fatalf("verbose failed evidence = %+v", receipt)
	}
	data, err := os.ReadFile(filepath.Join(workspace, result.Artifacts[1].Path))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 128*1024 || !bytes.Contains(data, []byte("BEGIN-DIAGNOSTIC")) || !bytes.Contains(data, []byte("FINAL-FAILURE")) || receipt.OutputArtifactSHA256 != sha256Hex(data) || receipt.OutputArtifactID != result.Artifacts[1].ID {
		t.Fatal("embedded verifier lost complete diagnostic or its binding")
	}
}

func TestSnapshotGoTestsDetectRunnerTimeout(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/alarm\n\ngo 1.27.2\n")
	writeFile(t, filepath.Join(root, "alarm_test.go"), "package alarm\nimport (\"testing\"; \"time\")\nfunc TestWait(t *testing.T) { time.Sleep(time.Second) }\n")
	env, err := sanitizedGoTestEnvironment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	limits := defaultGoTestLimits()
	limits.Package, limits.Command, limits.Reserve = 50*time.Millisecond, 30*time.Second, 0
	outputPath := filepath.Join(root, "full.log")
	got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, snapshotGoTestTargets{packages: []string{"."}}, env, outputPath, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gap != "test_timeout_gap" || got.TimeoutKind != "test" || !got.TimedOut || got.Passed || got.Completed || got.ExitCode != 1 || got.OutputTruncated {
		t.Fatalf("runner alarm = %+v", got)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Contains(data, []byte("panic: test timed out after 50ms")) {
		t.Fatalf("runner alarm output = %s, %v", data, err)
	}
}

func TestSnapshotGoTestsDistinguishCommandAndCallerTimeout(t *testing.T) {
	for _, kind := range []string{"preflight", "command", "caller_deadline", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go"), "#!/bin/sh\nprintf 'started\\n'\nexec /bin/sleep 10\n")
			if err := os.Chmod(filepath.Join(root, "go"), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root)
			env := os.Environ()
			ctx := t.Context()
			limits := defaultGoTestLimits()
			limits.Command, limits.Reserve = 100*time.Millisecond, 0
			snapshot := &reviewSourceSnapshot{root: root}
			if kind == "preflight" {
				writeFile(t, filepath.Join(root, "go.mod"), "module example.com/alarm\n")
				snapshot.identity = &repositorySnapshotManifest{}
			}
			if kind == "caller_deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()
				limits.Command = time.Second
			}
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := runSnapshotGoTests(ctx, snapshot, snapshotGoTestTargets{packages: []string{"."}}, env, filepath.Join(root, "full.log"), limits)
			if err != nil {
				t.Fatal(err)
			}
			if (got.Gap == "test_timeout_gap") != (kind == "command" || kind == "preflight") || got.TimeoutKind != kind || got.TimedOut != (kind != "canceled") || got.Completed || got.Passed {
				t.Fatalf("timeout = %+v", got)
			}
			if kind == "preflight" && (len(got.Command) != 0 || got.ExitCode != -1 || got.Diagnostic == "") {
				t.Fatalf("preflight deadline invented a test invocation: %+v", got)
			}
		})
	}
}

func TestSnapshotGoTestsPreserveFirstPackageTimeout(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "full.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	output := &goTestOutput{file: file, digest: sha256.New(), cancel: func() {}, limit: 1 << 20}
	commands := [][]string{
		{"/bin/sh", "-c", "printf 'ordinary failure\\n'; exit 1"},
		{"/bin/sh", "-c", "printf 'panic: test timed out after 50ms\\n'; exit 1"},
		{"/bin/sh", "-c", "exit 2"},
	}
	env := os.Environ()
	ctx := t.Context()
	executed, runErr := executeSnapshotGoTests(ctx, root, commands, [][]string{env, env, env}, output)
	got := summarizeSnapshotGoTests(ctx, ctx, &reviewSourceSnapshot{root: root}, []string{".", "./other", "./third"}, commands, executed, output, runErr)
	if len(executed) != 2 || !slices.Equal(got.Command, commands[1]) || got.ExitCode != 1 ||
		got.Gap != "test_timeout_gap" || got.TimeoutKind != "test" || got.Passed || got.Completed {
		t.Fatalf("package timeout changed across modules: %+v", got)
	}
}

func TestSnapshotGoTestsFailClosedOnOutputStorageLimit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go"), "#!/bin/sh\nprintf 'output exceeds storage budget'\nexit 0\n")
	if err := os.Chmod(filepath.Join(root, "go"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	limits := defaultGoTestLimits()
	limits.OutputBytes = 8
	got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, snapshotGoTestTargets{packages: []string{"."}}, replaceEnvironmentValue(os.Environ(), "PATH", root), filepath.Join(root, "full.log"), limits)
	if err == nil || got.Completed || got.Passed {
		t.Fatalf("incomplete durable output accepted: %+v, %v", got, err)
	}
}

func TestGoTestOutputDetectsSplitTimeoutAfterSummaryHead(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "full.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out := &goTestOutput{file: file, digest: sha256.New(), cancel: func() {}, limit: 1 << 20}
	for _, part := range []string{strings.Repeat("x", 160000) + "\npanic: test ti", "med out after 50ms\n"} {
		if _, err := out.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if !out.testTimeout {
		t.Fatal("split timeout marker after the old cap was lost")
	}
}

func TestSnapshotGoTestsRecordEmptyScopeWithoutExecutingGo(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	result, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, snapshotGoTestTargets{}, os.Environ(), filepath.Join(root, "output.log"), defaultGoTestLimits())
	if err != nil || result.Gap != "test_scope_empty" || result.Completed || len(result.Command) != 0 || len(result.Packages) != 0 || result.Diagnostic == "" {
		t.Fatalf("empty scope = %+v, %v", result, err)
	}
}

func TestSnapshotGoTestsGapOnExternalLocalDependencies(t *testing.T) {
	for _, mode := range []string{"workspace_use", "workspace_replace", "module_replace", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			parent := t.TempDir()
			frozen, external := filepath.Join(parent, "frozen"), filepath.Join(parent, "host")
			writeFile(t, filepath.Join(external, "go.mod"), "module example.test/external\n\ngo 1.22\n")
			writeFile(t, filepath.Join(external, "external.go"), "package external\n")
			module := "module example.test/review\n\ngo 1.22\n"
			switch mode {
			case "workspace_use":
				writeFile(t, filepath.Join(frozen, "go.work"), "go 1.22\nuse (\n.\n../host\n)\n")
			case "workspace_replace":
				writeFile(t, filepath.Join(frozen, "go.work"), "go 1.22\nuse .\nreplace example.test/external => ../host\n")
			case "module_replace":
				module += "replace example.test/external => ../host\n"
			case "symlink":
				writeFile(t, filepath.Join(frozen, "review.go"), "package review\n")
				if err := os.Symlink(external, filepath.Join(frozen, "local")); err != nil {
					t.Fatal(err)
				}
				module += "replace example.test/external => ./local\n"
			}
			writeFile(t, filepath.Join(frozen, "go.mod"), module+"require example.test/external v0.0.0\n")
			writeFile(t, filepath.Join(frozen, "review.go"), "package review\nimport _ \"example.test/external\"\n")
			env, err := sanitizedGoTestEnvironment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			snapshot := &reviewSourceSnapshot{root: frozen, identity: &repositorySnapshotManifest{}}
			result, err := runSnapshotGoTests(t.Context(), snapshot, snapshotGoTestTargets{packages: []string{"."}}, env, filepath.Join(parent, "output.log"), defaultGoTestLimits())
			if err != nil || result.Gap != "test_dependency_outside_snapshot" || result.Completed ||
				result.Passed || len(result.Command) != 0 || !strings.Contains(result.Diagnostic, "outside the snapshot") && !strings.Contains(result.Diagnostic, "escapes the snapshot") {
				t.Fatalf("external dependency was used or misclassified: %+v, %v", result, err)
			}
		})
	}
}
