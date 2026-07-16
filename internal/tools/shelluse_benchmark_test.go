//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// ============================================================================
// Shell-Use Benchmark Suite for hufu
// ============================================================================
// Inspired by Microsoft's shell-use benchmark, this test suite evaluates
// hufu's bash tool and companion tools (grep, glob, view, write, edit, ls,
// math) through a structured task → setup → execute → verify methodology.
//
// Each benchmark task has:
//   - Category: the type of shell operation
//   - Prompt: the natural-language instruction an LLM would receive
//   - Setup: commands to create the initial state
//   - Command: the shell command to execute via hufu's bash tool
//   - Verify: a function that checks the resulting state/output
//   - Expected: human-readable description of the expected outcome
//
// Categories cover: file operations, text processing, pipelines, search,
// archiving, environment, permissions, JSON/YAML, system info, and more.
// ============================================================================

// --- Benchmark task definition ---

type shellUseCategory string

const (
	catFileOps      shellUseCategory = "file_operations"
	catTextProcess  shellUseCategory = "text_processing"
	catPipelines    shellUseCategory = "pipelines"
	catSearch       shellUseCategory = "search"
	catArchiving    shellUseCategory = "archiving"
	catEnvironment  shellUseCategory = "environment"
	catPermissions  shellUseCategory = "permissions"
	catJSON         shellUseCategory = "json_processing"
	catSystemInfo   shellUseCategory = "system_info"
	catConditionals shellUseCategory = "conditionals"
	catVariables    shellUseCategory = "variable_expansion"
	catMultiStep    shellUseCategory = "multi_step"
)

type shellUseTask struct {
	ID       string
	Category shellUseCategory
	Prompt   string // What an LLM would be asked
	Setup    func(t *testing.T, dir string)
	Command  string
	Verify   func(t *testing.T, dir string, resp fantasy.ToolResponse)
	Expected string
}

// --- Helpers ---

func shellUseCtx(dir string) (context.Context, ToolConfig) {
	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"bash"})
	cfg := ApplyOptions([]ToolOption{
		WithWorkDir(dir),
		WithAllowedPaths([]string{dir}),
	})
	return ctx, cfg
}

func runBash(t *testing.T, ctx context.Context, cfg ToolConfig, command string) fantasy.ToolResponse {
	t.Helper()
	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID:    "bench",
		Name:  "bash",
		Input: fmt.Sprintf(`{"command": %q}`, command),
	}, cfg)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	return resp
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func assertContains(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("expected output to contain %q, got: %s", want, got)
	}
}

func assertNotContains(t *testing.T, got, avoid string) {
	t.Helper()
	if strings.Contains(got, avoid) {
		t.Errorf("expected output to NOT contain %q, got: %s", avoid, got)
	}
}

func assertNoError(t *testing.T, resp fantasy.ToolResponse) {
	t.Helper()
	if resp.IsError {
		t.Errorf("expected success, got error: %s", resp.Content)
	}
}

func assertError(t *testing.T, resp fantasy.ToolResponse) {
	t.Helper()
	if !resp.IsError {
		t.Errorf("expected error, got success: %s", resp.Content)
	}
}

func assertFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected file %s to exist: %v", path, err)
	}
}

func assertFileNotExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file %s to NOT exist, but it does", path)
	}
}

func assertFileContent(t *testing.T, path, expected string) {
	t.Helper()
	got := readFile(t, path)
	if strings.TrimSpace(got) != strings.TrimSpace(expected) {
		t.Errorf("file %s content mismatch:\nwant: %q\ngot:  %q", path, expected, got)
	}
}

// --- Benchmark task definitions ---

