// Package buildinfo identifies the Hufu executable independently of its subject project.
package buildinfo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Version is set by the release linker. Local builds retain dev.
var Version = "dev"

// Info distinguishes embedded build facts from a source checkout observed at startup.
// Modified is nil when the build's working-tree state is unknown.
type Info struct {
	Version   string `json:"version"`
	Revision  string `json:"revision,omitempty"`
	Modified  *bool  `json:"modified,omitempty"`
	GoVersion string `json:"go_version"`
	Source    string `json:"source"`
}

// Current captures identity once per process, before any subject workspace changes.
var Current = sync.OnceValue(func() Info {
	info, _ := debug.ReadBuildInfo()
	_, file, _, _ := runtime.Caller(0)
	return resolve(Version, info, file)
})

func resolve(version string, build *debug.BuildInfo, sourceFile string) Info {
	result := Info{Version: version, GoVersion: runtime.Version(), Source: "unknown"}
	if build != nil {
		result.GoVersion = build.GoVersion
		if result.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			result.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				result.Revision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					result.Modified = new(setting.Value == "true")
				}
			}
		}
		if result.Revision != "" {
			result.Source = "build_info"
			return result
		}
	}
	// A released binary must never acquire the identity of a checkout left on disk.
	if result.Version != "dev" {
		result.Source = "build_info"
		return result
	}
	if revision, modified, ok := sourceGit(sourceFile); ok {
		result.Revision, result.Modified, result.Source = revision, new(modified), "runtime_git"
	}
	return result
}

func sourceGit(sourceFile string) (string, bool, bool) {
	// runtime.Caller can contain a module-relative name under -trimpath. Never
	// resolve that against cwd, which may be the user's unrelated project.
	if !filepath.IsAbs(sourceFile) {
		return "", false, false
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(sourceFile)))
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", false, false
	}
	fields := strings.Fields(string(module))
	if len(fields) < 2 || fields[0] != "module" || fields[1] != "github.com/kjelly/hufu" {
		return "", false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	top, err := git(ctx, root, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", false, false
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != strings.TrimSpace(top) {
		return "", false, false
	}
	revision, err := git(ctx, root, "rev-parse", "--verify", "HEAD")
	revision = strings.TrimSpace(revision)
	if err != nil || (len(revision) != 40 && len(revision) != 64) || strings.ContainsFunc(revision, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) {
		return "", false, false
	}
	status, err := git(ctx, root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return "", false, false
	}
	return revision, status != "", true
}

// boundedOutput keeps a large checkout from creating unbounded startup output.
type boundedOutput struct{ buffer bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.buffer.Len()+len(p) > 64<<10 {
		return 0, fmt.Errorf("git identity output exceeds limit")
	}
	return b.buffer.Write(p)
}

func git(ctx context.Context, root string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.fsmonitor=false", "-C", root}, args...)...)
	cmd.WaitDelay = 200 * time.Millisecond
	// Ignore inherited repository redirections, including GIT_DIR/GIT_WORK_TREE.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")
	var output boundedOutput
	cmd.Stdout = &output
	err := cmd.Run()
	return output.buffer.String(), err
}

func (i Info) String() string {
	label := i.Version
	if i.Revision != "" {
		label += "+" + i.Revision[:min(12, len(i.Revision))]
	}
	if i.Modified != nil && *i.Modified {
		label += ".dirty"
	}
	return label
}
