//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestViewArtifactReadCoverageTracksReturnedSourceBytes(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		input   string
		ranges  []ArtifactByteRange
		isError bool
	}{
		{name: "full lines", source: "first\nsecond\n", input: `{"artifact_ref":"source"}`, ranges: []ArtifactByteRange{{Start: 0, End: 13}}},
		{name: "selected line", source: "first\nsecond\nthird\n", input: `{"artifact_ref":"source","offset":1,"limit":1}`, ranges: []ArtifactByteRange{{Start: 6, End: 13}}},
		{name: "skipped lines", source: "first\nsecond\n", input: `{"artifact_ref":"source","offset":20}`},
		{name: "truncated line", source: strings.Repeat("x", maxLineLength+3) + "\n", input: `{"artifact_ref":"source"}`, ranges: []ArtifactByteRange{{Start: 0, End: maxLineLength}}},
		{name: "truncated unicode line", source: strings.Repeat("界", maxLineLength+1) + "\n", input: `{"artifact_ref":"source"}`, ranges: []ArtifactByteRange{{Start: 0, End: 3 * maxLineLength}}},
		{name: "stripped carriage return", source: "a\r\nb", input: `{"artifact_ref":"source"}`, ranges: []ArtifactByteRange{{Start: 0, End: 1}, {Start: 2, End: 4}}},
		{name: "byte range", source: "0123456789", input: `{"artifact_ref":"source","byte_offset":3,"byte_limit":4}`, ranges: []ArtifactByteRange{{Start: 3, End: 7}}},
		{name: "byte end inside rune", source: "A界B", input: `{"artifact_ref":"source","byte_limit":3}`, ranges: []ArtifactByteRange{{Start: 0, End: 1}}},
		{name: "byte end at rune boundary", source: "A界B", input: `{"artifact_ref":"source","byte_limit":4}`, ranges: []ArtifactByteRange{{Start: 0, End: 4}}},
		{name: "byte start inside rune", source: "A界B", input: `{"artifact_ref":"source","byte_offset":2,"byte_limit":3}`, isError: true},
		{name: "byte end of nonempty source", source: "abc", input: `{"artifact_ref":"source","byte_offset":3,"byte_limit":4}`},
		{name: "invalid UTF8 range", source: "a\xffb", input: `{"artifact_ref":"source","byte_limit":3}`, isError: true},
		{name: "search only excerpts", source: "prefix needle suffix", input: `{"artifact_ref":"source","search_text":"needle","context_bytes":1}`, ranges: []ArtifactByteRange{{Start: 6, End: 14}}},
		{name: "search offset", source: "skip needle tail", input: `{"artifact_ref":"source","search_text":"needle","byte_offset":5,"context_bytes":1}`, ranges: []ArtifactByteRange{{Start: 5, End: 12}}},
		{name: "search overlapping excerpts", source: "prefix needle needle tail", input: `{"artifact_ref":"source","search_text":"needle","max_matches":2,"context_bytes":4}`, ranges: []ArtifactByteRange{{Start: 3, End: 24}}},
		{name: "search UTF8 boundaries", source: "前needle後", input: `{"artifact_ref":"source","search_text":"needle","context_bytes":1}`, ranges: []ArtifactByteRange{{Start: 3, End: 9}}},
		{name: "search scan not returned", source: "needle" + strings.Repeat("z", 2048), input: `{"artifact_ref":"source","search_text":"needle","context_bytes":1,"max_scan_bytes":1024}`, ranges: []ArtifactByteRange{{Start: 0, End: 7}}},
		{name: "search zero hits", source: strings.Repeat("z", 2048), input: `{"artifact_ref":"source","search_text":"needle","max_scan_bytes":1024}`},
		{name: "empty line read", input: `{"artifact_ref":"source"}`},
		{name: "empty byte read", input: `{"artifact_ref":"source","byte_limit":4}`},
		{name: "empty search", input: `{"artifact_ref":"source","search_text":"needle"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := executeView(t.Context(), fantasy.ToolCall{Input: tc.input}, t.TempDir(), ToolConfig{
				ArtifactOpener: seekableOpener([]byte(tc.source)),
			})
			if err != nil || response.IsError != tc.isError {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			got := ViewArtifactReadCoverage(response)
			if tc.isError {
				if len(got) != 0 {
					t.Fatalf("failed read has coverage: %+v", got)
				}
				return
			}
			digest := sha256.Sum256([]byte(tc.source))
			want := []ArtifactReadCoverage{{
				ArtifactID: "source", SHA256: hex.EncodeToString(digest[:]), ByteSize: int64(len(tc.source)), Ranges: append([]ArtifactByteRange{}, tc.ranges...),
			}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("coverage=%+v want %+v", got, want)
			}
		})
	}
}

func TestViewArtifactReadCoverageBindsResponseAndSource(t *testing.T) {
	var previousDigest string
	for _, source := range []string{"first source", "changed source"} {
		response, err := executeView(t.Context(), fantasy.ToolCall{Input: `{"artifact_ref":"source","byte_limit":5}`}, t.TempDir(), ToolConfig{
			ArtifactOpener: seekableOpener([]byte(source)),
		})
		if err != nil || response.IsError {
			t.Fatalf("response=%#v err=%v", response, err)
		}
		got := ViewArtifactReadCoverage(response)
		if len(got) != 1 || got[0].SHA256 == previousDigest {
			t.Fatalf("changed source reused identity: %+v", got)
		}
		previousDigest = got[0].SHA256
		for _, mutation := range []string{"truncate", "append", "failed"} {
			t.Run(source+"/"+mutation, func(t *testing.T) {
				changed := response
				switch mutation {
				case "truncate":
					changed.Content = changed.Content[:len(changed.Content)-1]
				case "append":
					changed.Content += "extra"
				case "failed":
					changed.IsError = true
				}
				if coverage := ViewArtifactReadCoverage(changed); len(coverage) != 0 {
					t.Fatalf("changed response retained coverage: %+v", coverage)
				}
			})
		}
	}
}

func TestViewArtifactReadCoverageBatchOmitsFailedAndOverflowedItems(t *testing.T) {
	for _, name := range []string{"failed", "overflowed"} {
		t.Run(name, func(t *testing.T) {
			source := "source\n"
			input := `{"items":[{"artifact_ref":"first"},{"artifact_ref":"second"}]}`
			if name == "overflowed" {
				source = strings.Repeat(strings.Repeat("x", 499)+"\n", 1000)
				input = `{"items":[{"artifact_ref":"first","limit":1000},{"artifact_ref":"second","limit":1000}]}`
			}
			response, err := executeView(t.Context(), fantasy.ToolCall{Input: input}, t.TempDir(), ToolConfig{
				ArtifactOpener: func(_ context.Context, ref string) (io.ReadCloser, error) {
					if name == "failed" && ref == "second" {
						return nil, fmt.Errorf("not authorized")
					}
					return seekableArtifact{bytes.NewReader([]byte(source))}, nil
				},
			})
			if err != nil || response.IsError {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			got := ViewArtifactReadCoverage(response)
			if len(got) != 1 || got[0].ArtifactID != "first" || !reflect.DeepEqual(got[0].Ranges, []ArtifactByteRange{{Start: 0, End: int64(len(source))}}) {
				t.Fatalf("failed or omitted item has coverage: %+v", got)
			}
			response.Content = response.Content[:len(response.Content)-1]
			if got := ViewArtifactReadCoverage(response); len(got) != 0 {
				t.Fatalf("trimmed batch retained coverage: %+v", got)
			}
		})
	}
}

type identityCoverageArtifact struct {
	*bytes.Reader
	digest    string
	byteSize  int64
	readBytes int
}

func (r *identityCoverageArtifact) Close() error { return nil }

func (r *identityCoverageArtifact) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.readBytes += n
	return n, err
}

func (r *identityCoverageArtifact) ArtifactReadIdentity() (string, int64) {
	return r.digest, r.byteSize
}

func TestViewArtifactReadCoverageUsesVerifiedReaderIdentity(t *testing.T) {
	source := []byte(strings.Repeat("source", 1000))
	digest := sha256.Sum256(source)
	for _, name := range []string{"verified", "invalid digest", "wrong size"} {
		t.Run(name, func(t *testing.T) {
			reader := &identityCoverageArtifact{
				Reader: bytes.NewReader(source), digest: hex.EncodeToString(digest[:]), byteSize: int64(len(source)),
			}
			if name == "invalid digest" {
				reader.digest = "not a sha256"
			}
			if name == "wrong size" {
				reader.byteSize++
			}
			response, err := executeView(t.Context(), fantasy.ToolCall{Input: `{"artifact_ref":"source","byte_limit":4}`}, t.TempDir(), ToolConfig{
				ArtifactOpener: func(context.Context, string) (io.ReadCloser, error) { return reader, nil },
			})
			if err != nil || response.IsError != (name != "verified") {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			got := ViewArtifactReadCoverage(response)
			if name != "verified" {
				if len(got) != 0 {
					t.Fatalf("invalid identity produced coverage: %+v", got)
				}
				return
			}
			if reader.readBytes != 4 || len(got) != 1 || got[0].SHA256 != hex.EncodeToString(digest[:]) {
				t.Fatalf("read bytes=%d coverage=%+v", reader.readBytes, got)
			}
		})
	}
}

type failingCoverageArtifact struct {
	io.ReadSeeker
	failAt int64
}

func (r failingCoverageArtifact) Close() error { return nil }

func (r failingCoverageArtifact) Read(p []byte) (int, error) {
	offset, err := r.ReadSeeker.Seek(0, io.SeekCurrent)
	if err != nil || offset >= r.failAt {
		return 0, fmt.Errorf("source read failed")
	}
	return r.ReadSeeker.Read(p)
}

func TestViewArtifactReadCoverageExcludesReadAndIdentityFailures(t *testing.T) {
	for _, name := range []string{"source read", "identity read"} {
		t.Run(name, func(t *testing.T) {
			failAt := int64(0)
			if name == "identity read" {
				failAt = 4
			}
			response, err := executeView(t.Context(), fantasy.ToolCall{Input: `{"artifact_ref":"source","byte_limit":4}`}, t.TempDir(), ToolConfig{
				ArtifactOpener: func(context.Context, string) (io.ReadCloser, error) {
					return failingCoverageArtifact{ReadSeeker: bytes.NewReader([]byte("source bytes")), failAt: failAt}, nil
				},
			})
			if err != nil || !response.IsError || len(ViewArtifactReadCoverage(response)) != 0 || len(ViewArtifactReads(response)) != 0 {
				t.Fatalf("failed read response=%#v err=%v", response, err)
			}
		})
	}
}