func shellUseTasks() []shellUseTask {
	return []shellUseTask{
		// ================================================================
		// FILE OPERATIONS
		// ================================================================
		{
			ID:       "file-001",
			Category: catFileOps,
			Prompt:   "Create a file named hello.txt with the content 'Hello, World!'",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "echo 'Hello, World!' | tee hello.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "hello.txt"))
				assertFileContent(t, filepath.Join(dir, "hello.txt"), "Hello, World!")
			},
			Expected: "hello.txt exists with 'Hello, World!'",
		},
		{
			ID:       "file-002",
			Category: catFileOps,
			Prompt:   "Copy file.txt to backup.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "file.txt"), "original content\n")
			},
			Command: "cp file.txt backup.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "backup.txt"))
				assertFileContent(t, filepath.Join(dir, "backup.txt"), "original content")
			},
			Expected: "backup.txt is a copy of file.txt",
		},
		{
			ID:       "file-003",
			Category: catFileOps,
			Prompt:   "Move old.log to archive/old.log",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "old.log"), "log data\n")
				os.MkdirAll(filepath.Join(dir, "archive"), 0o755)
			},
			Command: "mv old.log archive/old.log",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "archive", "old.log"))
				assertFileNotExists(t, filepath.Join(dir, "old.log"))
			},
			Expected: "old.log moved to archive/old.log",
		},
		{
			ID:       "file-004",
			Category: catFileOps,
			Prompt:   "Delete the file temp.tmp",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "temp.tmp"), "temporary\n")
			},
			Command: "rm temp.tmp",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileNotExists(t, filepath.Join(dir, "temp.tmp"))
			},
			Expected: "temp.tmp is deleted",
		},
		{
			ID:       "file-005",
			Category: catFileOps,
			Prompt:   "Create a directory structure: project/src, project/tests, project/docs",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "mkdir -p project/src project/tests project/docs",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				for _, sub := range []string{"src", "tests", "docs"} {
					assertFileExists(t, filepath.Join(dir, "project", sub))
				}
			},
			Expected: "project/src, project/tests, project/docs directories exist",
		},
		{
			ID:       "file-006",
			Category: catFileOps,
			Prompt:   "Append 'new entry' to the end of log.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "log.txt"), "line1\nline2\n")
			},
			Command: "echo 'new entry' | tee -a log.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				content := readFile(t, filepath.Join(dir, "log.txt"))
				if !strings.Contains(content, "new entry") {
					t.Errorf("log.txt should contain 'new entry', got: %s", content)
				}
				if !strings.HasPrefix(content, "line1\n") {
					t.Errorf("log.txt should still contain original content, got: %s", content)
				}
			},
			Expected: "log.txt has 'new entry' appended after original content",
		},
		{
			ID:       "file-007",
			Category: catFileOps,
			Prompt:   "Count the number of lines in data.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "data.txt"), "line1\nline2\nline3\nline4\nline5\n")
			},
			Command: "wc -l data.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "5")
			},
			Expected: "Output contains line count 5",
		},
		{
			ID:       "file-008",
			Category: catFileOps,
			Prompt:   "Create a symbolic link from link.txt to target.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "target.txt"), "target content\n")
			},
			Command: "ln -s target.txt link.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				linkPath := filepath.Join(dir, "link.txt")
				info, err := os.Lstat(linkPath)
				if err != nil {
					t.Errorf("symlink link.txt should exist: %v", err)
				} else if info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("link.txt should be a symlink")
				}
			},
			Expected: "link.txt is a symlink to target.txt",
		},

		// ================================================================
		// TEXT PROCESSING
		// ================================================================
		{
			ID:       "text-001",
			Category: catTextProcess,
			Prompt:   "Sort the lines in unsorted.txt and save to sorted.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "unsorted.txt"), "banana\napple\ncherry\ndate\n")
			},
			Command: "sort unsorted.txt | tee sorted.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				content := readFile(t, filepath.Join(dir, "sorted.txt"))
				lines := strings.Split(strings.TrimSpace(content), "\n")
				expected := []string{"apple", "banana", "cherry", "date"}
				if len(lines) != len(expected) {
					t.Errorf("expected %d lines, got %d", len(expected), len(lines))
					return
				}
				for i, l := range lines {
					if strings.TrimSpace(l) != expected[i] {
						t.Errorf("line %d: want %q, got %q", i, expected[i], l)
					}
				}
			},
			Expected: "sorted.txt contains lines in alphabetical order",
		},
		{
			ID:       "text-002",
			Category: catTextProcess,
			Prompt:   "Extract the 2nd column from CSV file data.csv",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "data.csv"), "name,age,city\nAlice,30,NYC\nBob,25,LA\n")
			},
			Command: "cut -d',' -f2 data.csv",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "age")
				assertContains(t, resp.Content, "30")
				assertContains(t, resp.Content, "25")
			},
			Expected: "Output contains the 2nd column values",
		},
		{
			ID:       "text-003",
			Category: catTextProcess,
			Prompt:   "Replace all occurrences of 'foo' with 'bar' in input.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "input.txt"), "foo bar foo\nfoo foo\nbar\n")
			},
			Command: "sed 's/foo/bar/g' input.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "bar bar bar")
				assertNotContains(t, resp.Content, "foo")
			},
			Expected: "All 'foo' replaced with 'bar'",
		},
		{
			ID:       "text-004",
			Category: catTextProcess,
			Prompt:   "Find unique lines in duplicates.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "duplicates.txt"), "apple\napple\nbanana\nbanana\ncherry\n")
			},
			Command: "sort duplicates.txt | uniq",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "apple")
				assertContains(t, resp.Content, "banana")
				assertContains(t, resp.Content, "cherry")
				// Count occurrences of each - should appear once
				if strings.Count(resp.Content, "apple") != 1 {
					t.Errorf("apple should appear once, got %d", strings.Count(resp.Content, "apple"))
				}
			},
			Expected: "Output has only unique lines",
		},
		{
			ID:       "text-005",
			Category: catTextProcess,
			Prompt:   "Convert all text in upper.txt to lowercase",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "upper.txt"), "HELLO WORLD\nFOO BAR\n")
			},
			Command: "cat upper.txt | tr 'A-Z' 'a-z'",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "hello world")
				assertContains(t, resp.Content, "foo bar")
				assertNotContains(t, resp.Content, "HELLO")
			},
			Expected: "All text converted to lowercase",
		},
		{
			ID:       "text-006",
			Category: catTextProcess,
			Prompt:   "Show the first 3 lines of longfile.txt",
			Setup: func(t *testing.T, dir string) {
				var sb strings.Builder
				for i := 1; i <= 10; i++ {
					sb.WriteString(fmt.Sprintf("line %d\n", i))
				}
				writeFile(t, filepath.Join(dir, "longfile.txt"), sb.String())
			},
			Command: "head -3 longfile.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "line 1")
				assertContains(t, resp.Content, "line 2")
				assertContains(t, resp.Content, "line 3")
				assertNotContains(t, resp.Content, "line 4")
			},
			Expected: "Only first 3 lines shown",
		},
		{
			ID:       "text-007",
			Category: catTextProcess,
			Prompt:   "Show the last 2 lines of longfile.txt",
			Setup: func(t *testing.T, dir string) {
				var sb strings.Builder
				for i := 1; i <= 10; i++ {
					sb.WriteString(fmt.Sprintf("line %d\n", i))
				}
				writeFile(t, filepath.Join(dir, "longfile.txt"), sb.String())
			},
			Command: "tail -2 longfile.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "line 9")
				assertContains(t, resp.Content, "line 10")
				assertNotContains(t, resp.Content, "line 8")
			},
			Expected: "Only last 2 lines shown",
		},
		{
			ID:       "text-008",
			Category: catTextProcess,
			Prompt:   "Count occurrences of each word in words.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "words.txt"), "apple banana apple cherry banana apple\n")
			},
			Command: "cat words.txt | tr ' ' '\\n' | sort | uniq -c | sort -rn",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// apple appears 3 times, banana 2 times, cherry 1 time
				assertContains(t, resp.Content, "3 apple")
				assertContains(t, resp.Content, "2 banana")
				assertContains(t, resp.Content, "1 cherry")
			},
			Expected: "Word counts: apple=3, banana=2, cherry=1",
		},

		// ================================================================
		// PIPELINES
		// ================================================================
		{
			ID:       "pipe-001",
			Category: catPipelines,
			Prompt:   "Find all .go files and count them",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "main.go"), "package main\n")
				writeFile(t, filepath.Join(dir, "util.go"), "package util\n")
				writeFile(t, filepath.Join(dir, "readme.md"), "# README\n")
				writeFile(t, filepath.Join(dir, "sub", "helper.go"), "package sub\n")
			},
			Command: "find . -name '*.go' | wc -l",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "3")
			},
			Expected: "3 .go files found",
		},
		{
			ID:       "pipe-002",
			Category: catPipelines,
			Prompt:   "List all files sorted by size, largest first",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "small.txt"), "a\n")
				writeFile(t, filepath.Join(dir, "large.txt"), strings.Repeat("x", 1000)+"\n")
				writeFile(t, filepath.Join(dir, "medium.txt"), strings.Repeat("y", 100)+"\n")
			},
			Command: "ls -lS *.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// large.txt should appear before small.txt
				largeIdx := strings.Index(resp.Content, "large.txt")
				smallIdx := strings.Index(resp.Content, "small.txt")
				if largeIdx < 0 || smallIdx < 0 {
					t.Errorf("expected both large.txt and small.txt in output")
					return
				}
				if largeIdx > smallIdx {
					t.Errorf("large.txt should appear before small.txt (sorted by size)")
				}
			},
			Expected: "Files listed largest first",
		},
		{
			ID:       "pipe-003",
			Category: catPipelines,
			Prompt:   "Find lines containing 'error' in log files and count them",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "app.log"), "info: started\nerror: failed\ninfo: done\nerror: timeout\n")
				writeFile(t, filepath.Join(dir, "sys.log"), "error: disk full\ninfo: ok\n")
			},
			Command: "grep 'error' *.log | wc -l",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "3")
			},
			Expected: "3 error lines found",
		},
		{
			ID:       "pipe-004",
			Category: catPipelines,
			Prompt:   "Extract all unique IP addresses from access.log",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "access.log"),
					"192.168.1.1 - GET /\n10.0.0.1 - POST /api\n192.168.1.1 - GET /css\n10.0.0.2 - GET /js\n")
			},
			Command: "grep -oE '[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+' access.log | sort -u",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "10.0.0.1")
				assertContains(t, resp.Content, "10.0.0.2")
				assertContains(t, resp.Content, "192.168.1.1")
				// 192.168.1.1 should appear only once
				if strings.Count(resp.Content, "192.168.1.1") != 1 {
					t.Errorf("192.168.1.1 should appear once (unique), got %d",
						strings.Count(resp.Content, "192.168.1.1"))
				}
			},
			Expected: "3 unique IP addresses",
		},
		{
			ID:       "pipe-005",
			Category: catPipelines,
			Prompt:   "Show the 3 most common words in document.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "document.txt"),
					"the quick brown fox the lazy dog the end\n")
			},
			Command: "cat document.txt | tr ' ' '\\n' | sort | uniq -c | sort -rn | head -3",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "the")
			},
			Expected: "Top 3 words shown with counts",
		},

		// ================================================================
		// SEARCH
		// ================================================================
		{
			ID:       "search-001",
			Category: catSearch,
			Prompt:   "Find all files modified in the last minute",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "recent.txt"), "recent\n")
				writeFile(t, filepath.Join(dir, "old.txt"), "old\n")
				// Set old.txt's modification time to 1 hour ago
				oldTime := time.Now().Add(-1 * time.Hour)
				os.Chtimes(filepath.Join(dir, "old.txt"), oldTime, oldTime)
			},
			Command: "find . -name '*.txt' -mmin -5",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "recent.txt")
				assertNotContains(t, resp.Content, "old.txt")
			},
			Expected: "Only recently modified files found",
		},
		{
			ID:       "search-002",
			Category: catSearch,
			Prompt:   "Search for the pattern 'TODO' in all .go files",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "main.go"), "package main\n// TODO: fix this\nfunc main() {}\n")
				writeFile(t, filepath.Join(dir, "util.go"), "package util\n// TODO: refactor\n")
				writeFile(t, filepath.Join(dir, "readme.md"), "# TODO list\n")
			},
			Command: "grep -n 'TODO' *.go",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "TODO")
				assertContains(t, resp.Content, "main.go")
				assertContains(t, resp.Content, "util.go")
				assertNotContains(t, resp.Content, "readme.md")
			},
			Expected: "TODO found in .go files with line numbers",
		},
		{
			ID:       "search-003",
			Category: catSearch,
			Prompt:   "Find all empty files in the current directory tree",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "empty1.txt"), "")
				writeFile(t, filepath.Join(dir, "data.txt"), "data\n")
				writeFile(t, filepath.Join(dir, "sub", "empty2.txt"), "")
			},
			Command: "find . -type f -empty",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "empty1.txt")
				assertContains(t, resp.Content, "empty2.txt")
				assertNotContains(t, resp.Content, "data.txt")
			},
			Expected: "Only empty files found",
		},
		{
			ID:       "search-004",
			Category: catSearch,
			Prompt:   "Case-insensitive search for 'warning' in log.txt",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "log.txt"),
					"WARNING: high temp\nInfo: ok\nwarning: low memory\nError: crash\n")
			},
			Command: "grep -in 'warning' log.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "WARNING: high temp")
				assertContains(t, resp.Content, "warning: low memory")
				assertNotContains(t, resp.Content, "Info: ok")
			},
			Expected: "Both 'WARNING' and 'warning' lines found",
		},
		{
			ID:       "search-005",
			Category: catSearch,
			Prompt:   "Find files larger than 500 bytes",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "big.bin"), strings.Repeat("x", 1000))
				writeFile(t, filepath.Join(dir, "small.bin"), strings.Repeat("x", 100))
			},
			Command: "find . -type f -size +500c",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "big.bin")
				assertNotContains(t, resp.Content, "small.bin")
			},
			Expected: "Only files > 500 bytes found",
		},

		// ================================================================
		// ARCHIVING
		// ================================================================
		{
			ID:       "archive-001",
			Category: catArchiving,
			Prompt:   "Create a tar.gz archive of the src/ directory",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "src", "a.txt"), "a\n")
				writeFile(t, filepath.Join(dir, "src", "b.txt"), "b\n")
			},
			Command: "tar czf archive.tar.gz src/",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "archive.tar.gz"))
				// Verify it's a valid gzip
				info, err := os.Stat(filepath.Join(dir, "archive.tar.gz"))
				if err != nil {
					t.Errorf("archive.tar.gz should exist: %v", err)
				} else if info.Size() < 50 {
					t.Errorf("archive.tar.gz seems too small: %d bytes", info.Size())
				}
			},
			Expected: "archive.tar.gz created from src/",
		},
		{
			ID:       "archive-002",
			Category: catArchiving,
			Prompt:   "List the contents of archive.tar.gz without extracting",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "data", "file1.txt"), "content1\n")
				writeFile(t, filepath.Join(dir, "data", "file2.txt"), "content2\n")
				// Create archive using exec (not the bash tool) to set up state
				cmd := exec.Command("tar", "czf", filepath.Join(dir, "test.tar.gz"), "-C", dir, "data")
				if err := cmd.Run(); err != nil {
					t.Fatalf("setup: tar create failed: %v", err)
				}
			},
			Command: "tar tzf test.tar.gz",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "data/")
				assertContains(t, resp.Content, "file1.txt")
				assertContains(t, resp.Content, "file2.txt")
			},
			Expected: "Archive contents listed",
		},
		{
			ID:       "archive-003",
			Category: catArchiving,
			Prompt:   "Extract archive.tar.gz to the extracted/ directory",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "files", "hello.txt"), "hello\n")
				cmd := exec.Command("tar", "czf", filepath.Join(dir, "extract.tar.gz"), "-C", dir, "files")
				if err := cmd.Run(); err != nil {
					t.Fatalf("setup: tar create failed: %v", err)
				}
				os.MkdirAll(filepath.Join(dir, "extracted"), 0o755)
			},
			Command: "tar xzf extract.tar.gz -C extracted/",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "extracted", "files", "hello.txt"))
			},
			Expected: "Archive extracted to extracted/",
		},
		{
			ID:       "archive-004",
			Category: catArchiving,
			Prompt:   "Compress file.txt using gzip",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "compress.txt"), strings.Repeat("compressible text ", 50))
			},
			Command: "gzip compress.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "compress.txt.gz"))
				assertFileNotExists(t, filepath.Join(dir, "compress.txt"))
			},
			Expected: "compress.txt replaced by compress.txt.gz",
		},

		// ================================================================
		// ENVIRONMENT
		// ================================================================
		{
			ID:       "env-001",
			Category: catEnvironment,
			Prompt:   "Print the current working directory",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "pwd",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, dir)
			},
			Expected: "pwd output matches working directory",
		},
		{
			ID:       "env-002",
			Category: catEnvironment,
			Prompt:   "List all environment variables",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "env",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// PATH should be in the output
				assertContains(t, resp.Content, "PATH=")
			},
			Expected: "Environment variables listed",
		},
		{
			ID:       "env-003",
			Category: catEnvironment,
			Prompt:   "Print the value of HOME variable",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "echo $HOME",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				home := os.Getenv("HOME")
				if home != "" {
					assertContains(t, resp.Content, home)
				}
			},
			Expected: "HOME variable value printed",
		},
		{
			ID:       "env-004",
			Category: catEnvironment,
			Prompt:   "Print the current user name",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "whoami",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// Just verify it returns something non-empty
				if strings.TrimSpace(resp.Content) == "" {
					t.Errorf("whoami should return a non-empty username")
				}
			},
			Expected: "Current username printed",
		},
		{
			ID:       "env-005",
			Category: catEnvironment,
			Prompt:   "Set a variable and use it in a command",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "MYVAR=test123 && echo $MYVAR",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "test123")
			},
			Expected: "Variable set and used",
		},

		// ================================================================
		// PERMISSIONS
		// ================================================================
		{
			ID:       "perm-001",
			Category: catPermissions,
			Prompt:   "Make script.sh executable",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "script.sh"), "#!/bin/bash\necho hello\n")
			},
			Command: "chmod +x script.sh",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				info, err := os.Stat(filepath.Join(dir, "script.sh"))
				if err != nil {
					t.Fatalf("stat script.sh: %v", err)
				}
				if info.Mode()&0o100 == 0 {
					t.Errorf("script.sh should be executable, mode: %v", info.Mode())
				}
			},
			Expected: "script.sh is now executable",
		},
		{
			ID:       "perm-002",
			Category: catPermissions,
			Prompt:   "Set file permissions to read-only (444)",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "readonly.txt"), "content\n")
			},
			Command: "chmod 444 readonly.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				info, err := os.Stat(filepath.Join(dir, "readonly.txt"))
				if err != nil {
					t.Fatalf("stat: %v", err)
				}
				mode := info.Mode().Perm()
				if mode != 0o444 {
					t.Errorf("expected 0444, got %o", mode)
				}
			},
			Expected: "File is read-only (444)",
		},
		{
			ID:       "perm-003",
			Category: catPermissions,
			Prompt:   "Create a file with specific permissions 600",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "touch secret.txt && chmod 600 secret.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				info, err := os.Stat(filepath.Join(dir, "secret.txt"))
				if err != nil {
					t.Fatalf("stat: %v", err)
				}
				mode := info.Mode().Perm()
				if mode != 0o600 {
					t.Errorf("expected 0600, got %o", mode)
				}
			},
			Expected: "File created with 600 permissions",
		},

		// ================================================================
		// JSON PROCESSING
		// ================================================================
		{
			ID:       "json-001",
			Category: catJSON,
			Prompt:   "Parse JSON and extract the 'name' field",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "data.json"), `{"name": "Alice", "age": 30, "city": "NYC"}`+"\n")
			},
			Command: "python3 -c \"import json; print(json.load(open('data.json'))['name'])\"",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "Alice")
			},
			Expected: "Name field 'Alice' extracted",
		},
		{
			ID:       "json-002",
			Category: catJSON,
			Prompt:   "Create a JSON file with user data",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "echo '{\"user\": \"bob\", \"active\": true}' | tee user.json",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				content := readFile(t, filepath.Join(dir, "user.json"))
				var data map[string]interface{}
				if err := json.Unmarshal([]byte(content), &data); err != nil {
					t.Errorf("user.json is not valid JSON: %v", err)
				}
				if data["user"] != "bob" {
					t.Errorf("expected user=bob, got %v", data["user"])
				}
			},
			Expected: "Valid JSON file created",
		},
		{
			ID:       "json-003",
			Category: catJSON,
			Prompt:   "Extract all names from a JSON array",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "users.json"),
					`[{"name":"Alice"},{"name":"Bob"},{"name":"Charlie"}]`+"\n")
			},
			Command: "python3 -c \"import json; [print(u['name']) for u in json.load(open('users.json'))]\"",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "Alice")
				assertContains(t, resp.Content, "Bob")
				assertContains(t, resp.Content, "Charlie")
			},
			Expected: "All names extracted from JSON array",
		},

		// ================================================================
		// SYSTEM INFO
		// ================================================================
		{
			ID:       "sys-001",
			Category: catSystemInfo,
			Prompt:   "Show system kernel name and machine hardware name",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "uname -m",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				if strings.TrimSpace(resp.Content) == "" {
					t.Errorf("uname -m should return hardware name")
				}
			},
			Expected: "Machine hardware name printed",
		},
		{
			ID:       "sys-002",
			Category: catSystemInfo,
			Prompt:   "Show disk usage of the current directory",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "data.bin"), strings.Repeat("x", 10000))
			},
			Command: "du -sh .",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// Should contain a size value
				if !strings.Contains(resp.Content, "K") && !strings.Contains(resp.Content, "M") &&
					!strings.Contains(resp.Content, "G") && !strings.Contains(resp.Content, "bytes") &&
					!strings.Contains(resp.Content, "B") {
					t.Errorf("du output should contain a size, got: %s", resp.Content)
				}
			},
			Expected: "Disk usage shown",
		},
		{
			ID:       "sys-003",
			Category: catSystemInfo,
			Prompt:   "Show free disk space",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "df -h .",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// Should contain filesystem info
				assertContains(t, resp.Content, "Filesystem")
				assertContains(t, resp.Content, "Avail")
			},
			Expected: "Free disk space shown",
		},
		{
			ID:       "sys-004",
			Category: catSystemInfo,
			Prompt:   "Show the current date in ISO format",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "date -I",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				// Should look like YYYY-MM-DD
				content := strings.TrimSpace(resp.Content)
				if len(content) < 10 {
					t.Errorf("date -I should return YYYY-MM-DD, got: %s", content)
				}
			},
			Expected: "Date in ISO format",
		},
		{
			ID:       "sys-005",
			Category: catSystemInfo,
			Prompt:   "Count the number of CPU cores",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "nproc",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				content := strings.TrimSpace(resp.Content)
				if content == "" || content == "0" {
					t.Errorf("nproc should return a positive number, got: %s", content)
				}
			},
			Expected: "Number of CPU cores shown",
		},

		// ================================================================
		// CONDITIONALS
		// ================================================================
		{
			ID:       "cond-001",
			Category: catConditionals,
			Prompt:   "Check if a file exists and print 'exists' or 'missing'",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "check.txt"), "exists\n")
			},
			Command: "test -f check.txt && echo 'exists' || echo 'missing'",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "exists")
				assertNotContains(t, resp.Content, "missing")
			},
			Expected: "Prints 'exists' when file is present",
		},
		{
			ID:       "cond-002",
			Category: catConditionals,
			Prompt:   "Check if a nonexistent file exists and print 'exists' or 'missing'",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "test -f nofile.txt && echo 'exists' || echo 'missing'",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "missing")
				assertNotContains(t, resp.Content, "exists")
			},
			Expected: "Prints 'missing' when file is absent",
		},
		{
			ID:       "cond-003",
			Category: catConditionals,
			Prompt:   "Check if a variable is empty and set a default",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "echo ${UNSET_VAR:-default_value}",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "default_value")
			},
			Expected: "Default value used for unset variable",
		},
		{
			ID:       "cond-004",
			Category: catConditionals,
			Prompt:   "Check if a directory exists and create it if not",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "test -d newdir || mkdir newdir",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "newdir"))
			},
			Expected: "Directory created if it didn't exist",
		},

		// ================================================================
		// VARIABLE EXPANSION
		// ================================================================
		{
			ID:       "var-001",
			Category: catVariables,
			Prompt:   "Use command substitution to store and print the current date",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "DATE=$(date +%Y) && echo $DATE",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "20") // Year starts with 20
			},
			Expected: "Year printed via command substitution",
		},
		{
			ID:       "var-002",
			Category: catVariables,
			Prompt:   "Use string length to get the length of a variable",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "STR=hello && echo ${#STR}",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "5")
			},
			Expected: "Length of 'hello' is 5",
		},
		{
			ID:       "var-003",
			Category: catVariables,
			Prompt:   "Use substring extraction to get first 3 chars of a string",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "STR=abcdef && echo ${STR:0:3}",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "abc")
			},
			Expected: "First 3 chars 'abc' extracted",
		},
		{
			ID:       "var-004",
			Category: catVariables,
			Prompt:   "Use parameter expansion to replace part of a string",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "STR=hello_world && echo ${STR/world/earth}",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "hello_earth")
			},
			Expected: "'world' replaced with 'earth'",
		},

		// ================================================================
		// MULTI-STEP
		// ================================================================
		{
			ID:       "multi-001",
			Category: catMultiStep,
			Prompt:   "Create a project structure with README, src/main.go, and tests/main_test.go",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "mkdir -p src tests && echo '# Project' | tee README.md && echo 'package main' | tee src/main.go && echo 'package main' | tee tests/main_test.go",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "README.md"))
				assertFileExists(t, filepath.Join(dir, "src", "main.go"))
				assertFileExists(t, filepath.Join(dir, "tests", "main_test.go"))
				assertFileContent(t, filepath.Join(dir, "src", "main.go"), "package main")
			},
			Expected: "Project structure with 3 files created",
		},
		{
			ID:       "multi-002",
			Category: catMultiStep,
			Prompt:   "Generate a file with numbers 1-10, then show only even numbers",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "seq 1 10 | tee numbers.txt && echo '---' && awk 'NR%2==0' numbers.txt",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "2")
				assertContains(t, resp.Content, "4")
				assertContains(t, resp.Content, "6")
				assertContains(t, resp.Content, "8")
				assertContains(t, resp.Content, "10")
			},
			Expected: "Even numbers 2,4,6,8,10 shown",
		},
		{
			ID:       "multi-003",
			Category: catMultiStep,
			Prompt:   "Create a backup of all .txt files with .bak extension",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "a.txt"), "content a\n")
				writeFile(t, filepath.Join(dir, "b.txt"), "content b\n")
				writeFile(t, filepath.Join(dir, "c.log"), "log\n")
			},
			Command: "for f in *.txt; do cp \"$f\" \"${f}.bak\"; done",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "a.txt.bak"))
				assertFileExists(t, filepath.Join(dir, "b.txt.bak"))
				assertFileNotExists(t, filepath.Join(dir, "c.log.bak"))
			},
			Expected: "All .txt files backed up with .bak extension",
		},
		{
			ID:       "multi-004",
			Category: catMultiStep,
			Prompt:   "Create a log file with timestamps for 5 entries",
			Setup:    func(t *testing.T, dir string) {},
			Command:  "for i in 1 2 3 4 5; do echo \"$(date +%H:%M:%S) - Entry $i\" | tee -a logfile.txt; done",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertFileExists(t, filepath.Join(dir, "logfile.txt"))
				content := readFile(t, filepath.Join(dir, "logfile.txt"))
				for i := 1; i <= 5; i++ {
					expected := fmt.Sprintf("Entry %d", i)
					if !strings.Contains(content, expected) {
						t.Errorf("logfile.txt should contain %q, got: %s", expected, content)
					}
				}
			},
			Expected: "logfile.txt with 5 timestamped entries",
		},
		{
			ID:       "multi-005",
			Category: catMultiStep,
			Prompt:   "Find the largest file in the directory and print its size",
			Setup: func(t *testing.T, dir string) {
				writeFile(t, filepath.Join(dir, "small.txt"), "a\n")
				writeFile(t, filepath.Join(dir, "large.txt"), strings.Repeat("x", 5000))
				writeFile(t, filepath.Join(dir, "medium.txt"), strings.Repeat("y", 500))
			},
			Command: "ls -lS *.txt | head -1 | awk '{print $5, $9}'",
			Verify: func(t *testing.T, dir string, resp fantasy.ToolResponse) {
				assertNoError(t, resp)
				assertContains(t, resp.Content, "large.txt")
			},
			Expected: "Largest file (large.txt) with its size shown",
		},
	}
}

