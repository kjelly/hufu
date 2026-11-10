package buildinfo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	for _, tc := range []struct {
		name, release             string
		build                     *debug.BuildInfo
		version, revision, source string
		modified                  *bool
	}{
		{name: "missing metadata", release: "dev", version: "dev", source: "unknown"},
		{name: "release", release: "1.2.3", version: "1.2.3", source: "build_info"},
		{name: "module install", release: "dev", build: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, version: "v1.2.3", source: "build_info"},
		{name: "embedded revision", release: "dev", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "false"}}}, version: "dev", revision: "abc123", source: "build_info", modified: new(false)},
		{name: "dirty release", release: "1.2.3", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "true"}}}, version: "1.2.3", revision: "abc123", source: "build_info", modified: new(true)},
		{name: "unknown modified", release: "dev", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc123"}, {Key: "vcs.modified", Value: "invalid"}}}, version: "dev", revision: "abc123", source: "build_info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resolve(tc.release, tc.build, "github.com/kjelly/hufu/internal/buildinfo/buildinfo.go")
			if got.Version != tc.version || got.Revision != tc.revision || got.Source != tc.source || (got.Modified == nil) != (tc.modified == nil) || (got.Modified != nil && *got.Modified != *tc.modified) {
				t.Fatalf("identity = %+v, want %+v", got, tc)
			}
		})
	}
}

func TestSourceGit(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module github.com/kjelly/hufu\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "go.mod")
	runGit("-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
	head := runGit("rev-parse", "HEAD")
	source := filepath.Join(root, "internal", "buildinfo", "buildinfo.go")
	// Inherited Git redirections must not select the user's project.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "unrelated"))
	for _, dirty := range []bool{false, true} {
		if dirty {
			if err := os.WriteFile(filepath.Join(root, "untracked"), []byte("dirty"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := resolve("dev", nil, source)
		if got.Source != "runtime_git" || got.Revision != head || got.Modified == nil || *got.Modified != dirty {
			t.Fatalf("source identity = %+v, dirty=%t", got, dirty)
		}
		released := resolve("1.2.3", nil, source)
		if released.Revision != "" || released.Source != "build_info" {
			t.Fatalf("release read live checkout: %+v", released)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/subject\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := resolve("dev", nil, source); got.Source != "unknown" || got.Revision != "" {
		t.Fatalf("used subject identity: %+v", got)
	}
	if got := resolve("dev", nil, filepath.Join(root, "missing", "internal", "buildinfo", "buildinfo.go")); got.Source != "unknown" {
		t.Fatalf("guessed parent checkout: %+v", got)
	}
}

func TestBoundedOutput(t *testing.T) {
	var output boundedOutput
	if _, err := output.Write(bytes.Repeat([]byte{'x'}, 64<<10)); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{'x'}); err == nil {
		t.Fatal("accepted unbounded Git output")
	}
}
