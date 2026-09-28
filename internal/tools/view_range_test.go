//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"charm.land/fantasy"
)

type seekableArtifact struct{ *bytes.Reader }

func (seekableArtifact) Close() error { return nil }

func seekableOpener(data []byte) ArtifactOpener {
	return func(context.Context, string) (io.ReadCloser, error) {
		return seekableArtifact{bytes.NewReader(data)}, nil
	}
}

var viewRangeHeader = regexp.MustCompile(`^<artifact:ref-a bytes=\[(\d+),(\d+)\) total=(\d+)>\n`)

// readViewRange runs one byte-range view and returns the chunk and the next
// offset, or -1 at the end of the artifact.
func readViewRange(t *testing.T, ctx context.Context, cfg ToolConfig, input string) (string, int) {
	t.Helper()
	response, err := executeView(ctx, fantasy.ToolCall{Input: input}, t.TempDir(), cfg)
	if err != nil || response.IsError {
		t.Fatalf("view %s: response=%q err=%v", input, response.Content, err)
	}
	match := viewRangeHeader.FindStringSubmatch(response.Content)
	if match == nil {
		t.Fatalf("missing range header: %q", response.Content)
	}
	body := strings.TrimPrefix(response.Content, match[0])
	closing := strings.LastIndex(body, "\n</artifact:ref-a>\n")
	if closing < 0 {
		t.Fatalf("missing closing tag: %q", response.Content)
	}
	chunk, trailer := body[:closing], body[closing+len("\n</artifact:ref-a>\n"):]
	start, _ := strconv.Atoi(match[1])
	end, _ := strconv.Atoi(match[2])
	if end-start != len(chunk) {
		t.Fatalf("header range [%d,%d) does not match %d chunk bytes", start, end, len(chunk))
	}
	if trailer == "[end of artifact]" {
		return chunk, -1
	}
	if trailer != fmt.Sprintf("[next byte_offset=%d]", end) {
		t.Fatalf("trailer = %q", trailer)
	}
	return chunk, end
}

func TestViewArtifactByteRangesRebuildContentExactly(t *testing.T) {
	// Multi-byte runes land on chunk boundaries for several limits.
	content := []byte(strings.Repeat("ascii line\n漢字テキスト🙂 mixed\n", 400))
	for _, limit := range []int{97, 1024, 4093, 32768} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			ctx := WithArtifactMaxReadBytes(context.Background(), 32768)
			cfg := ToolConfig{ArtifactOpener: seekableOpener(content)}
			var rebuilt strings.Builder
			for offset := 0; offset >= 0; {
				chunk, next := readViewRange(t, ctx, cfg, fmt.Sprintf(`{"artifact_ref":"ref-a","byte_offset":%d,"byte_limit":%d}`, offset, limit))
				rebuilt.WriteString(chunk)
				if next >= 0 && next <= offset {
					t.Fatalf("no progress at offset %d", offset)
				}
				offset = next
			}
			if rebuilt.String() != string(content) {
				t.Fatal("rebuilt content differs from the artifact")
			}
		})
	}
}

func TestViewArtifactByteRangeErrors(t *testing.T) {
	content := []byte("ab漢cd")
	ctx := WithArtifactMaxReadBytes(context.Background(), 16)
	cfg := ToolConfig{ArtifactOpener: seekableOpener(content)}
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "negative offset", input: `{"artifact_ref":"ref-a","byte_offset":-1}`, want: "byte_offset must be non-negative"},
		{name: "zero limit", input: `{"artifact_ref":"ref-a","byte_limit":0}`, want: "byte_limit must be positive"},
		{name: "limit above maximum", input: `{"artifact_ref":"ref-a","byte_limit":17}`, want: "exceeds the maximum of 16 bytes"},
		{name: "offset beyond end", input: `{"artifact_ref":"ref-a","byte_offset":9}`, want: "beyond the end of the artifact (7 bytes)"},
		{name: "offset inside a rune", input: `{"artifact_ref":"ref-a","byte_offset":3}`, want: "use byte_offset 2"},
		{name: "mixed with line offset", input: `{"artifact_ref":"ref-a","byte_offset":0,"offset":2}`, want: "cannot be combined with offset/limit"},
		{name: "file path", input: `{"file_path":"x.txt","byte_offset":0}`, want: "apply only to artifact_ref"},
		{name: "items with top-level range", input: `{"byte_limit":4,"items":[{"artifact_ref":"ref-a"}]}`, want: "items cannot be combined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := executeView(ctx, fantasy.ToolCall{Input: tc.input}, t.TempDir(), cfg)
			if err != nil || !response.IsError || !strings.Contains(response.Content, tc.want) {
				t.Fatalf("response=%q err=%v, want error containing %q", response.Content, err, tc.want)
			}
		})
	}

	t.Run("split rune at the chunk end is trimmed", func(t *testing.T) {
		chunk, next := readViewRange(t, ctx, cfg, `{"artifact_ref":"ref-a","byte_limit":3}`)
		if chunk != "ab" || next != 2 {
			t.Fatalf("chunk=%q next=%d", chunk, next)
		}
	})
	t.Run("offset at end", func(t *testing.T) {
		if chunk, next := readViewRange(t, ctx, cfg, `{"artifact_ref":"ref-a","byte_offset":7}`); chunk != "" || next != -1 {
			t.Fatalf("chunk=%q next=%d", chunk, next)
		}
	})
	t.Run("invalid utf-8", func(t *testing.T) {
		binary := ToolConfig{ArtifactOpener: seekableOpener([]byte{'a', 0xff, 0xfe, 'b'})}
		response, _ := executeView(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"ref-a","byte_limit":4}`}, t.TempDir(), binary)
		if !response.IsError || !strings.Contains(response.Content, "not UTF-8 text") {
			t.Fatalf("response=%q", response.Content)
		}
	})
	t.Run("non-seekable opener", func(t *testing.T) {
		plain := ToolConfig{ArtifactOpener: func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("abc")), nil
		}}
		response, _ := executeView(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"ref-a","byte_limit":2}`}, t.TempDir(), plain)
		if !response.IsError || !strings.Contains(response.Content, "does not support byte-range reads") {
			t.Fatalf("response=%q", response.Content)
		}
	})
	t.Run("batch item", func(t *testing.T) {
		response, err := executeView(ctx, fantasy.ToolCall{Input: `{"items":[{"artifact_ref":"ref-a","byte_limit":2},{"artifact_ref":"ref-a","byte_offset":2,"byte_limit":3}]}`}, t.TempDir(), cfg)
		if err != nil || response.IsError || !strings.Contains(response.Content, "bytes=[0,2)") || !strings.Contains(response.Content, "bytes=[2,5)") {
			t.Fatalf("response=%q err=%v", response.Content, err)
		}
	})
	t.Run("default limit is the context maximum", func(t *testing.T) {
		if chunk, next := readViewRange(t, context.Background(), cfg, `{"artifact_ref":"ref-a","byte_offset":0}`); chunk != string(content) || next != -1 {
			t.Fatalf("chunk=%q next=%d", chunk, next)
		}
	})
}