// --- Benchmark runner ---

func TestShellUseBenchmark(t *testing.T) {
	tasks := shellUseTasks()

	for _, task := range tasks {
		t.Run(task.ID, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cfg := shellUseCtx(dir)

			// Setup
			task.Setup(t, dir)

			// Execute
			resp := runBash(t, ctx, cfg, task.Command)

			// Verify
			task.Verify(t, dir, resp)
		})
		// If we get here without t.Fatal, the test passed
		// (subtests handle their own pass/fail)
	}
}

// TestShellUseBenchmarkSummary runs all tasks and produces a summary report.
// This is the main entry point that mirrors how the shell-use benchmark
// reports results: by category with pass/fail counts.
func TestShellUseBenchmarkSummary(t *testing.T) {
	tasks := shellUseTasks()
	categories := make(map[shellUseCategory][]shellUseTask)
	for _, task := range tasks {
		categories[task.Category] = append(categories[task.Category], task)
	}

	// Sort category names for consistent output
	catNames := make([]string, 0, len(categories))
	for c := range categories {
		catNames = append(catNames, string(c))
	}
	sort.Strings(catNames)

	totalPass := 0
	totalFail := 0
	categoryResults := make(map[string]int) // category -> pass count

	for _, catName := range catNames {
		catTasks := categories[shellUseCategory(catName)]
		pass := 0
		fail := 0

		for _, task := range catTasks {
			// Run each task in its own subtest
			passed := t.Run(task.ID, func(t *testing.T) {
				dir := t.TempDir()
				ctx, cfg := shellUseCtx(dir)
				task.Setup(t, dir)
				resp := runBash(t, ctx, cfg, task.Command)
				task.Verify(t, dir, resp)
			})

			if passed {
				pass++
				totalPass++
			} else {
				fail++
				totalFail++
			}
		}

		categoryResults[catName] = pass
		t.Logf("  [%s] %d/%d passed", catName, pass, len(catTasks))
	}

	t.Logf("")
	t.Logf("=== Shell-Use Benchmark Summary ===")
	t.Logf("Total tasks: %d", len(tasks))
	t.Logf("Passed: %d", totalPass)
	t.Logf("Failed: %d", totalFail)
	t.Logf("Pass rate: %.1f%%", float64(totalPass)/float64(len(tasks))*100)
	t.Logf("")

	for _, catName := range catNames {
		catTasks := categories[shellUseCategory(catName)]
		pass := categoryResults[catName]
		t.Logf("  %-20s %d/%d (%.0f%%)", catName, pass, len(catTasks),
			float64(pass)/float64(len(catTasks))*100)
	}
}

