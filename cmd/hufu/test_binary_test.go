package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/kjelly/hufu/internal/providerproxy"
)

// cliBinaryCache owns one build for the whole test process, including failures.
// Its directory must outlive individual tests and their t.Cleanup callbacks.
type cliBinaryCache struct {
	once sync.Once
	root string
	path string
	err  error
}

var cliTestBinary cliBinaryCache

func TestMain(m *testing.M) {
	// In-process CLI fixtures use the same invocation-owned provider proxy
	// as production. Dispatch its child mode before test flag parsing.
	if len(os.Args) == 2 && os.Args[1] == providerproxy.ChildArg {
		os.Exit(providerproxy.RunChild(os.Stdin, os.Stdout))
	}
	code := m.Run()
	// Every consumer has finished, including parallel tests and their cleanups.
	if cliTestBinary.root != "" {
		if err := os.RemoveAll(cliTestBinary.root); err != nil {
			fmt.Fprintln(os.Stderr, "clean shared CLI test binary:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func buildTestBinary(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate CLI module root")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	path, err := cliTestBinary.build(t.Context(), moduleRoot)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// build publishes a path only after go build succeeds. sync.Once publishes the
// same diagnostic to all waiters; no caller can use a partial or stale binary.
func (binary *cliBinaryCache) build(ctx context.Context, moduleRoot string) (string, error) {
	binary.once.Do(func() {
		binary.root, binary.err = os.MkdirTemp(os.TempDir(), "hufu-cli-tests-")
		if binary.err != nil {
			binary.err = fmt.Errorf("create CLI test binary directory: %w", binary.err)
			return
		}
		name := "hufu"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		path := filepath.Join(binary.root, name)
		command := exec.CommandContext(ctx, "go", "build", "-p", "2", "-o", path, "./cmd/hufu")
		command.Dir = moduleRoot
		if output, err := command.CombinedOutput(); err != nil {
			binary.err = fmt.Errorf("build CLI test binary: %w\n%s", err, output)
			return
		}
		binary.path = path
	})
	return binary.path, binary.err
}
