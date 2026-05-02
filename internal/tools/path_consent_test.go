package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsPathAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(subDir, 0o755)

	allowedPaths := []string{subDir}

	tests := []struct {
		name    string
		path    string
		allowed bool
	}{
		{"path inside allowed", filepath.Join(subDir, "file.txt"), true},
		{"path is allowed dir itself", subDir, true},
		{"path outside allowed", filepath.Join(tmpDir, "other", "file.txt"), false},
		{"path parent of allowed", tmpDir, false},
		{"path with different prefix", "/tmp/other/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPathAllowed(tt.path, allowedPaths)
			if result != tt.allowed {
				t.Errorf("isPathAllowed(%q, %v) = %v, want %v", tt.path, allowedPaths, result, tt.allowed)
			}
		})
	}
}

func TestIsPathAllowedMultiple(t *testing.T) {
	tmpDir := t.TempDir()
	dir1 := filepath.Join(tmpDir, "dir1")
	dir2 := filepath.Join(tmpDir, "dir2")
	os.MkdirAll(dir1, 0o755)
	os.MkdirAll(dir2, 0o755)

	allowedPaths := []string{dir1, dir2}

	if !isPathAllowed(filepath.Join(dir1, "test.txt"), allowedPaths) {
		t.Error("path in dir1 should be allowed")
	}
	if !isPathAllowed(filepath.Join(dir2, "test.txt"), allowedPaths) {
		t.Error("path in dir2 should be allowed")
	}
	if isPathAllowed(filepath.Join(tmpDir, "dir3", "test.txt"), allowedPaths) {
		t.Error("path outside both dirs should not be allowed")
	}
}

func TestPathConsentRemembered(t *testing.T) {
	pc := NewPathConsent()

	if pc.IsRemembered("/some/path") {
		t.Error("empty consent should not remember anything")
	}

	pc.mu.Lock()
	pc.remembered = append(pc.remembered, "/some/path"+string(os.PathSeparator))
	pc.mu.Unlock()

	if !pc.IsRemembered("/some/path/file.txt") {
		t.Error("child of remembered path should be remembered")
	}
	if !pc.IsRemembered("/some/path") {
		t.Error("exact remembered path should be remembered")
	}
	if pc.IsRemembered("/other/path") {
		t.Error("unrelated path should not be remembered")
	}
	if pc.IsRemembered("/some/path-other") {
		t.Error("path with common prefix but different directory should not be remembered")
	}
}

func TestCheckPathOrConsentDeny(t *testing.T) {
	tmpDir := t.TempDir()
	outsideFile := filepath.Join(tmpDir, "outside.txt")
	os.WriteFile(outsideFile, []byte("data"), 0o644)

	cfg := ToolConfig{
		WorkDir:      tmpDir,
		AllowedPaths: []string{filepath.Join(tmpDir, "allowed")},
		PathConsent:  nil,
	}

	_, err := checkPathOrConsent(outsideFile, tmpDir, "read", cfg)
	if err == nil {
		t.Error("expected error for path outside allowed with no consent")
	}
}

func TestCheckPathOrConsentAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	os.MkdirAll(allowedDir, 0o755)

	insideFile := filepath.Join(allowedDir, "file.txt")
	os.WriteFile(insideFile, []byte("data"), 0o644)

	cfg := ToolConfig{
		WorkDir:      tmpDir,
		AllowedPaths: []string{allowedDir},
		PathConsent:  nil,
	}

	path, err := checkPathOrConsent(insideFile, tmpDir, "read", cfg)
	if err != nil {
		t.Errorf("expected no error for path inside allowed, got: %v", err)
	}
	if path != insideFile {
		t.Errorf("expected %s, got %s", insideFile, path)
	}
}

func TestExtractPathsFromCommand(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantMin int
		wantHas []string
	}{
		{
			name:    "cd with absolute path",
			cmd:     "cd /home/user/project",
			wantMin: 1,
			wantHas: []string{"/home/user/project"},
		},
		{
			name:    "cd with quoted path",
			cmd:     `cd "/home/user/my project"`,
			wantMin: 1,
			wantHas: []string{"/home/user/my project"},
		},
		{
			name:    "cd with single quoted path",
			cmd:     `cd '/home/user/my project'`,
			wantMin: 1,
			wantHas: []string{"/home/user/my project"},
		},
		{
			name:    "absolute path in command",
			cmd:     "cat /etc/passwd",
			wantMin: 1,
			wantHas: []string{"/etc/passwd"},
		},
		{
			name:    "relative path only",
			cmd:     "cat file.txt",
			wantMin: 0,
			wantHas: []string{},
		},
		{
			name:    "pipe with absolute paths",
			cmd:     "cat /var/log/syslog | grep error",
			wantMin: 1,
			wantHas: []string{"/var/log/syslog"},
		},
		{
			name:    "redirect output to absolute path",
			cmd:     "echo hello > /tmp/out.txt",
			wantMin: 1,
			wantHas: []string{"/tmp/out.txt"},
		},
		{
			name:    "no paths",
			cmd:     "ls -la",
			wantMin: 0,
			wantHas: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := extractPathsFromCommand(tt.cmd, "/home/user")
			if len(paths) < tt.wantMin {
				t.Errorf("extractPathsFromCommand(%q) returned %d paths, want at least %d", tt.cmd, len(paths), tt.wantMin)
			}
			for _, want := range tt.wantHas {
				found := false
				for _, p := range paths {
					if p == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("extractPathsFromCommand(%q) missing path %q, got %v", tt.cmd, want, paths)
				}
			}
		})
	}
}

func TestIsSystemPath(t *testing.T) {
	tests := []struct {
		path  string
		isSys bool
	}{
		{"/usr/bin/ls", true},
		{"/usr/lib/x86_64-linux-gnu/libc.so", true},
		{"/bin/bash", true},
		{"/sbin/iptables", true},
		{"/lib/modules/5.15.0/kernel.ko", true},
		{"/proc/self/status", true},
		{"/sys/class/net", true},
		{"/dev/null", true},
		{"/etc/alternatives/python3", true},
		{"/home/user/file.txt", false},
		{"/tmp/output.txt", false},
		{"/var/log/syslog", false},
		{"/opt/app/config.yaml", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			result := isSystemPath(tt.path)
			if result != tt.isSys {
				t.Errorf("isSystemPath(%q) = %v, want %v", tt.path, result, tt.isSys)
			}
		})
	}
}

func TestResolveAndValidatePathWithConsentAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "project")
	os.MkdirAll(subDir, 0o755)

	cfg := ToolConfig{
		WorkDir:      subDir,
		AllowedPaths: []string{subDir},
	}

	path, err := resolveAndValidatePathWithConsent("test.txt", cfg)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if path != filepath.Join(subDir, "test.txt") {
		t.Errorf("expected %s, got %s", filepath.Join(subDir, "test.txt"), path)
	}
}
