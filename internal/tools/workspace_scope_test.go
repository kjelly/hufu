//go:build linux || darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

type spoofedScopedTool struct{}

func (spoofedScopedTool) Info() fantasy.ToolInfo                      { return fantasy.ToolInfo{Name: "view"} }
func (spoofedScopedTool) ProviderOptions() fantasy.ProviderOptions    { return fantasy.ProviderOptions{} }
func (*spoofedScopedTool) SetProviderOptions(fantasy.ProviderOptions) {}
func (spoofedScopedTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("ok"), nil
}

func TestDescribeToolWorkspaceScopeUsesConcreteDescriptor(t *testing.T) {
	view := DescribeToolWorkspaceScope(NewViewTool())
	if !view.MayReadWorkspace || view.ReadBehavior != PathScopeEnforced || view.MayWriteWorkspace {
		t.Fatalf("view descriptor = %#v", view)
	}
	write := DescribeToolWorkspaceScope(NewWriteTool())
	if !write.MayReadWorkspace || !write.MayWriteWorkspace || write.ReadBehavior != PathScopeEnforced || write.WriteBehavior != PathScopeEnforced {
		t.Fatalf("write descriptor = %#v", write)
	}
	spoof := DescribeToolWorkspaceScope(&spoofedScopedTool{})
	if !spoof.MayReadWorkspace || !spoof.MayWriteWorkspace || spoof.ReadBehavior != PathScopeUnsupported || spoof.WriteBehavior != PathScopeUnsupported {
		t.Fatalf("spoof descriptor = %#v", spoof)
	}
}

func TestTaskPathScopeIntersectionPreservesRequestedScope(t *testing.T) {
	root := t.TempDir()
	requested := filepath.Join(root, "docs") + string(filepath.Separator)
	got, err := IntersectPathScopeCeilings([]string{requested}, []string{root}, []string{filepath.Join(root, "docs")})
	if err != nil || len(got) != 1 || got[0] != requested {
		t.Fatalf("intersection = %#v, %v", got, err)
	}
	if _, err := IntersectPathScopeCeilings([]string{root}, []string{requested}); err == nil {
		t.Fatal("configured descendant authorized requested ancestor")
	}
	if _, err := IntersectPathScopeCeilings([]string{}); err == nil {
		t.Fatal("explicit empty scope was accepted")
	}
}

func TestCfgWithMergedPathsIntersectsConfiguredRuntimeAndTaskScopes(t *testing.T) {
	root := t.TempDir()
	requested := filepath.Join(root, "docs", "a.md")
	cfg := ToolConfig{
		AllowedPaths:      []string{root},
		AllowedWritePaths: []string{filepath.Join(root, "docs")},
	}
	ctx := context.WithValue(t.Context(), AgentAllowedPathsKey, []string{filepath.Join(root, "runtime")})
	ctx = context.WithValue(ctx, AgentAllowedWritePathsKey, []string{filepath.Join(root, "docs")})
	ctx = context.WithValue(ctx, AgentTaskPathScopeKey, AgentTaskPathScope{
		ReadPaths: []string{requested}, WritePaths: []string{requested},
		ReadBounded: true, WriteBounded: true, Digest: "merged-scope",
	})
	merged := cfgWithMergedPaths(cfg, ctx)
	if merged.TaskPathScopeError != nil {
		t.Fatal(merged.TaskPathScopeError)
	}
	if len(merged.AllowedPaths) != 1 || merged.AllowedPaths[0] != requested || len(merged.AllowedWritePaths) != 1 || merged.AllowedWritePaths[0] != requested {
		t.Fatalf("merged paths = read %#v, write %#v", merged.AllowedPaths, merged.AllowedWritePaths)
	}

	disjoint := context.WithValue(t.Context(), AgentAllowedWritePathsKey, []string{filepath.Join(root, "internal")})
	disjoint = context.WithValue(disjoint, AgentTaskPathScopeKey, AgentTaskPathScope{
		ReadPaths: []string{requested}, WritePaths: []string{requested},
		ReadBounded: true, WriteBounded: true, Digest: "disjoint-scope",
	})
	if got := cfgWithMergedPaths(cfg, disjoint); got.TaskPathScopeError == nil {
		t.Fatal("disjoint runtime write ceiling was accepted")
	}
}

func scopedToolTestContext(root, relative string, write bool) context.Context {
	path := filepath.Join(root, relative)
	scope := AgentTaskPathScope{ReadPaths: []string{path}, ReadBounded: true, Digest: "test-scope"}
	if write {
		scope.WritePaths = []string{path}
		scope.WriteBounded = true
	}
	return context.WithValue(context.Background(), AgentTaskPathScopeKey, scope)
}

