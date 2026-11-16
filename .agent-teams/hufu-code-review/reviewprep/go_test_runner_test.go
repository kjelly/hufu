package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/v2"
	"os"
	"path/filepath"
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
			got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, []string{"./target"}, env, outputPath, defaultGoTestLimits())
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
	if limits.Package != 30*time.Minute || limits.Command < limits.Package+time.Minute || action < limits.Command+5*time.Minute || time.Duration(manifest.Spec.Timeout)*time.Second < action {
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
	got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, []string{"."}, env, outputPath, limits)
	if err != nil {
		t.Fatal(err)
	}
	if got.TimeoutKind != "test" || !got.TimedOut || got.Passed || got.Completed || got.ExitCode != 1 || got.OutputTruncated {
		t.Fatalf("runner alarm = %+v", got)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil || !bytes.Contains(data, []byte("panic: test timed out after 50ms")) {
		t.Fatalf("runner alarm output = %s, %v", data, err)
	}
}

func TestSnapshotGoTestsDistinguishCommandAndCallerTimeout(t *testing.T) {
	for _, kind := range []string{"command", "caller_deadline", "canceled"} {
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
			got, err := runSnapshotGoTests(ctx, &reviewSourceSnapshot{root: root}, []string{"."}, env, filepath.Join(root, "full.log"), limits)
			if err != nil {
				t.Fatal(err)
			}
			if got.TimeoutKind != kind || got.TimedOut != (kind != "canceled") || got.Completed || got.Passed {
				t.Fatalf("timeout = %+v", got)
			}
		})
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
	got, err := runSnapshotGoTests(t.Context(), &reviewSourceSnapshot{root: root}, []string{"."}, replaceEnvironmentValue(os.Environ(), "PATH", root), filepath.Join(root, "full.log"), limits)
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
