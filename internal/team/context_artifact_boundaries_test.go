package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

// These captures include the exit footer. The non-truncating cases use short
// lines; truncating cases use one long line, matching bash's legacy bounds.
func TestContextArtifactProductionSizeBoundaries(t *testing.T) {
	policy := defaultContextArtifactPolicy()
	cases := []struct {
		name         string
		size         int
		legacy, want bool
	}{
		{"below preview", policy.PreviewBytes - 1, true, false},
		{"at preview", policy.PreviewBytes, true, false},
		{"above preview with loss", policy.PreviewBytes + 1, true, true},
		{"above preview without loss", policy.PreviewBytes + 1, false, false},
		{"below min without loss", policy.MinBytes - 1, false, false},
		{"below min with loss", policy.MinBytes - 1, true, true},
		{"at min without loss", policy.MinBytes, false, false},
		{"at min with loss", policy.MinBytes, true, true},
		{"above min without loss", policy.MinBytes + 1, false, true},
		{"below cap", policy.MaxArtifactBytes - 1, true, true},
		{"at cap", policy.MaxArtifactBytes, true, true},
		{"above cap", policy.MaxArtifactBytes + 1, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const footer = "\nExit code: 7"
			bodyBytes := tc.size - len(footer)
			body := strings.Repeat("x", bodyBytes)
			if !tc.legacy {
				line := strings.Repeat("x", 100) + "\n"
				body = strings.Repeat(line, bodyBytes/len(line)) + strings.Repeat("x", bodyBytes%len(line))
			}
			marked := []byte(body)
			copy(marked, "HEAD-7c1f")
			copy(marked[len(marked)/2:], "MIDDLE-8d2e")
			copy(marked[len(marked)-len("TAIL-9e3f"):], "TAIL-9e3f")
			content := string(marked) + footer
			f := newContextArtifactFixture(t, policy)
			svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
			ctx := f.attemptCtx(t, svc, f.task.ID, 1)
			response, ok := offloadThrough(ctx, "bash", "boundary", tools.ToolOutputCapture{
				ToolName: "bash", Content: content, IsError: true, LegacyWouldTruncate: tc.legacy,
			})
			if len(content) != tc.size || ok != tc.want {
				t.Fatalf("size=%d offload=%v, want %d/%v", len(content), ok, tc.size, tc.want)
			}
			events := contextArtifactEvents(t, f.c)
			if !ok {
				if len(events) != 0 || len(svc.refs) != 0 {
					t.Fatal("ineligible output exposed a reference")
				}
				return
			}
			matches := contextArtifactRefPattern.FindStringSubmatch(response.Content)
			if len(matches) != 2 || len(events) != 1 || !response.IsError {
				t.Fatal("publication lost reference, event, or command failure")
			}
			var published contextArtifactPublishedPayload
			if err := json.Unmarshal(events[0].Payload, &published); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256([]byte(content))
			if published.SHA256 != hex.EncodeToString(digest[:]) || published.Bytes != int64(tc.size) {
				t.Fatal("published digest or size differs from full capture")
			}
			rebuilt, _ := rebuildThroughView(t, f.c, ctx, matches[1])
			if rebuilt != content || !strings.HasSuffix(response.Content, footer) {
				t.Fatal("authorized byte-range reads did not preserve complete failed output")
			}
		})
	}
}

func TestBashSmallLongLineHonorsLegacyFallback(t *testing.T) {
	f := newContextArtifactFixture(t, defaultContextArtifactPolicy())
	svc := f.c.newContextArtifactService(f.task.ID, TaskDef{})
	ctx := f.attemptCtx(t, svc, f.task.ID, 1)
	bash := tools.NewBashTool(tools.WithWorkDir(f.workspace), tools.WithAllowedPaths([]string{f.workspace}))
	// 6000 ASCII bytes exceeds the 2000-rune line limit, but the full capture
	// including its exit footer is below the 8192-byte offload preview floor.
	response, err := bash.Run(svc.withToolOffloader(ctx, "bash", "small-long-line"), fantasy.ToolCall{
		ID: "small-long-line", Name: "bash", Input: fmt.Sprintf(`{"command":%q,"working_directory":%q}`, "printf x >> invocation-count; printf '%06000d' 0; exit 9", f.workspace),
	})
	if err != nil || !response.IsError || !strings.HasPrefix(response.Content, "[output truncated by hufu:") ||
		!strings.HasSuffix(response.Content, "Exit code: 9") || strings.Contains(response.Content, "artifact_ref=") {
		t.Fatalf("unexpected fallback: %q, %v", response.Content, err)
	}
	if len(svc.refs) != 0 || len(contextArtifactEvents(t, f.c)) != 0 {
		t.Fatal("output below preview unexpectedly published an artifact")
	}
	count, err := os.ReadFile(filepath.Join(f.workspace, "invocation-count"))
	if err != nil || string(count) != "x" {
		t.Fatalf("fallback repeated the original command: %q, %v", count, err)
	}
}
