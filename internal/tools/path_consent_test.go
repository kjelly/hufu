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

	if pc.IsRemembered("/some/path/file.txt") {
		t.Error("empty consent should not remember anything")
	}

	pc.mu.Lock()
	pc.remembered = append(pc.remembered, "/some/path/")
	pc.mu.Unlock()

	if !pc.IsRemembered("/some/path/file.txt") {
		t.Error("file in remembered dir should be remembered")
	}
	if !pc.IsRemembered("/some/path/subdir/file.txt") {
		t.Error("file in subdirectory of remembered dir should be remembered")
	}
	if pc.IsRemembered("/some/other/file.txt") {
		t.Error("unrelated path should not be remembered")
	}
	if pc.IsRemembered("/some/path-other/file.txt") {
		t.Error("path with common prefix but different directory should not be remembered")
	}
}

func TestPathConsentDenied(t *testing.T) {
	pc := NewPathConsent()

	if pc.IsDenied("/secrets/key.pem") {
		t.Error("empty consent should not deny anything")
	}

	pc.mu.Lock()
	pc.denied = append(pc.denied, "/secrets/")
	pc.mu.Unlock()

	if !pc.IsDenied("/secrets/key.pem") {
		t.Error("file in denied dir should be denied")
	}
	if !pc.IsDenied("/secrets/other.pem") {
		t.Error("other file in denied dir should be denied")
	}
	if !pc.IsDenied("/secrets/subproject/file.txt") {
		t.Error("file in subdirectory of denied dir should be denied (prefix matching)")
	}
	if pc.IsDenied("/other/path/file.txt") {
		t.Error("unrelated path should not be denied")
	}
}

func TestPathConsentAllowedOverridesDenied(t *testing.T) {
	pc := NewPathConsent()

	pc.mu.Lock()
	pc.denied = append(pc.denied, "/secrets/")
	pc.remembered = append(pc.remembered, "/secrets/subproject/")
	pc.mu.Unlock()

	if !pc.IsDenied("/secrets/key.pem") {
		t.Error("file in denied dir should still be denied")
	}
	if !pc.IsDenied("/secrets/subproject/file.txt") {
		t.Error("file in subdirectory of denied dir should be denied (prefix matching)")
	}
	if !pc.IsRemembered("/secrets/subproject/file.txt") {
		t.Error("file in remembered subdirectory should be remembered")
	}
}

func TestDirOfPath(t *testing.T) {
	tmpDir := t.TempDir()
	subDir := filepath.Join(tmpDir, "mydir")
	os.MkdirAll(subDir, 0o755)

	tests := []struct {
		name string
		path string
		want string
	}{
		{"existing directory", subDir, subDir},
		{"file in directory", filepath.Join(subDir, "file.txt"), subDir},
		{"non-existent file with extension", filepath.Join(tmpDir, "noexist", "file.txt"), filepath.Join(tmpDir, "noexist")},
		{"non-existent directory without extension", filepath.Join(tmpDir, "noexist", "subdir") + string(os.PathSeparator), filepath.Join(tmpDir, "noexist", "subdir")},
		{"non-existent dotfile", filepath.Join(tmpDir, "noexist", ".gitignore"), filepath.Join(tmpDir, "noexist")},
		{"non-existent Makefile treated as file", filepath.Join(tmpDir, "noexist", "Makefile"), filepath.Join(tmpDir, "noexist")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dirOfPath(tt.path)
			if got != tt.want {
				t.Errorf("dirOfPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
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
		{
			name:    "env -i with home and path",
			cmd:     "env -i HOME=/home/kjelly PATH=/usr/bin GH_CONFIG_DIR=/home/kjelly/.config/gh gh workflow run deploy.yml",
			wantMin: 0,
			wantHas: []string{},
		},
		{
			name:    "env var assignment mixed with real path",
			cmd:     "HOME=/home/user cat /etc/passwd",
			wantMin: 1,
			wantHas: []string{"/etc/passwd"},
		},
		{
			name:    "multiple env vars with real paths",
			cmd:     "FOO=/tmp/bar BAZ=/tmp/baz ls /var/log",
			wantMin: 1,
			wantHas: []string{"/var/log"},
		},
		{
			name:    "flag with path still detected",
			cmd:     "curl --config=/etc/app.conf https://example.com",
			wantMin: 1,
			wantHas: []string{"/etc/app.conf"},
		},
		{
			name:    "env assignment at start",
			cmd:     "HOME=/home/test make",
			wantMin: 0,
			wantHas: []string{},
		},
		{
			name:    "env assignment with semicolon separator",
			cmd:     "HOME=/home/test; ls /tmp",
			wantMin: 1,
			wantHas: []string{"/tmp"},
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
