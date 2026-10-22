package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var cliTestBinary struct {
	once sync.Once
	root string
	path string
	err  error
}

func TestMain(m *testing.M) {
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
	cliTestBinary.once.Do(func() {
		cliTestBinary.root, cliTestBinary.err = os.MkdirTemp(os.TempDir(), "hufu-cli-tests-")
		if cliTestBinary.err != nil {
			return
		}
		_, source, _, ok := runtime.Caller(0)
		if !ok {
			cliTestBinary.err = errors.New("locate CLI module root")
			return
		}
		cliTestBinary.path = filepath.Join(cliTestBinary.root, "hufu")
		command := exec.CommandContext(t.Context(), "go", "build", "-p", "2", "-o", cliTestBinary.path, "./cmd/hufu")
		command.Dir = filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
		if output, err := command.CombinedOutput(); err != nil {
			cliTestBinary.err = fmt.Errorf("build CLI test binary: %w\n%s", err, output)
		}
	})
	if cliTestBinary.err != nil {
		t.Fatal(cliTestBinary.err)
	}
	return cliTestBinary.path
}
