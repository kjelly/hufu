package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSnapshotGoTestExecutionCompletion(t *testing.T) {
	for _, test := range []struct {
		name      string
		script    string
		cancel    bool
		completed bool
		passed    bool
		exitCode  int
	}{
		{name: "pass", script: "exit 0", completed: true, passed: true},
		{name: "target_failure", script: "printf 'FAIL: target test failed\\n'; exit 1", completed: true, exitCode: 1},
		{name: "invocation_error", script: "exit 2", exitCode: 2},
		{name: "canceled", script: "exit 0", cancel: true, exitCode: -1},
		{name: "missing_executable", exitCode: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root)
			if test.script != "" {
				if err := os.WriteFile(filepath.Join(root, "go"), []byte("#!/bin/sh\n"+test.script+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			got := runSnapshotGoTests(ctx, &reviewSourceSnapshot{root: root}, []string{"./target"}, os.Environ())
			if got.Completed != test.completed || got.Passed != test.passed || got.ExitCode != test.exitCode {
				t.Fatalf("execution = %+v", got)
			}
			if test.name == "target_failure" && got.Diagnostic == "" {
				t.Fatal("target failure lost its diagnostic")
			}
		})
	}
}