// --- Tool-specific benchmark tests ---

// TestShellUseGrepTool benchmarks the grep tool with various patterns.
func TestShellUseGrepTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "test.txt"), "hello world\nfoo bar\nhello again\n")

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"grep"})
	tool := NewGrepTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	// Test: basic pattern match
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "grep-1",
		Name:  "grep",
		Input: `{"pattern": "hello", "path": "test.txt"}`,
	})
	if err != nil {
		t.Fatalf("grep tool error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "hello world")
	assertContains(t, resp.Content, "hello again")
}

// TestShellUseGlobTool benchmarks the glob tool.
func TestShellUseGlobTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "b.txt"), "b\n")
	writeFile(t, filepath.Join(dir, "c.go"), "c\n")

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"glob"})
	tool := NewGlobTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "glob-1",
		Name:  "glob",
		Input: `{"pattern": "*.txt"}`,
	})
	if err != nil {
		t.Fatalf("glob tool error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "a.txt")
	assertContains(t, resp.Content, "b.txt")
	assertNotContains(t, resp.Content, "c.go")
}

// TestShellUseViewTool benchmarks the view tool.
func TestShellUseViewTool(t *testing.T) {
	dir := t.TempDir()
	content := "line1\nline2\nline3\n"
	writeFile(t, filepath.Join(dir, "view.txt"), content)

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"view"})
	tool := NewViewTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "view-1",
		Name:  "view",
		Input: `{"file_path": "view.txt"}`,
	})
	if err != nil {
		t.Fatalf("view tool error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "line1")
	assertContains(t, resp.Content, "line2")
	assertContains(t, resp.Content, "line3")
}

// TestShellUseWriteTool benchmarks the write tool.
func TestShellUseWriteTool(t *testing.T) {
	dir := t.TempDir()

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"write"})
	tool := NewWriteTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "write-1",
		Name:  "write",
		Input: `{"file_path": "output.txt", "content": "written by tool"}`,
	})
	if err != nil {
		t.Fatalf("write tool error: %v", err)
	}
	assertNoError(t, resp)
	assertFileExists(t, filepath.Join(dir, "output.txt"))
	assertFileContent(t, filepath.Join(dir, "output.txt"), "written by tool")
}

