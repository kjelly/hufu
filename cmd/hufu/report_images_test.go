package main

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

func TestImageReportKeepsOpaqueReferencesAndResultCommands(t *testing.T) {
	id := "res-" + strings.Repeat("a", 64)
	result := &team.TaskResult{RuntimeMedia: &team.RuntimeMediaProjection{Sources: []team.RuntimeMediaSource{{Parts: []agent.ToolMediaPart{
		{Type: "image", Image: media.HistoryReference{Ref: media.Ref{ID: "opaque-image", MediaType: "image/png", Width: 2, Height: 3, ByteSize: 123}}},
	}}}}, Artifacts: []team.ArtifactRef{{Path: "/private/cas/must-not-read"}}}
	data := &reportData{ReportSource: &team.RunReport{Team: "review", ImageResultIDs: map[string]string{"1": id}}, Todos: []*team.TodoItem{{ID: "1", TypedResult: result}}}
	var out strings.Builder
	writeImageResultReport(&out, data)
	for _, required := range []string{"opaque-image", "image/png", "2x3", "hufu results images " + id, "--artifact <image-id> --export <new-path>"} {
		if !strings.Contains(out.String(), required) {
			t.Fatalf("image report omitted %s", required)
		}
	}
	if strings.Contains(out.String(), "/private/cas") || strings.Contains(out.String(), "base64") {
		t.Fatal("image report exposed storage or inline image data")
	}
	var plain strings.Builder
	writeImageResultReport(&plain, &reportData{})
	if plain.Len() != 0 {
		t.Fatal("text-only report acquired an image section")
	}
}
