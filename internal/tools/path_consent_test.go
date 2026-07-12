package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestIsPathAllowedCurrentWorkingDir(t *testing.T) {
	tmpDir := t.TempDir()
	child := filepath.Join(tmpDir, "docs", "note.txt")
	if err := os.MkdirAll(filepath.Dir(child), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(child, []byte("data"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
	})

	if !isPathAllowed(child, nil) {
		t.Fatalf("expected %q to be allowed under current working directory %q", child, tmpDir)
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

func TestTeamPathConsentPersistsAlwaysDecisions(t *testing.T) {
	teamDir := t.TempDir()
	consent, err := NewTeamPathConsent(teamDir)
	if err != nil {
		t.Fatalf("NewTeamPathConsent() error = %v", err)
	}

	consent.mu.Lock()
	consent.remembered = append(consent.remembered, "/srv/example/")
	consent.denied = append(consent.denied, "/srv/private/")
	err = consent.persistLocked()
	consent.mu.Unlock()
	if err != nil {
		t.Fatalf("persistLocked() error = %v", err)
	}

	reloaded, err := NewTeamPathConsent(teamDir)
	if err != nil {
		t.Fatalf("NewTeamPathConsent() reload error = %v", err)
	}
	if !reloaded.IsRemembered("/srv/example/file.txt") {
		t.Error("persisted allow was not restored")
	}
	if !reloaded.IsDenied("/srv/private/file.txt") {
		t.Error("persisted deny was not restored")
	}
}

func TestUpdatePathConsentPolicyReplacesOppositeDecision(t *testing.T) {
	teamDir := t.TempDir()
	policy, err := UpdatePathConsentPolicy(teamDir, "allow", "/srv/example")
	if err != nil {
		t.Fatalf("allow error = %v", err)
	}
	if got, want := strings.Join(policy.Allowed, ","), "/srv/example"; got != want {
		t.Fatalf("allowed = %q, want %q", got, want)
	}

	policy, err = UpdatePathConsentPolicy(teamDir, "deny", "/srv/example")
	if err != nil {
		t.Fatalf("deny error = %v", err)
	}
	if len(policy.Allowed) != 0 || strings.Join(policy.Denied, ",") != "/srv/example" {
		t.Fatalf("policy after deny = %#v, want only denied /srv/example", policy)
	}

	policy, err = UpdatePathConsentPolicy(teamDir, "remove", "/srv/example")
	if err != nil {
		t.Fatalf("remove error = %v", err)
	}
	if len(policy.Allowed) != 0 || len(policy.Denied) != 0 {
		t.Fatalf("policy after remove = %#v, want empty", policy)
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

func TestParseConsentInputPathAbsolute(t *testing.T) {
	tmpDir := t.TempDir()
	selection := parseConsentInput(filepath.Join(tmpDir, "docs"), "/ignored")
	if selection.kind != ConsentDenied {
		t.Fatalf("expected ConsentDenied, got %v", selection.kind)
	}
	if selection.suggestedPath != filepath.Clean(filepath.Join(tmpDir, "docs")) {
		t.Fatalf("unexpected suggested path: %q", selection.suggestedPath)
	}
}

func TestParseConsentInputPathRelative(t *testing.T) {
	tmpDir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("Chdir failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(orig)
	})

	if err := os.MkdirAll("docs", 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	selection := parseConsentInput("docs", "/ignored")
	if selection.kind != ConsentDenied {
		t.Fatalf("expected ConsentDenied, got %v", selection.kind)
	}
	want := filepath.Clean(filepath.Join(tmpDir, "docs")) + string(os.PathSeparator)
	if selection.suggestedPath != strings.TrimSuffix(want, string(os.PathSeparator)) {
		t.Fatalf("unexpected suggested path: %q want %q", selection.suggestedPath, strings.TrimSuffix(want, string(os.PathSeparator)))
	}
}

func TestPathConsentInteractiveAbortShortCircuits(t *testing.T) {
	interactiveAbortRequested.Store(true)
	defer interactiveAbortRequested.Store(false)

	pc := NewPathConsent()
	result, suggestion, err := pc.AskConsent("/tmp/does-not-matter", "read", "view", "args")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != ConsentDenied {
		t.Fatalf("expected ConsentDenied, got %v", result)
	}
	if suggestion != "" {
		t.Fatalf("expected empty suggestion, got %q", suggestion)
	}
}

func TestFormatConsentPreviewLinesWideEnough(t *testing.T) {
	prefix := "Agent: helper — "
	text := "Execute §6 Pre-failure Verification (Baseline) from the runbook."

	got := formatConsentPreviewLines(prefix, text, 200)
	if len(got) != 1 {
		t.Fatalf("expected one line, got %d: %#v", len(got), got)
	}
	want := prefix + text
	if got[0] != want {
		t.Fatalf("formatConsentPreviewLines() = %q, want %q", got[0], want)
	}
	if strings.Contains(got[0], "...") {
		t.Fatalf("did not expect ellipsis in %q", got[0])
	}
}

func TestFormatConsentPreviewLinesWrapsWithoutEllipsis(t *testing.T) {
	prefix := "Tool:  bash → "
	text := "go run ./cmd/pilot vm-target exec --name ipa-ha-client -- cat /etc/ssh/sshd_config"

	got := formatConsentPreviewLines(prefix, text, 48)
	if len(got) < 2 {
		t.Fatalf("expected wrapped output, got %#v", got)
	}
	for i, line := range got {
		if strings.Contains(line, "...") {
			t.Fatalf("line %d unexpectedly contains ellipsis: %q", i, line)
		}
		if gotWidth := utf8.RuneCountInString(line); gotWidth > 48 {
			t.Fatalf("line %d exceeds width: %d > 48 (%q)", i, gotWidth, line)
		}
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

func TestResolveAndValidatePathWithRememberedConsent(t *testing.T) {
	tmpDir := t.TempDir()
	workDir := filepath.Join(tmpDir, "project")
	consentedDir := filepath.Join(tmpDir, "shared")
	if err := os.MkdirAll(consentedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	consent := NewPathConsent()
	consent.mu.Lock()
	consent.remembered = append(consent.remembered, consentedDir+string(os.PathSeparator))
	consent.mu.Unlock()

	want := filepath.Join(consentedDir, "note.txt")
	path, err := resolveAndValidatePathWithConsent(want, ToolConfig{
		WorkDir:      workDir,
		AllowedPaths: []string{workDir},
		PathConsent:  consent,
	})
	if err != nil {
		t.Fatalf("resolveAndValidatePathWithConsent() error = %v", err)
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestConsentOutcomeString(t *testing.T) {
	cases := []struct {
		name   string
		result ConsentResult
		err    error
		want   string
	}{
		{"always", ConsentAlways, nil, "always"},
		{"once", ConsentOnce, nil, "once"},
		{"denied", ConsentDenied, nil, "denied"},
		{"error wins over result", ConsentAlways, errors.New("boom"), "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := consentOutcomeString(tc.result, tc.err); got != tc.want {
				t.Errorf("consentOutcomeString(%v, %v) = %q, want %q", tc.result, tc.err, got, tc.want)
			}
		})
	}
}

func TestConsentOutcomeStringTimeout(t *testing.T) {
	err := fmt.Errorf("%w after 2m0s", ErrConsentPromptTimeout)
	if got := consentOutcomeString(ConsentDenied, err); got != "timeout-denied" {
		t.Errorf("consentOutcomeString(timeout err) = %q, want %q", got, "timeout-denied")
	}
}

func TestAskConsentUnattendedFastDeny(t *testing.T) {
	SetProcessUnattended(true)
	t.Cleanup(func() { SetProcessUnattended(false) })

	pc := NewPathConsent()
	result, suggestion, err := pc.AskConsent("/etc/nowhere-special", "access", "sudo", "cat /etc/nowhere-special")
	if result != ConsentDenied {
		t.Fatalf("result = %v, want ConsentDenied", result)
	}
	if suggestion != "" {
		t.Fatalf("suggestion = %q, want empty", suggestion)
	}
	if err == nil || !strings.Contains(err.Error(), "--allow-path") {
		t.Fatalf("err = %v, want actionable --allow-path error", err)
	}
}

func TestConsentReadLineWithTimeout(t *testing.T) {
	release := make(chan string, 1)
	consentStdin.mu.Lock()
	consentStdin.ch = make(chan string, 1)
	consentStdin.pending = false
	consentStdin.readLine = func() string { return <-release }
	consentStdin.mu.Unlock()
	t.Cleanup(func() {
		consentStdin.mu.Lock()
		consentStdin.ch = nil
		consentStdin.pending = false
		consentStdin.readLine = nil
		consentStdin.mu.Unlock()
		close(release)
	})

	// Nobody answers: the read times out.
	line, ok := consentReadLineWithTimeout(50 * time.Millisecond)
	if ok || line != "" {
		t.Fatalf("expected timeout, got ok=%v line=%q", ok, line)
	}

	// The abandoned reader stays pending; a late answer is delivered to the
	// NEXT read instead of being lost or racing a second reader.
	release <- "a"
	line, ok = consentReadLineWithTimeout(2 * time.Second)
	if !ok || line != "a" {
		t.Fatalf("expected late line to be delivered, got ok=%v line=%q", ok, line)
	}

	// After delivery the pending flag clears, so a fresh read spawns.
	release <- "y"
	line, ok = consentReadLineWithTimeout(2 * time.Second)
	if !ok || line != "y" {
		t.Fatalf("expected fresh read, got ok=%v line=%q", ok, line)
	}
}

func TestDrainStaleConsentInput(t *testing.T) {
	consentStdin.mu.Lock()
	consentStdin.ch = make(chan string, 1)
	consentStdin.ch <- "stale"
	consentStdin.mu.Unlock()
	t.Cleanup(func() {
		consentStdin.mu.Lock()
		consentStdin.ch = nil
		consentStdin.pending = false
		consentStdin.readLine = nil
		consentStdin.mu.Unlock()
	})

	drainStaleConsentInput()

	select {
	case got := <-consentStdin.ch:
		t.Fatalf("stale line %q not drained", got)
	default:
	}
}

func TestConsentPromptTimeoutEnv(t *testing.T) {
	t.Setenv("HUFU_CONSENT_TIMEOUT", "7")
	if got := consentPromptTimeout(); got != 7*time.Second {
		t.Errorf("consentPromptTimeout() = %v, want 7s", got)
	}
	t.Setenv("HUFU_CONSENT_TIMEOUT", "0")
	if got := consentPromptTimeout(); got != 0 {
		t.Errorf("consentPromptTimeout() with 0 = %v, want 0 (wait forever)", got)
	}
	t.Setenv("HUFU_CONSENT_TIMEOUT", "junk")
	if got := consentPromptTimeout(); got != defaultConsentPromptTimeout {
		t.Errorf("consentPromptTimeout() with junk = %v, want default", got)
	}
}

// TestResolveAndValidatePathWithConsentAllowedOutsideWorkDir reproduces a real
// failure: an agent's allowed-paths entry lives outside the project WorkDir
// (a /tmp scratch directory a deployer agent writes helper scripts into).
// isPathAllowed correctly matches it, but the write/edit/download tools then
// re-validated with resolveAndValidatePath, which only knows about WorkDir
// and rejected the very path AllowedPaths had just approved.
func TestResolveAndValidatePathWithConsentAllowedOutsideWorkDir(t *testing.T) {
	tmpDir := t.TempDir()
	projectDir := filepath.Join(tmpDir, "project")
	scratchDir := filepath.Join(tmpDir, "scratch")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(project) error = %v", err)
	}
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(scratch) error = %v", err)
	}

	cfg := ToolConfig{
		WorkDir:      projectDir,
		AllowedPaths: []string{projectDir, scratchDir},
	}

	want := filepath.Join(scratchDir, "configure.sh")
	path, err := resolveAndValidatePathWithConsent(want, cfg)
	if err != nil {
		t.Fatalf("resolveAndValidatePathWithConsent() error = %v, want success for an allowed-paths entry outside WorkDir", err)
	}
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}