// TestShellUseLsTool benchmarks the ls tool.
func TestShellUseLsTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file1.txt"), "a\n")
	writeFile(t, filepath.Join(dir, "file2.go"), "b\n")
	os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"ls"})
	tool := NewLsTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "ls-1",
		Name:  "ls",
		Input: `{"path": "."}`,
	})
	if err != nil {
		t.Fatalf("ls tool error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "file1.txt")
	assertContains(t, resp.Content, "file2.go")
	assertContains(t, resp.Content, "subdir")
}

// TestShellUseMathTool benchmarks the math tool.
func TestShellUseMathTool(t *testing.T) {
	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"math"})
	tool := NewMathTool()

	// Test basic arithmetic
	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "math-1",
		Name:  "math",
		Input: `{"expression": "2 + 3 * 4"}`,
	})
	if err != nil {
		t.Fatalf("math tool error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "14")

	// Test division
	resp2, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "math-2",
		Name:  "math",
		Input: `{"expression": "100 / 4"}`,
	})
	if err != nil {
		t.Fatalf("math tool error: %v", err)
	}
	assertNoError(t, resp2)
	assertContains(t, resp2.Content, "25")
}

// TestShellUseEditTool benchmarks the edit tool.
func TestShellUseEditTool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "edit.txt"), "hello world\nfoo bar\n")

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"edit"})
	tool := NewEditTool(WithWorkDir(dir), WithAllowedPaths([]string{dir}))

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "edit-1",
		Name:  "edit",
		Input: `{"file_path": "edit.txt", "old_string": "hello world", "new_string": "hello universe"}`,
	})
	if err != nil {
		t.Fatalf("edit tool error: %v", err)
	}
	assertNoError(t, resp)
	content := readFile(t, filepath.Join(dir, "edit.txt"))
	assertContains(t, content, "hello universe")
	assertNotContains(t, content, "hello world")
}

