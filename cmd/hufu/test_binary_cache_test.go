package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A tiny module exercises the real Go command without compiling another copy
// of hufu for every cache regression.
func cliBinaryFixture(t *testing.T, source string) string {
	t.Helper()
	root := t.TempDir()
	// Keep VCS discovery inside this fixture, independent of the host temp root.
	if output, err := exec.CommandContext(t.Context(), "git", "init", "--quiet", root).CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v\n%s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(root, "cmd", "hufu"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"go.mod":           "module cli-fixture\n\ngo 1.27.0\n",
		"cmd/hufu/main.go": source,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCLIBinaryCacheConcurrentCallers(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken=%t", broken), func(t *testing.T) {
			source := "package main\nfunc main() {}\n"
			if broken {
				source = "package main\nvar _ = missingCLIBuilderSymbol\n"
			}
			root := cliBinaryFixture(t, source)
			var cache cliBinaryCache
			t.Cleanup(func() {
				if err := os.RemoveAll(cache.root); err != nil {
					t.Error(err)
				}
			})
			type result struct {
				path string
				err  error
			}
			results := make([]result, 24)
			var wait sync.WaitGroup
			for i := range results {
				wait.Go(func() { results[i].path, results[i].err = cache.build(t.Context(), root) })
			}
			wait.Wait()
			first := results[0]
			for _, got := range results {
				if got.path != first.path || got.err != first.err {
					t.Fatalf("inconsistent cached result: %+v, first %+v", got, first)
				}
			}
			if broken {
				if first.path != "" || first.err == nil || !strings.Contains(first.err.Error(), "missingCLIBuilderSymbol") {
					t.Fatalf("lost build diagnostic: path=%q err=%v", first.path, first.err)
				}
				var exitErr *exec.ExitError
				if !errors.As(first.err, &exitErr) {
					t.Fatalf("lost original exit error: %v", first.err)
				}
			} else if first.err != nil || first.path == "" {
				t.Fatalf("build: %v", first.err)
			}
			// Invert the source outcome. A second build would now change the result.
			replacement := "package main\nvar _ = anotherMissingSymbol\n"
			if broken {
				replacement = "package main\nfunc main() {}\n"
			}
			if err := os.WriteFile(filepath.Join(root, "cmd", "hufu", "main.go"), []byte(replacement), 0o600); err != nil {
				t.Fatal(err)
			}
			path, err := cache.build(t.Context(), root)
			if path != first.path || err != first.err {
				t.Fatalf("rebuilt cached binary: %q, %v", path, err)
			}
		})
	}
}

func TestCLIBinaryProcessLifetime(t *testing.T) {
	// Independent test processes must own different directories. Each process
	// verifies the binary survives a consumer's t.Cleanup, then TestMain removes it.
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprintf("broken=%t", broken), func(t *testing.T) {
			source := "package main\nfunc main() {}\n"
			if broken {
				source = "package main\nvar _ = missingLifetimeSymbol\n"
			}
			root := cliBinaryFixture(t, source)
			commands := make([]*exec.Cmd, 2)
			outputs := make([]strings.Builder, len(commands))
			for i := range commands {
				commands[i] = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCLIBinaryLifetimeHelper$", "-test.v")
				commands[i].Env = append(os.Environ(), "HUFU_BINARY_FIXTURE="+root)
				commands[i].Stdout = &outputs[i]
				commands[i].Stderr = &outputs[i]
				if err := commands[i].Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// Reap every started helper even if another helper fails first.
					if commands[i].ProcessState == nil {
						_ = commands[i].Wait()
					}
				})
			}
			roots := make([]string, len(commands))
			for i, command := range commands {
				err := command.Wait()
				if broken {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || strings.Count(outputs[i].String(), "undefined: missingLifetimeSymbol") != 2 {
						t.Fatalf("helper lost repeated failure diagnostics: %v\n%s", err, outputs[i].String())
					}
				} else if err != nil {
					t.Fatalf("helper: %v\n%s", err, outputs[i].String())
				}
				for _, line := range strings.Split(outputs[i].String(), "\n") {
					if value, ok := strings.CutPrefix(line, "CLI_BINARY_ROOT="); ok {
						roots[i] = value
					}
				}
				if roots[i] == "" {
					t.Fatalf("missing helper root: %s", outputs[i].String())
				}
				if _, err := os.Stat(roots[i]); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("TestMain did not remove %q: %v", roots[i], err)
				}
			}
			if roots[0] == roots[1] {
				t.Fatalf("processes shared binary directory: %q", roots[0])
			}
		})
	}
}

func TestCLIBinaryLifetimeHelper(t *testing.T) {
	root := os.Getenv("HUFU_BINARY_FIXTURE")
	if root == "" {
		return
	}
	var first string
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			path, err := cliTestBinary.build(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if first == "" {
				first = path
			} else if first != path {
				t.Fatalf("binary changed: %q != %q", first, path)
			}
			t.Cleanup(func() {
				if _, err := os.Stat(path); err != nil {
					t.Errorf("binary removed before consumer cleanup: %v", err)
				}
			})
		})
	}
	fmt.Println("CLI_BINARY_ROOT=" + cliTestBinary.root)
}
