//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestViewArtifactReadMetadataExcludesFailedAndOmittedItems(t *testing.T) {
	for _, name := range []string{"partial", "oversized batch", "invalid range", "cancelled", "mixed"} {
		t.Run(name, func(t *testing.T) {
			content := "source evidence\n"
			input := `{"items":[{"artifact_ref":"good"},{"artifact_ref":"denied"}]}`
			want := []string{"good"}
			ctx := t.Context()
			dir := t.TempDir()
			if name == "oversized batch" {
				content = strings.Repeat(strings.Repeat("x", 499)+"\n", 1000)
				input = `{"items":[{"artifact_ref":"good","limit":1000},{"artifact_ref":"omitted","limit":1000}]}`
			}
			if name == "invalid range" {
				input = `{"artifact_ref":"good","byte_offset":999,"byte_limit":4}`
				want = nil
			}
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = nil
			}
			if name == "mixed" {
				path := filepath.Join(dir, "source.txt")
				if err := os.WriteFile(path, []byte("filesystem source"), 0o600); err != nil {
					t.Fatal(err)
				}
				input = fmt.Sprintf(`{"items":[{"artifact_ref":"good"},{"file_path":%q}]}`, path)
				want = nil
			}
			response, err := executeView(ctx, fantasy.ToolCall{Input: input}, dir, ToolConfig{AllowedPaths: []string{dir}, ArtifactOpener: func(_ context.Context, ref string) (io.ReadCloser, error) {
				if ref == "denied" {
					return nil, fmt.Errorf("not authorized")
				}
				return seekableArtifact{bytes.NewReader([]byte(content))}, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := ViewArtifactReads(response); !reflect.DeepEqual(got, want) {
				t.Fatalf("successful reads=%v want %v; %s", got, want, response.Content[:min(len(response.Content), 120)])
			}
		})
	}
}
