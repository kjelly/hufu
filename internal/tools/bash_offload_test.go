//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// recordingOffloader captures what a tool hands over and accepts or
// declines it.
type recordingOffloader struct {
	captures []ToolOutputCapture
	accept   bool
}

func (r *recordingOffloader) install(ctx context.Context) context.Context {
	return WithToolOutputOffloader(ctx, func(_ context.Context, capture ToolOutputCapture) (fantasy.ToolResponse, bool) {
		r.captures = append(r.captures, capture)
		if !r.accept {
			return fantasy.ToolResponse{}, false
		}
		return fantasy.NewTextResponse("offloaded"), true
	})
}

func TestBashHandsCompleteOutputToOffloader(t *testing.T) {
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, ".env"), []byte("OFFLOAD_TEST=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// seq 1 30000 is about 170 KB and 30000 lines, past both legacy bounds.
	var full strings.Builder
	for i := 1; i <= 30000; i++ {
		full.WriteString(strconv.Itoa(i))
		full.WriteByte('\n')
	}
	cases := []struct {
		name     string
		cfg      ToolConfig
		input    string
		exitCode int
	}{
		{name: "normal", input: `{"command":"seq 1 30000"}`},
		{name: "non-zero exit", input: `{"command":"seq 1 30000; exit 3"}`, exitCode: 3},
		{name: "restricted", cfg: ToolConfig{RestrictedBash: true}, input: `{"command":"seq 1 30000"}`},
		{name: "direnv", cfg: ToolConfig{Direnv: true}, input: `{"command":"seq 1 30000","working_directory":"` + workDir + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(bashCallCtx([]string{"bash"}), AgentAllowedPathsKey, []string{workDir})
			offloader := &recordingOffloader{accept: true}
			response, err := executeBash(offloader.install(ctx), fantasy.ToolCall{ID: "call-1", Name: "bash", Input: tc.input}, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(offloader.captures) != 1 || response.Content != "offloaded" {
				t.Fatalf("captures=%d response=%q", len(offloader.captures), response.Content)
			}
			capture := offloader.captures[0]
			want := assembleBashOutput(full.String(), "", tc.exitCode)
			if capture.ToolName != "bash" || capture.Content != want || capture.IsError != (tc.exitCode != 0) || !capture.LegacyWouldTruncate {
				t.Fatalf("capture tool=%q bytes=%d want=%d isError=%v legacy=%v", capture.ToolName, len(capture.Content), len(want), capture.IsError, capture.LegacyWouldTruncate)
			}
		})
	}
}

func TestBashFallsBackWhenOffloaderDeclines(t *testing.T) {
	offloader := &recordingOffloader{accept: false}
	ctx := offloader.install(bashCallCtx([]string{"bash"}))
	response, err := executeBash(ctx, fantasy.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"seq 1 30000; exit 2"}`}, ToolConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(offloader.captures) != 1 || !response.IsError || !strings.HasPrefix(response.Content, "[output truncated by hufu:") || !strings.HasSuffix(response.Content, "Exit code: 2") {
		t.Fatalf("legacy fallback response=%q...", response.Content[:min(len(response.Content), 120)])
	}
}

func TestShellPathsThatMustNotOffload(t *testing.T) {
	t.Run("sudo reroute", func(t *testing.T) {
		writeFakeSudo(t)
		offloader := &recordingOffloader{accept: true}
		ctx := offloader.install(bashCallCtx([]string{"bash", "sudo"}))
		response, err := executeBash(ctx, fantasy.ToolCall{ID: "call-1", Name: "bash", Input: `{"command":"sudo seq 1 30000"}`}, ApplyOptions(nil))
		if err != nil || response.IsError || len(offloader.captures) != 0 || !strings.Contains(response.Content, "routed through the sudo tool") {
			t.Fatalf("captures=%d response error=%v err=%v", len(offloader.captures), response.IsError, err)
		}
	})
	t.Run("non-offloadable caller", func(t *testing.T) {
		offloader := &recordingOffloader{accept: true}
		response, err := runShellCommand(offloader.install(context.Background()), 5*time.Second, "", false, false, "bash", []string{"-c", "seq 1 30000"}, nil)
		if err != nil || len(offloader.captures) != 0 || response.Content == "offloaded" {
			t.Fatalf("captures=%d err=%v", len(offloader.captures), err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		offloader := &recordingOffloader{accept: true}
		response, err := runShellCommand(offloader.install(context.Background()), 200*time.Millisecond, "", false, true, "bash", []string{"-c", "seq 1 30000; sleep 5"}, nil)
		if err != nil || len(offloader.captures) != 0 || !response.IsError || !strings.Contains(response.Content, "timed out") {
			t.Fatalf("captures=%d response=%q err=%v", len(offloader.captures), response.Content[:min(len(response.Content), 80)], err)
		}
	})
}