// --- Security benchmark tests ---

// TestShellUseSecurityBannedCommands verifies that banned commands are rejected.
func TestShellUseSecurityBannedCommands(t *testing.T) {
	dir := t.TempDir()
	ctx, cfg := shellUseCtx(dir)

	banned := []string{
		"alias ll='ls -la'",
		"bg %1",
		"kill -9 1",
		"set -e",
		"source ~/.bashrc",
		"trap 'echo done' EXIT",
		"umask 022",
	}

	for _, cmd := range banned {
		t.Run(cmd, func(t *testing.T) {
			resp := runBash(t, ctx, cfg, cmd)
			assertError(t, resp)
		})
	}
}

// TestShellUseSecurityCdBlocked verifies that cd outside the single common
// "cd <dir> && <rest>" leading shape is still blocked.
func TestShellUseSecurityCdBlocked(t *testing.T) {
	dir := t.TempDir()
	ctx, cfg := shellUseCtx(dir)

	resp := runBash(t, ctx, cfg, "ls; cd . && ls")
	assertError(t, resp)
	assertContains(t, resp.Content, "not allowed")
}

// TestShellUseSecurityLeadingCdIsTranslated verifies that the common
// "cd <dir> && <rest>" shape is auto-translated into working_directory
// instead of being rejected, since models default to this out of shell
// habit and rejecting it just costs a round trip for no security benefit
// (the directory still goes through the normal working_directory allowlist
// check below).
func TestShellUseSecurityLeadingCdIsTranslated(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "marker.txt"), "marker\n")
	ctx, cfg := shellUseCtx(dir)

	resp := runBash(t, ctx, cfg, fmt.Sprintf("cd %s && ls marker.txt", dir))
	assertNoError(t, resp)
	assertContains(t, resp.Content, "marker.txt")
}

