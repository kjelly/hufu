//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestArtifactScopeFallbackTraversalMatchesPolicy(t *testing.T) {
	root := t.TempDir()
	ordinary := filepath.Join(root, "ordinary.go")
	blockedRoot := filepath.Join(root, "runtime-artifacts", "data")
	blocked := filepath.Join(blockedRoot, "blocked-unique-id")
	blockedMetaRoot := filepath.Join(root, "runtime-artifacts", "meta")
	blockedMeta := filepath.Join(blockedMetaRoot, "blocked-unique-id.json")
	for path, content := range map[string]string{
		ordinary:    "ordinary-unique-content",
		blocked:     "blocked-unique-content",
		blockedMeta: "blocked-unique-content",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(blocked, filepath.Join(root, "blocked-file-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blockedRoot, filepath.Join(root, "blocked-dir-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blockedMetaRoot, filepath.Join(root, "blocked-meta-alias")); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{blockedRoot, blockedMetaRoot}}

	ls, _, err := listDirectoryTreeWithPolicy(root, nil, -1, maxLSFiles, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range ls {
		if strings.Contains(entry.relPath, "blocked") || strings.Contains(entry.name, "blocked") {
			t.Fatalf("ls fallback leaked blocked entry: %#v", entry)
		}
	}

	paths, _, err := globWithWalkPolicy("**/*", root, defaultGlobLimit, "workspace", false, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Clean(paths[0]) != filepath.Clean(ordinary) {
		t.Fatalf("glob fallback paths=%v, want only %q", paths, ordinary)
	}

	candidates, err := collectAuthorizedFileCandidates(context.Background(), root, policy)
	if err != nil {
		t.Fatal(err)
	}
	response, err := grepFallbackCandidates(context.Background(), grepArgs{Pattern: "ordinary-unique-content", Literal: true}, root, "", defaultGrepLimit, "workspace", false, candidates)
	if err != nil || response.IsError || !strings.Contains(response.Content, "ordinary.go") || strings.Contains(response.Content, "blocked-unique") {
		t.Fatalf("grep fallback response=%#v err=%v", response, err)
	}

	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	ctx = SetToolsAllowed(ctx, []string{"ls", "glob", "grep"})
	for _, tool := range []fantasy.AgentTool{
		NewLsTool(WithWorkDir(root), WithAllowedPaths([]string{root})),
		NewGlobTool(WithWorkDir(root), WithAllowedPaths([]string{root})),
		NewGrepTool(WithWorkDir(root), WithAllowedPaths([]string{root})),
	} {
		if !IsTrustedArtifactPathTool(tool) {
			t.Fatalf("%s is not explicitly classified as artifact-policy safe", tool.Info().Name)
		}
		gated := tool
		response, runErr := gated.Run(ctx, fantasy.ToolCall{Name: tool.Info().Name, Input: `{"pattern":"ordinary-unique-content"}`})
		if tool.Info().Name == "ls" || tool.Info().Name == "glob" {
			response, runErr = gated.Run(ctx, fantasy.ToolCall{Name: tool.Info().Name, Input: `{"pattern":"**/*"}`})
		}
		if runErr != nil || response.IsError {
			t.Fatalf("%s response=%#v err=%v", tool.Info().Name, response, runErr)
		}
		if strings.Contains(response.Content, "blocked-unique") || strings.Contains(response.Content, "blocked-file-alias") || strings.Contains(response.Content, "blocked-dir-alias") {
			t.Fatalf("%s leaked blocked data: %q", tool.Info().Name, response.Content)
		}
	}
}

func TestGrepPolicySubtractsFromNativeRgSelection(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockedRoot := filepath.Join(root, "blocked")
	fixtures := map[string]string{
		"visible.go":        "native-selection-needle\n",
		"space name.txt":    "native-selection-needle\n",
		"line\nname.txt":    "native-selection-needle\n",
		".hidden.txt":       "native-selection-needle\n",
		"ignored.txt":       "native-selection-needle\n",
		"blocked/secret.go": "native-selection-needle\n",
	}
	for name, content := range fixtures {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	policy := &ArtifactPathPolicy{BlockedPaths: []string{blockedRoot}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"native-selection-needle"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if !strings.Contains(response.Content, "visible.go") || !strings.Contains(response.Content, "space name.txt") || !strings.Contains(response.Content, "line\nname.txt") {
		t.Fatalf("native visible files missing from response: %q", response.Content)
	}
	for _, unwanted := range []string{".hidden.txt", "ignored.txt", "blocked/secret.go"} {
		if strings.Contains(response.Content, unwanted) {
			t.Fatalf("native-excluded or blocked file %q leaked: %q", unwanted, response.Content)
		}
	}
}

func TestGrepNoPolicyPreservesNativeDirectorySelection(t *testing.T) {
	if _, err := exec.LookPath("rg"); err != nil {
		t.Skip("ripgrep is required for the native no-policy regression")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name := range map[string]struct{}{"visible.go": {}, ".hidden.txt": {}, "ignored.txt": {}} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("no-policy-needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	response, err := executeGrep(context.Background(), fantasy.ToolCall{Input: `{"pattern":"no-policy-needle"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError || !strings.Contains(response.Content, "visible.go") {
		t.Fatalf("response=%#v err=%v, want native visible selection", response, err)
	}
	if strings.Contains(response.Content, ".hidden.txt") || strings.Contains(response.Content, "ignored.txt") {
		t.Fatalf("no-policy direct search changed native selection: %q", response.Content)
	}
}

func TestGrepExplicitIgnoredFilePreservesDirectOperand(t *testing.T) {
	root := t.TempDir()
	ignored := filepath.Join(root, "ignored.txt")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ignored, []byte("explicit-ignored-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{filepath.Join(root, "unrelated-artifact")}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"explicit-ignored-needle","path":"` + ignored + `","include":"*.go"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError || !strings.Contains(response.Content, "explicit-ignored-needle") {
		t.Fatalf("response=%#v err=%v, want explicit ignored file match despite mismatching include", response, err)
	}
}

func TestGrepExplicitIgnoredDirectorySubtractsBlockedDescendants(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "ignored-dir")
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored-dir/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(dir, ".hidden.txt"):    "explicit-directory-hidden-needle\n",
		filepath.Join(dir, "visible.txt"):    "explicit-directory-visible-needle\n",
		filepath.Join(blocked, "secret.txt"): "explicit-directory-blocked-needle\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hiddenDir := filepath.Join(root, ".hidden-dir")
	hiddenBlocked := filepath.Join(hiddenDir, "blocked")
	for path, content := range map[string]string{
		filepath.Join(hiddenDir, "visible.txt"):    "hidden-directory-visible-needle\n",
		filepath.Join(hiddenDir, ".hidden.txt"):    "hidden-directory-hidden-needle\n",
		filepath.Join(hiddenBlocked, "secret.txt"): "hidden-directory-blocked-needle\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{blocked, hiddenBlocked}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	input := `{"pattern":"explicit-directory-","path":"` + dir + `"}`
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: input}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	if !strings.Contains(response.Content, "visible.txt") {
		t.Fatalf("explicit ignored directory result missing visible.txt: %q", response.Content)
	}
	if strings.Contains(response.Content, ".hidden.txt") || strings.Contains(response.Content, "blocked-needle") || strings.Contains(response.Content, "secret.txt") {
		t.Fatalf("ignored-directory native exclusions or blocked descendant leaked: %q", response.Content)
	}

	hiddenInput := `{"pattern":"hidden-directory-","path":"` + hiddenDir + `"}`
	hiddenResponse, err := executeGrep(ctx, fantasy.ToolCall{Input: hiddenInput}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || hiddenResponse.IsError || !strings.Contains(hiddenResponse.Content, "visible.txt") {
		t.Fatalf("explicit hidden directory result=%#v err=%v, want native visible child", hiddenResponse, err)
	}
	if strings.Contains(hiddenResponse.Content, ".hidden.txt") || strings.Contains(hiddenResponse.Content, "blocked-needle") || strings.Contains(hiddenResponse.Content, "secret.txt") {
		t.Fatalf("hidden-directory native exclusions or blocked descendant leaked: %q", hiddenResponse.Content)
	}
	for _, want := range []string{"visible.txt"} {
		if !strings.Contains(response.Content, want) {
			t.Fatalf("explicit directory result missing %q: %q", want, response.Content)
		}
	}
}

func TestGrepPolicyFallbackUsesGnuNativeEnumeration(t *testing.T) {
	root := t.TempDir()
	forceGrepFallback(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(root, "blocked.txt")
	for name, content := range map[string]string{
		"visible.go":  "gnu-fallback-needle\n",
		".hidden.txt": "gnu-fallback-needle\n",
		"ignored.txt": "gnu-fallback-needle\n",
		"blocked.txt": "gnu-fallback-needle\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{blocked}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"gnu-fallback-needle","include":"*.txt"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	for _, want := range []string{".hidden.txt", "ignored.txt"} {
		if !strings.Contains(response.Content, want) {
			t.Fatalf("GNU fallback omitted native %q: %q", want, response.Content)
		}
	}
	if strings.Contains(response.Content, "blocked.txt") || strings.Contains(response.Content, "visible.go") {
		t.Fatalf("GNU fallback returned blocked or include-mismatching file: %q", response.Content)
	}

	explicitResponse, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"gnu-fallback-needle","path":"` + filepath.Join(root, "ignored.txt") + `","include":"*.go"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || explicitResponse.IsError || !strings.Contains(explicitResponse.Content, "No matches found") {
		t.Fatalf("GNU fallback explicit file response=%#v err=%v, want native include filtering", explicitResponse, err)
	}
}

func TestGrepFallbackExcludesNestedSymlinkRegularFile(t *testing.T) {
	root := t.TempDir()
	forceGrepFallback(t)

	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("nested-symlink-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "outside-alias.txt")); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{filepath.Join(root, "unrelated-artifact")}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"nested-symlink-needle"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError || !strings.Contains(response.Content, "No matches found") {
		t.Fatalf("response=%#v err=%v, want nested symlink file excluded", response, err)
	}
}

func TestGrepFallbackFollowsExplicitSymlinkDirectoryRoot(t *testing.T) {
	root := t.TempDir()
	forceGrepFallback(t)

	target := filepath.Join(root, "real-root")
	file := filepath.Join(target, "nested", "visible.txt")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("explicit-symlink-root-needle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "dir-alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{filepath.Join(root, "unrelated-artifact")}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	input := `{"pattern":"explicit-symlink-root-needle","path":"` + alias + `"}`
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: input}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError || !strings.Contains(response.Content, "dir-alias") || !strings.Contains(response.Content, "explicit-symlink-root-needle") {
		t.Fatalf("response=%#v err=%v, want recursive explicit symlink-directory result", response, err)
	}
}

func TestGrepFallbackRejectsOutsideAndBlockedSymlinkAliases(t *testing.T) {
	root := t.TempDir()
	forceGrepFallback(t)

	outside := filepath.Join(t.TempDir(), "outside.txt")
	blockedRoot := filepath.Join(root, "runtime-artifacts")
	blocked := filepath.Join(blockedRoot, "secret.txt")
	for path := range map[string]struct{}{outside: {}, blocked: {}} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("alias-bypass-needle\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside-alias.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(blocked, filepath.Join(root, "blocked-alias.txt")); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{blockedRoot}}
	ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
	response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"alias-bypass-needle"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
	if err != nil || response.IsError || !strings.Contains(response.Content, "No matches found") || strings.Contains(response.Content, "alias-bypass-needle") {
		t.Fatalf("response=%#v err=%v, want outside and blocked aliases excluded", response, err)
	}
}

func forceGrepFallback(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	grepPath, err := exec.LookPath("grep")
	if err != nil {
		t.Skip("GNU grep is required for the fallback regression")
	}
	if err := os.Symlink(grepPath, filepath.Join(bin, "grep")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
}

func TestGrepEmptyAuthorizedCandidatesNeverSearchesRoot(t *testing.T) {
	root := t.TempDir()
	ordinary := filepath.Join(root, "ordinary.go")
	blockedRoot := filepath.Join(root, "runtime-artifacts", "data")
	blocked := filepath.Join(blockedRoot, "blocked-only.secret")
	if err := os.MkdirAll(blockedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ordinary, []byte("permitted-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("blocked-root-unique-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := &ArtifactPathPolicy{BlockedPaths: []string{blockedRoot}}

	tests := []struct {
		name  string
		input string
	}{
		{name: "default root blocked only", input: `{"pattern":"blocked-root-unique-content"}`},
		{name: "explicit root blocked only", input: `{"pattern":"blocked-root-unique-content","path":"` + root + `"}`},
		{name: "default root include excludes permitted", input: `{"pattern":"blocked-root-unique-content","include":"*.secret"}`},
		{name: "explicit root include excludes permitted", input: `{"pattern":"blocked-root-unique-content","include":"*.secret","path":"` + root + `"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
			response, err := executeGrep(ctx, fantasy.ToolCall{Input: tt.input}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
			if err != nil || response.IsError || !strings.Contains(response.Content, "No matches found") || strings.Contains(response.Content, "blocked-root-unique-content") {
				t.Fatalf("response=%#v err=%v, want no search of blocked root", response, err)
			}
		})
	}

	if candidates, err := collectAuthorizedFileCandidates(context.Background(), root, policy); err != nil {
		t.Fatal(err)
	} else if candidates == nil || len(candidates) != 1 || candidates[0] != ordinary {
		t.Fatalf("authorized candidates=%v, want non-nil ordinary-only set", candidates)
	}
	empty := make([]string, 0)
	args := grepArgs{Pattern: "blocked-root-unique-content", Include: "*.secret"}
	for _, backend := range []struct {
		name string
		run  func() (fantasy.ToolResponse, error)
	}{
		{name: "rg", run: func() (fantasy.ToolResponse, error) {
			return grepWithRgCandidates(context.Background(), args, root, args.Include, defaultGrepLimit, "workspace", false, empty)
		}},
		{name: "fallback", run: func() (fantasy.ToolResponse, error) {
			return grepFallbackCandidates(context.Background(), args, root, args.Include, defaultGrepLimit, "workspace", false, empty)
		}},
	} {
		t.Run(backend.name, func(t *testing.T) {
			response, err := backend.run()
			if err != nil || response.IsError || !strings.Contains(response.Content, "No matches found") || strings.Contains(response.Content, "blocked-root-unique-content") {
				t.Fatalf("response=%#v err=%v, want empty-candidate no-match result", response, err)
			}
		})
	}
}

func TestGrepLeadingDashPatternIsDataForNativeAndFallbackBackends(t *testing.T) {
	backends := []struct {
		name     string
		fallback bool
	}{
		{name: "native"},
		{name: "fallback", fallback: true},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			root := t.TempDir()
			if backend.fallback {
				forceGrepFallback(t)
			} else if _, err := exec.LookPath("rg"); err != nil {
				t.Skip("ripgrep is required for the native leading-dash regression")
			}
			match := filepath.Join(root, "match.txt")
			if err := os.WriteFile(match, []byte("-leading-dash needle\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			response, err := executeGrep(context.Background(), fantasy.ToolCall{Input: `{"pattern":"-leading-dash"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
			if err != nil || response.IsError || !strings.Contains(response.Content, "match.txt") {
				t.Fatalf("response=%#v err=%v, want leading-dash pattern match", response, err)
			}
		})
	}
}

func TestGrepLeadingDashPatternSurvivesPolicyCandidateSelection(t *testing.T) {
	backends := []struct {
		name     string
		fallback bool
	}{
		{name: "native", fallback: false},
		{name: "fallback", fallback: true},
	}
	for _, backend := range backends {
		t.Run(backend.name, func(t *testing.T) {
			root := t.TempDir()
			if backend.fallback {
				forceGrepFallback(t)
			} else if _, err := exec.LookPath("rg"); err != nil {
				t.Skip("ripgrep is required for the native policy regression")
			}
			blocked := filepath.Join(root, "blocked")
			if err := os.MkdirAll(blocked, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "allowed.txt"), []byte("-policy-leading-dash\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocked, "secret.txt"), []byte("-policy-leading-dash\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			policy := &ArtifactPathPolicy{BlockedPaths: []string{blocked}}
			ctx := context.WithValue(context.Background(), ArtifactPathPolicyKey, *policy)
			response, err := executeGrep(ctx, fantasy.ToolCall{Input: `{"pattern":"-policy-leading-dash"}`}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
			if err != nil || response.IsError || !strings.Contains(response.Content, "allowed.txt") || strings.Contains(response.Content, "secret.txt") {
				t.Fatalf("response=%#v err=%v, want allowed leading-dash match without blocked candidate", response, err)
			}
		})
	}
}
