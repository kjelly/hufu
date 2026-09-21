//go:build linux || darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestViewBatchReadsFilesAndArtifactsInOrder(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "first.txt")
	if err := os.WriteFile(filePath, []byte("zero\none\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := marshalViewInput(t, map[string]any{
		"items": []map[string]any{
			{"file_path": filePath, "offset": 1, "limit": 1},
			{"artifact_ref": "artifact-exact", "limit": 1},
		},
	})
	response, err := executeView(t.Context(), fantasy.ToolCall{Input: input}, dir, ToolConfig{
		AllowedPaths: []string{dir},
		ArtifactOpener: func(_ context.Context, ref string) (io.ReadCloser, error) {
			if ref != "artifact-exact" {
				t.Fatalf("artifact ref = %q", ref)
			}
			return io.NopCloser(strings.NewReader("artifact line\nsecond line\n")), nil
		},
	})
	if err != nil || response.IsError {
		t.Fatalf("batch response=%#v err=%v", response, err)
	}
	for _, want := range []string{
		`<batch_view status="ok" succeeded="2" failed="0" skipped="0">`,
		`<item index="0" status="ok">`,
		"2 one",
		`<item index="1" status="ok">`,
		"1 artifact line",
	} {
		if !strings.Contains(response.Content, want) {
			t.Errorf("batch response does not contain %q:\n%s", want, response.Content)
		}
	}
	if fileIndex, artifactIndex := strings.Index(response.Content, "2 one"), strings.Index(response.Content, "1 artifact line"); fileIndex < 0 || artifactIndex < fileIndex {
		t.Fatalf("batch results are not in input order: %q", response.Content)
	}
}

func TestViewBatchRejectsInvalidShapeBeforeReading(t *testing.T) {
	validItem := map[string]any{"file_path": "first.txt"}
	tests := []struct {
		name      string
		input     map[string]any
		wantError string
	}{
		{name: "empty", input: map[string]any{"items": []any{}}, wantError: "at least one"},
		{name: "mixed modes", input: map[string]any{"file_path": "single.txt", "items": []any{validItem}}, wantError: "cannot be combined"},
		{name: "missing item source", input: map[string]any{"items": []any{map[string]any{}}}, wantError: "items[0]: exactly one"},
		{name: "two item sources", input: map[string]any{"items": []any{map[string]any{"file_path": "a", "artifact_ref": "b"}}}, wantError: "items[0]: exactly one"},
	}
	overLimit := make([]map[string]any, maxBatchViewItems+1)
	for i := range overLimit {
		overLimit[i] = validItem
	}
	tests = append(tests, struct {
		name      string
		input     map[string]any
		wantError string
	}{name: "too many", input: map[string]any{"items": overLimit}, wantError: fmt.Sprintf("max %d", maxBatchViewItems)})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := executeView(t.Context(), fantasy.ToolCall{Input: marshalViewInput(t, tt.input)}, t.TempDir(), ToolConfig{})
			if err != nil || !response.IsError || !strings.Contains(response.Content, tt.wantError) {
				t.Fatalf("response=%#v err=%v, want error containing %q", response, err, tt.wantError)
			}
		})
	}
}

func TestViewBatchReportsPartialFailureWithoutExposingDeniedPath(t *testing.T) {
	allowedDir := t.TempDir()
	deniedDir := t.TempDir()
	allowedPath := filepath.Join(allowedDir, "allowed.txt")
	deniedPath := filepath.Join(deniedDir, "denied.txt")
	if err := os.WriteFile(allowedPath, []byte("allowed content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deniedPath, []byte("secret denied content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := marshalViewInput(t, map[string]any{
		"items": []map[string]any{
			{"file_path": allowedPath},
			{"file_path": deniedPath},
		},
	})
	response, err := executeView(t.Context(), fantasy.ToolCall{Input: input}, allowedDir, ToolConfig{AllowedPaths: []string{allowedDir}})
	if err != nil || response.IsError {
		t.Fatalf("partial batch response=%#v err=%v", response, err)
	}
	for _, want := range []string{`status="partial_failure"`, `succeeded="1"`, `failed="1"`, "allowed content", "outside allowed paths"} {
		if !strings.Contains(response.Content, want) {
			t.Errorf("partial response does not contain %q:\n%s", want, response.Content)
		}
	}
	if strings.Contains(response.Content, "secret denied content") {
		t.Fatalf("batch exposed denied file content: %s", response.Content)
	}
}

func TestViewBatchReportsErrorWhenEveryReadFails(t *testing.T) {
	dir := t.TempDir()
	input := marshalViewInput(t, map[string]any{
		"items": []map[string]any{
			{"file_path": filepath.Join(dir, "missing-one.txt")},
			{"file_path": filepath.Join(dir, "missing-two.txt")},
		},
	})
	response, err := executeView(t.Context(), fantasy.ToolCall{Input: input}, dir, ToolConfig{AllowedPaths: []string{dir}})
	if err != nil || !response.IsError {
		t.Fatalf("failed batch response=%#v err=%v", response, err)
	}
	for _, want := range []string{`status="error"`, `succeeded="0"`, `failed="2"`} {
		if !strings.Contains(response.Content, want) {
			t.Errorf("failed response does not contain %q:\n%s", want, response.Content)
		}
	}
}

func TestViewBatchBoundsAggregateOutput(t *testing.T) {
	dir := t.TempDir()
	largeContent := strings.Repeat(strings.Repeat("x", 200)+"\n", 2000)
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("file-%d.txt", i))
		content := largeContent
		if i == 2 {
			content = "must be skipped\n"
		}
		if err := os.WriteFile(paths[i], []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	input := marshalViewInput(t, map[string]any{
		"items": []map[string]any{
			{"file_path": paths[0]},
			{"file_path": paths[1]},
			{"file_path": paths[2]},
		},
	})
	response, err := executeView(t.Context(), fantasy.ToolCall{Input: input}, dir, ToolConfig{AllowedPaths: []string{dir}})
	if err != nil || response.IsError {
		t.Fatalf("bounded batch response=%#v err=%v", response, err)
	}
	for _, want := range []string{`status="partial_failure"`, `succeeded="1"`, `failed="1"`, `skipped="1"`, "batch output would exceed", "batch output limit was reached"} {
		if !strings.Contains(response.Content, want) {
			t.Errorf("bounded response does not contain %q", want)
		}
	}
	if strings.Contains(response.Content, "must be skipped") {
		t.Fatalf("batch read content after reaching aggregate limit")
	}
}

func TestViewBatchSchemaDescribesBoundedItems(t *testing.T) {
	items, ok := NewViewTool().Info().Parameters["items"].(map[string]any)
	if !ok {
		t.Fatalf("view items schema missing: %#v", NewViewTool().Info().Parameters)
	}
	if items["minItems"] != 1 || items["maxItems"] != maxBatchViewItems {
		t.Fatalf("view items bounds = min %#v max %#v", items["minItems"], items["maxItems"])
	}
	itemSchema, ok := items["items"].(map[string]any)
	if !ok || itemSchema["type"] != "object" {
		t.Fatalf("view item schema = %#v", items["items"])
	}
}

func marshalViewInput(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