// TestShellUseSecurityWorkingDirectory tests working_directory parameter.
func TestShellUseSecurityWorkingDirectory(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	writeFile(t, filepath.Join(dir2, "marker.txt"), "marker\n")

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"bash"})
	cfg := ApplyOptions([]ToolOption{
		WithWorkDir(dir1),
		WithAllowedPaths([]string{dir1, dir2}),
	})

	// Execute in dir2 via working_directory parameter
	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID:    "wd-1",
		Name:  "bash",
		Input: fmt.Sprintf(`{"command": "ls marker.txt", "working_directory": %q}`, dir2),
	}, cfg)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "marker.txt")
}

// TestShellUseSecurityRedundantCdWithExplicitWorkDirIsStripped verifies that
// a model which already set working_directory but also habitually prefixed
// the command with a matching "cd <same dir> && ..." gets the redundant cd
// stripped instead of rejected — a real run hit this exact shape 3 times in
// one session, self-correcting each time by dropping the cd on retry.
func TestShellUseSecurityRedundantCdWithExplicitWorkDirIsStripped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "marker.txt"), "marker\n")

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"bash"})
	cfg := ApplyOptions([]ToolOption{
		WithWorkDir(dir),
		WithAllowedPaths([]string{dir}),
	})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID:    "wd-redundant-cd",
		Name:  "bash",
		Input: fmt.Sprintf(`{"command": %q, "working_directory": %q}`, fmt.Sprintf("cd %s && ls marker.txt", dir), dir),
	}, cfg)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	assertNoError(t, resp)
	assertContains(t, resp.Content, "marker.txt")
}