func TestEnforcedFileToolsDenyPathEscape(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "allowed.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := ToolConfig{WorkDir: root, AllowedPaths: []string{root}}
	readCtx := scopedToolTestContext(root, "allowed.txt", false)
	writeCtx := scopedToolTestContext(root, "allowed.txt", true)
	calls := []struct {
		name string
		call func() (fantasy.ToolResponse, error)
	}{
		{"view", func() (fantasy.ToolResponse, error) {
			return executeView(readCtx, fantasy.ToolCall{Input: `{"file_path":"outside.txt"}`}, root, cfg)
		}},
		{"write", func() (fantasy.ToolResponse, error) {
			return executeWrite(writeCtx, fantasy.ToolCall{Input: `{"file_path":"outside.txt","content":"changed"}`}, cfg)
		}},
		{"edit", func() (fantasy.ToolResponse, error) {
			return executeEdit(writeCtx, fantasy.ToolCall{Input: `{"file_path":"outside.txt","old_string":"outside","new_string":"changed"}`}, cfg)
		}},
		{"multiedit", func() (fantasy.ToolResponse, error) {
			return executeMultiEdit(writeCtx, fantasy.ToolCall{Input: `{"file_path":"outside.txt","edits":[{"old_string":"outside","new_string":"changed"}]}`}, cfg)
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			response, err := tc.call()
			if err != nil || !response.IsError || !strings.Contains(response.Content, "outside the admitted task scope") {
				t.Fatalf("response = %#v, err = %v", response, err)
			}
		})
	}
	data, err := os.ReadFile(filepath.Join(root, "outside.txt"))
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside file = %q, %v", data, err)
	}
}

func TestScopedWriteReplacesFinalSymlinkWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(root, "target.txt")); err != nil {
		t.Fatal(err)
	}
	cfg := ToolConfig{WorkDir: root, AllowedPaths: []string{root}}
	response, err := executeWrite(scopedToolTestContext(root, "target.txt", true), fantasy.ToolCall{Input: `{"file_path":"target.txt","content":"inside"}`}, cfg)
	if err != nil || response.IsError {
		t.Fatalf("write response = %#v, err = %v", response, err)
	}
	outsideData, _ := os.ReadFile(sentinel)
	insideData, insideErr := os.ReadFile(filepath.Join(root, "target.txt"))
	if string(outsideData) != "outside" || insideErr != nil || string(insideData) != "inside" {
		t.Fatalf("outside=%q inside=%q err=%v", outsideData, insideData, insideErr)
	}
}

func TestScopedEditRejectsConcurrentDestinationReplacement(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "target.txt")
	if err := os.WriteFile(path, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMergedPaths(ToolConfig{WorkDir: root, AllowedPaths: []string{root}}, scopedToolTestContext(root, "target.txt", true))
	access, err := newScopedFileAccess(cfg, "target.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	defer access.close()
	_, identity, err := access.readRegular()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(root, "old.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := access.writeAtomic(t.Context(), []byte("edited"), identity, identity.Mode()); err == nil {
		t.Fatal("concurrent replacement was overwritten")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "replacement" {
		t.Fatalf("replacement changed to %q", data)
	}
}

func TestScopedFileAccessParentSymlinkRaceCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	slot := filepath.Join(root, "slot")
	parked := filepath.Join(root, "parked")
	if err := os.Mkdir(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), AgentTaskPathScopeKey, AgentTaskPathScope{
		ReadPaths: []string{slot + string(filepath.Separator)}, WritePaths: []string{slot + string(filepath.Separator)},
		ReadBounded: true, WriteBounded: true, Digest: "race-scope",
	})
	cfg := ToolConfig{WorkDir: root, AllowedPaths: []string{root}}
	stop := make(chan struct{})
	var swapWG sync.WaitGroup
	swapWG.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if os.Rename(slot, parked) == nil {
				_ = os.Symlink(outside, slot)
				_ = os.Remove(slot)
				_ = os.Rename(parked, slot)
			}
		}
	})
	for i := range 100 {
		input := fmt.Sprintf(`{"file_path":"slot/value.txt","content":"inside-%d"}`, i)
		_, _ = executeWrite(ctx, fantasy.ToolCall{Input: input}, cfg)
		view, _ := executeView(ctx, fantasy.ToolCall{Input: `{"file_path":"slot/value.txt"}`}, root, cfg)
		if strings.Contains(view.Content, "outside") {
			close(stop)
			swapWG.Wait()
			t.Fatal("scoped view observed outside-root content")
		}
	}
	close(stop)
	swapWG.Wait()
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "outside" {
		t.Fatalf("outside sentinel = %q, %v", data, err)
	}
}
