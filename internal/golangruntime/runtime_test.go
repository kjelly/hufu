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

func TestExecuteTrustedProgramUsesSharedArtifactCoverageTypes(t *testing.T) {
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
	"github.com/kjelly/hufu/internal/tools"
)
func Run(_ context.Context, in io.Reader, out io.Writer) error {
	var reads []tools.ArtifactReadCoverage
	if err := json.NewDecoder(in).Decode(&reads); err != nil { return err }
	reads[0].Ranges = append(reads[0].Ranges, tools.ArtifactByteRange{Start: 4, End: 6})
	return json.NewEncoder(out).Encode(reads)
}
`)
	program, err := Prepare(teamDir, "action")
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`[{"artifact_id":"source","sha256":"digest","byte_size":6,"ranges":[{"start":0,"end":4}]}]`)
	result, err := Execute(t.Context(), "", program, input, os.Environ(), 1024, 1024)
	if err != nil {
		t.Fatalf("Execute: %v; stderr=%s", err, result.Stderr)
	}
	want := `[{"artifact_id":"source","sha256":"digest","byte_size":6,"ranges":[{"start":0,"end":4},{"start":4,"end":6}]}]`
	if got := strings.TrimSpace(string(result.Stdout)); got != want {
		t.Fatalf("stdout=%q want %q", got, want)
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

func TestExecuteTrustedProgramSupervisesDescendantCommands(t *testing.T) {
	teamDir := t.TempDir()
	source := filepath.Join(teamDir, "action")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSource(t, source, `package main
import (
 "context"
 "io"
 "os"
 "os/exec"
 "time"
 "github.com/kjelly/hufu/internal/processutil"
)
func Run(ctx context.Context, _ io.Reader, out io.Writer) error {
 ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
 defer cancel()
 cmd := exec.CommandContext(ctx, "/bin/sh", "-c", "(sleep 0.5; printf orphan > \"$HUFU_TEST_ORPHAN\") & wait")
 cmd.Env = os.Environ()
 if err := processutil.RunContextCommand(cmd, time.Second); err == nil { return io.EOF }
 _, err := io.WriteString(out, "canceled")
 return err
}
`)
	program, err := Prepare(teamDir, "action")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "orphan")
	result, err := Execute(t.Context(), "", program, nil, append(os.Environ(), "HUFU_TEST_ORPHAN="+marker), 1024, 1024)
	if err != nil || string(result.Stdout) != "canceled" {
		t.Fatalf("supervised action: %v; %s; %s", err, result.Stdout, result.Stderr)
	}
	// A surviving grandchild would publish this marker after the action returns.
	select {
	case <-time.After(600 * time.Millisecond):
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant survived cancellation: %v", err)
	}
}

func TestExecuteNormalizerUsesBoundedDataOnlyChild(t *testing.T) {
	if !NormalizerSupported() {
		t.Skip("bounded normalizers require Linux")
	}
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	for _, tc := range []struct {
		name, source, stderr string
		want                 string
		limit                int
		hang, truncated      bool
	}{
		{name: "echo", source: strings.Replace(minimalSource("_, err := io.Copy(out, in); return err"), "_ io.Reader", "in io.Reader", 1), want: `{"draft":true}`, limit: 1024},
		{name: "output limit", source: strings.Replace(minimalSource("_, err := io.Copy(out, in); return err"), "_ io.Reader", "in io.Reader", 1), want: `{"d`, limit: 3, truncated: true},
		{name: "deadline", source: minimalSource("for {}"), limit: 1024, hang: true},
		{name: "filesystem import", source: `package main
import ("context"; "io"; "os")
func Run(context.Context, io.Reader, io.Writer) error { return os.ErrNotExist }
`, limit: 1024, stderr: "os"},
		{name: "parser requires source bytes", source: `package main
import ("context"; "io"; "go/parser"; "go/token")
func Run(_ context.Context, _ io.Reader, _ io.Writer) error { _, err := parser.ParseFile(token.NewFileSet(), "sample.go", nil, 0); return err }
`, limit: 1024, stderr: "must pass source bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "action")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			writeSource(t, source, tc.source)
			program, err := Prepare(root, "action")
			if err != nil {
				t.Fatal(err)
			}
			timeout := 5 * time.Second
			if tc.hang {
				timeout = 250 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			result, err := ExecuteNormalizer(ctx, "", program, []byte(`{"draft":true}`), tc.limit)
			switch {
			case tc.hang:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline failure = %v, stderr = %s", err, result.Stderr)
				}
			case tc.stderr != "":
				if err == nil || !strings.Contains(string(result.Stderr), tc.stderr) {
					t.Fatalf("child allowed unsupported input: %v, stderr = %s", err, result.Stderr)
				}
			default:
				if err != nil || string(result.Stdout) != tc.want || result.StdoutTruncated != tc.truncated {
					t.Fatalf("child output = %q, truncated = %v, err = %v, stderr = %s", result.Stdout, result.StdoutTruncated, err, result.Stderr)
				}
			}
			entries, err := os.ReadDir(scratch)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "hufu-normalizer-") {
					t.Fatal("normalizer staging directory leaked")
				}
			}
		})
	}
}