// TestShellUseSecurityConflictingCdWithExplicitWorkDirStillRejected verifies
// that a cd targeting a DIFFERENT directory than the explicit
// working_directory is still rejected rather than silently picking one —
// only an exact match is treated as redundant.
func TestShellUseSecurityConflictingCdWithExplicitWorkDirStillRejected(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()

	ctx := context.WithValue(context.Background(), UnattendedKey, true)
	ctx = SetToolsAllowed(ctx, []string{"bash"})
	cfg := ApplyOptions([]ToolOption{
		WithWorkDir(dir1),
		WithAllowedPaths([]string{dir1, dir2}),
	})

	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID:    "wd-conflicting-cd",
		Name:  "bash",
		Input: fmt.Sprintf(`{"command": %q, "working_directory": %q}`, fmt.Sprintf("cd %s && ls", dir2), dir1),
	}, cfg)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	assertError(t, resp)
	assertContains(t, resp.Content, "not allowed")
}

// TestShellUseSecurityTimeout verifies timeout enforcement.
func TestShellUseSecurityTimeout(t *testing.T) {
	dir := t.TempDir()
	ctx, cfg := shellUseCtx(dir)

	start := time.Now()
	resp, err := executeBash(ctx, fantasy.ToolCall{
		ID:    "timeout-1",
		Name:  "bash",
		Input: `{"command": "sleep 10", "timeout": 1}`,
	}, cfg)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("executeBash error: %v", err)
	}
	assertError(t, resp)
	assertContains(t, resp.Content, "timed out")
	if elapsed > 5*time.Second {
		t.Errorf("timeout should have triggered within ~1s, took %s", elapsed)
	}
}

// --- Edge case benchmarks ---

// TestShellUseEdgeCases tests edge cases in shell execution.
func TestShellUseEdgeCases(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty_command", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp, err := executeBash(ctx, fantasy.ToolCall{
			ID:    "edge-1",
			Name:  "bash",
			Input: `{"command": ""}`,
		}, cfg)
		if err != nil {
			t.Fatalf("error: %v", err)
		}
		assertError(t, resp)
	})

	t.Run("command_not_found", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "nonexistentcommand12345")
		assertError(t, resp)
	})

	t.Run("exit_code_propagation", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "exit 42")
		assertError(t, resp)
	})

	t.Run("multiline_command", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "echo line1\necho line2\necho line3")
		assertNoError(t, resp)
		assertContains(t, resp.Content, "line1")
		assertContains(t, resp.Content, "line2")
		assertContains(t, resp.Content, "line3")
	})

	t.Run("pipe_chain", func(t *testing.T) {
		writeFile(t, filepath.Join(dir, "pipe.txt"), "c\na\nb\na\nc\nb\n")
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "cat pipe.txt | sort | uniq -c | sort -rn")
		assertNoError(t, resp)
		assertContains(t, resp.Content, "2")
	})

	t.Run("subshell", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "(echo inside_subshell)")
		assertNoError(t, resp)
		assertContains(t, resp.Content, "inside_subshell")
	})

	t.Run("heredoc", func(t *testing.T) {
		ctx, cfg := shellUseCtx(dir)
		resp := runBash(t, ctx, cfg, "cat << 'EOF'\nhello\nworld\nEOF")
		assertNoError(t, resp)
		assertContains(t, resp.Content, "hello")
		assertContains(t, resp.Content, "world")
	})
}
