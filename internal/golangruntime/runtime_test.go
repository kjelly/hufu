package golangruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == ChildArg {
		os.Exit(RunChild(os.Args[2], os.Args[3], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func TestExecuteTrustedProgramMayUseFilesAndExternalProcesses(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, `package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
)

func Run(ctx context.Context, _ io.Reader, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "printf trusted > \"$HUFU_TEST_OUTPUT\"")
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil { return err }
	data, err := os.ReadFile(os.Getenv("HUFU_TEST_OUTPUT"))
	if err != nil { return err }
	return json.NewEncoder(out).Encode(map[string]string{"value": string(data)})
}
`)
	program, err := Prepare(teamDir, "./action")
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "result.txt")
	result, err := Execute(t.Context(), "", program, []byte(`{}`), append(os.Environ(), "HUFU_TEST_OUTPUT="+output), 1024, 1024)
	if err != nil {
		t.Fatalf("Execute: %v; stderr=%s", err, result.Stderr)
	}
	if got := strings.TrimSpace(string(result.Stdout)); got != `{"value":"trusted"}` {
		t.Fatalf("stdout = %q", got)
	}
	if data, err := os.ReadFile(output); err != nil || string(data) != "trusted" {
		t.Fatalf("trusted action file = %q, %v", data, err)
	}
}

func TestExecuteRejectsSourceChangedAfterPrepare(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, minimalSource(`return nil`))
	program, err := Prepare(teamDir, "action")
	if err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, minimalSource(`_, _ = io.WriteString(out, "changed"); return nil`))
	result, err := Execute(t.Context(), "", program, nil, os.Environ(), 1024, 1024)
	if err == nil || !strings.Contains(string(result.Stderr), "source changed after preflight") {
		t.Fatalf("Execute error = %v, stderr=%q", err, result.Stderr)
	}
}

func TestExecuteHonorsCancellation(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, `package main
import ("context"; "io"; "os/exec")
func Run(ctx context.Context, _ io.Reader, _ io.Writer) error {
	return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 10").Run()
}
`)
	program, err := Prepare(teamDir, "action")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = Execute(ctx, "", program, nil, os.Environ(), 1024, 1024)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestPrepareRejectsEscapesAndSymlinks(t *testing.T) {
	teamDir := t.TempDir()
	outside := t.TempDir()
	writeSource(t, outside, minimalSource("return nil"))
	if _, err := Prepare(teamDir, outside); err == nil {
		t.Fatal("Prepare accepted source outside team directory")
	}
	link := filepath.Join(teamDir, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(teamDir, link); err == nil {
		t.Fatal("Prepare accepted a symlink source")
	}
}

func TestPrepareRejectsWrongRunSignature(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, "package main\nfunc Run() error { return nil }\n")
	if _, err := Prepare(teamDir, "action"); err == nil || !strings.Contains(err.Error(), "must have signature") {
		t.Fatalf("Prepare error = %v", err)
	}
}

func writeSource(t *testing.T, directory, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func minimalSource(body string) string {
	return "package main\nimport (\"context\"; \"io\")\nfunc Run(_ context.Context, _ io.Reader, out io.Writer) error { " + body + " }\n"
}
