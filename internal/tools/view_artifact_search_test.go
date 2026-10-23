//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
)

type searchArtifactReader struct {
	*bytes.Reader
	closed *bool
}

func (r searchArtifactReader) Close() error { *r.closed = true; return nil }

func runArtifactSearch(t *testing.T, ctx context.Context, data []byte, args map[string]any) (fantasy.ToolResponse, int, bool) {
	t.Helper()
	opened := 0
	closed := false
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ToolConfig{ArtifactOpener: func(_ context.Context, id string) (io.ReadCloser, error) {
		opened++
		if id != "allowed" {
			return nil, fmt.Errorf("denied")
		}
		return searchArtifactReader{bytes.NewReader(data), &closed}, nil
	}}
	response, err := executeView(ctx, fantasy.ToolCall{Input: string(input)}, t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return response, opened, closed
}

func TestArtifactSearchLocatesLargeEvidenceWithoutReturningWholeContent(t *testing.T) {
	prefix := "UNSEEN sentinel\n" + strings.Repeat("source padding\n", 30000)
	needle := "literal+a[0].*界"
	data := []byte(prefix + needle + strings.Repeat("尾", 400))
	response, opened, closed := runArtifactSearch(t, t.Context(), data, map[string]any{"artifact_ref": "allowed", "search_text": needle, "context_bytes": 31})
	if response.IsError || opened != 1 || !closed {
		t.Fatalf("search=%#v opened=%d closed=%t", response, opened, closed)
	}
	if !strings.Contains(response.Content, fmt.Sprintf("<match byte_offset=%d ", len(prefix))) || !strings.Contains(response.Content, needle) || strings.Contains(response.Content, "UNSEEN sentinel") || !utf8.ValidString(response.Content) || len(response.Content) > 2048 {
		t.Fatalf("unbounded or inaccurate search: %s", response.Content)
	}
	if !reflect.DeepEqual(ViewArtifactReads(response), []string{"allowed"}) {
		t.Fatal("search lost authenticated read metadata")
	}
}

func TestArtifactSearchContinuationFindsBoundarySpanningLiteral(t *testing.T) {
	data := []byte(strings.Repeat("a", 1022) + "needle" + strings.Repeat("z", 1500))
	args := map[string]any{"artifact_ref": "allowed", "search_text": "needle", "max_scan_bytes": 1024, "context_bytes": 16}
	first, _, _ := runArtifactSearch(t, t.Context(), data, args)
	if first.IsError || !strings.Contains(first.Content, "unseen content was not searched") || strings.Contains(first.Content, "<match byte_offset=") {
		t.Fatalf("first=%s", first.Content)
	}
	_, tail, ok := strings.Cut(first.Content, "[next search byte_offset=")
	if !ok {
		t.Fatal(first.Content)
	}
	value, _, _ := strings.Cut(tail, "]")
	next, err := strconv.Atoi(value)
	if err != nil {
		t.Fatal(err)
	}
	if next <= 0 || next > 1022 {
		t.Fatalf("continuation dropped overlapping evidence: %d", next)
	}
	args["byte_offset"] = next
	second, _, _ := runArtifactSearch(t, t.Context(), data, args)
	if second.IsError || !strings.Contains(second.Content, "<match byte_offset=1022 ") {
		t.Fatalf("second=%s", second.Content)
	}
}

func TestArtifactSearchRejectsInvalidArgumentsBeforeOpening(t *testing.T) {
	for _, change := range []map[string]any{
		{"file_path": "file", "artifact_ref": ""}, {"offset": 1}, {"limit": 1}, {"byte_limit": 32}, {"byte_offset": -1},
		{"max_matches": 9}, {"context_bytes": 1025}, {"max_scan_bytes": 512}, {"search_text": strings.Repeat("x", 257)},
		{"search_text": "", "max_matches": 1}, {"items": []any{map[string]any{"artifact_ref": "allowed"}}},
	} {
		args := map[string]any{"artifact_ref": "allowed", "search_text": "needle"}
		for key, value := range change {
			args[key] = value
		}
		response, opened, _ := runArtifactSearch(t, t.Context(), []byte("needle"), args)
		if !response.IsError || opened != 0 || len(ViewArtifactReads(response)) != 0 {
			t.Fatalf("invalid args opened artifact: %+v %#v", args, response)
		}
	}
	response, opened, _ := runArtifactSearch(t, WithArtifactMaxReadBytes(t.Context(), 512), []byte("needle"), map[string]any{"artifact_ref": "allowed", "search_text": "needle"})
	if !response.IsError || opened != 0 {
		t.Fatal("ignored output budget")
	}
}

func TestArtifactSearchBatchHonorsAuthorizationAndOutputMembership(t *testing.T) {
	args := map[string]any{"items": []any{map[string]any{"artifact_ref": "allowed", "search_text": "needle"}, map[string]any{"artifact_ref": "denied", "search_text": "needle"}}}
	response, _, closed := runArtifactSearch(t, t.Context(), []byte("needle"), args)
	if response.IsError || !closed || !reflect.DeepEqual(ViewArtifactReads(response), []string{"allowed"}) {
		t.Fatalf("batch=%#v", response)
	}
	for _, data := range [][]byte{[]byte("nothing here"), {0xff, 0xfe}} {
		response, _, closed := runArtifactSearch(t, t.Context(), data, map[string]any{"artifact_ref": "allowed", "search_text": "needle"})
		if !closed {
			t.Fatal("reader not closed")
		}
		if !utf8.Valid(data) && (!response.IsError || len(ViewArtifactReads(response)) != 0) {
			t.Fatal("invalid UTF-8 became evidence")
		}
	}
}

type cancellingSearchReader struct {
	*bytes.Reader
	cancel context.CancelFunc
}

func (r cancellingSearchReader) Read(p []byte) (int, error) { r.cancel(); return r.Reader.Read(p) }
func (r cancellingSearchReader) Close() error               { return nil }

func TestArtifactSearchCancellationNeverIssuesEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response, err := executeView(ctx, fantasy.ToolCall{Input: `{"artifact_ref":"allowed","search_text":"needle"}`}, t.TempDir(), ToolConfig{ArtifactOpener: func(context.Context, string) (io.ReadCloser, error) {
		return cancellingSearchReader{bytes.NewReader([]byte("needle")), cancel}, nil
	}})
	if err != nil || !response.IsError || len(ViewArtifactReads(response)) != 0 {
		t.Fatalf("cancelled=%#v %v", response, err)
	}
}
