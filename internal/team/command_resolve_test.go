package team

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestResolveStageExecutables(t *testing.T) {
	tempDir := t.TempDir()

	// Create a dummy executable file under tempDir for project-local testing.
	localScript := filepath.Join(tempDir, "local-runner.sh")
	if err := os.WriteFile(localScript, []byte("#!/bin/sh\necho hello\n"), 0755); err != nil {
		t.Fatalf("failed to create dummy script: %v", err)
	}

	// Create a dummy non-executable file (mode 0644) under tempDir.
	nonExecScript := filepath.Join(tempDir, "non-exec.sh")
	if err := os.WriteFile(nonExecScript, []byte("#!/bin/sh\necho hello\n"), 0644); err != nil {
		t.Fatalf("failed to create non-exec script: %v", err)
	}

	tests := []struct {
		name        string
		command     string
		workDir     string
		wantFinding bool
		wantCount   int
		wantCode    string
		wantHintSub string
	}{
		{
			name:        "standard PATH executable and builtin",
			command:     "echo hello | grep -q hello",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "shell builtins and keywords",
			command:     "test -f foo || exit 1",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "explicit relative path existing and executable",
			command:     "./local-runner.sh --flag",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "explicit relative path existing but non-executable mode 0644",
			command:     "./non-exec.sh",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
			wantHintSub: "Grant execute permission",
		},
		{
			name:        "absolute path existing but non-executable mode 0644",
			command:     nonExecScript,
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
			wantHintSub: "Grant execute permission",
		},
		{
			name:        "explicit relative path missing",
			command:     "./missing-runner.sh",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "bare command not in PATH but exists locally",
			command:     "local-runner.sh",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
			wantHintSub: "A project-local executable was found at",
		},
		{
			name:        "bare command not in PATH and not local",
			command:     "definitely_missing_cmd_99999",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "multi-stage pipeline with middle stage unresolved",
			command:     "echo test | definitely_missing_cmd_99999 | grep test",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "duplicate unresolved across multiple pipeline stages produces multiple findings",
			command:     "definitely_missing_cmd_99999 | definitely_missing_cmd_99999",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   2,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "duplicate unresolved within same stage is deduplicated within stage",
			command:     "definitely_missing_cmd_99999 && definitely_missing_cmd_99999",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "shell negation continues to the executable",
			command:     "! grep -q running",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "shell negation still catches unresolved executable",
			command:     "! definitely_missing_cmd_99999",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "shell control keywords are not executable names",
			command:     "if test -f foo; then echo ok; fi",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "case body unresolved executable is inspected",
			command:     "case x in x) definitely_missing_cmd_99999 ;; esac",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "case command substitution selector is not an executable",
			command:     `case "$(printf x)" in x) true ;; *) false ;; esac`,
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "case command substitution selector still inspects body",
			command:     `case "$(printf x)" in x) definitely_missing_cmd_99999 ;; *) false ;; esac`,
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
		{
			name:        "absolute path existing",
			command:     "/bin/ls -la",
			workDir:     tempDir,
			wantFinding: false,
		},
		{
			name:        "absolute path missing",
			command:     "/nonexistent_dir_xyz/missing_bin",
			workDir:     tempDir,
			wantFinding: true,
			wantCount:   1,
			wantCode:    FindingExecutableUnresolved,
		},
	}

	// Test FIFO / named pipe resolution if supported
	fifoPath := filepath.Join(tempDir, "test_fifo.pipe")
	if err := syscall.Mkfifo(fifoPath, 0755); err == nil {
		t.Run("explicit relative path to named pipe FIFO with 0755 mode", func(t *testing.T) {
			findings := ResolveCommandExecutables("./test_fifo.pipe", tempDir)
			if len(findings) != 1 {
				t.Fatalf("expected 1 finding for FIFO, got %d", len(findings))
			}
			if findings[0].Code != FindingExecutableUnresolved {
				t.Errorf("finding.Code = %q, want %q", findings[0].Code, FindingExecutableUnresolved)
			}
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := ResolveCommandExecutables(tt.command, tt.workDir)
			if (len(findings) > 0) != tt.wantFinding {
				t.Fatalf("ResolveCommandExecutables(%q) got %d findings, wantFinding %v. Findings: %v",
					tt.command, len(findings), tt.wantFinding, findings)
			}
			if tt.wantFinding {
				if tt.wantCount > 0 && len(findings) != tt.wantCount {
					t.Errorf("ResolveCommandExecutables(%q) got %d findings, wantCount %d. Findings: %v",
						tt.command, len(findings), tt.wantCount, findings)
				}
				if findings[0].Code != tt.wantCode {
					t.Errorf("finding.Code = %q, want %q", findings[0].Code, tt.wantCode)
				}
				if findings[0].Severity != FindingSeverityError {
					t.Errorf("finding.Severity = %q, want %q", findings[0].Severity, FindingSeverityError)
				}
				if tt.wantHintSub != "" && !containsSub(findings[0].Hint, tt.wantHintSub) {
					t.Errorf("finding.Hint = %q, want to contain %q", findings[0].Hint, tt.wantHintSub)
				}
			}
		})
	}
}

type mockFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return m.size }
func (m mockFileInfo) Mode() os.FileMode  { return m.mode }
func (m mockFileInfo) ModTime() time.Time { return m.modTime }
func (m mockFileInfo) IsDir() bool        { return m.isDir }
func (m mockFileInfo) Sys() any           { return nil }

func TestIsExecutableFile(t *testing.T) {
	tests := []struct {
		name     string
		info     os.FileInfo
		wantExec bool
	}{
		{
			name:     "regular executable file (0755)",
			info:     mockFileInfo{name: "exec.sh", mode: 0755},
			wantExec: true,
		},
		{
			name:     "regular non-executable file (0644)",
			info:     mockFileInfo{name: "file.txt", mode: 0644},
			wantExec: false,
		},
		{
			name:     "directory with 0755 mode",
			info:     mockFileInfo{name: "dir", mode: os.ModeDir | 0755, isDir: true},
			wantExec: false,
		},
		{
			name:     "named pipe FIFO with 0755 mode",
			info:     mockFileInfo{name: "fifo", mode: os.ModeNamedPipe | 0755},
			wantExec: false,
		},
		{
			name:     "socket with 0755 mode",
			info:     mockFileInfo{name: "sock", mode: os.ModeSocket | 0755},
			wantExec: false,
		},
		{
			name:     "character device with 0755 mode",
			info:     mockFileInfo{name: "chardev", mode: os.ModeCharDevice | 0755},
			wantExec: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isExecutableFile(tt.info)
			if got != tt.wantExec {
				t.Errorf("isExecutableFile(%s mode=%v) = %v, want %v", tt.name, tt.info.Mode(), got, tt.wantExec)
			}
		})
	}
}

func containsSub(s, sub string) bool {
	return strings.Contains(s, sub)
}
