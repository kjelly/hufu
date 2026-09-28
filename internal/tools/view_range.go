//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

// executeViewArtifactRange reads one bounded byte range of an authorized
// artifact. It seeks instead of loading the artifact to reach the offset,
// and returns raw text without line numbers so ranges concatenate exactly.
func executeViewArtifactRange(ctx context.Context, args viewItemArgs, cfg ToolConfig) (fantasy.ToolResponse, error) {
	if cfg.ArtifactOpener == nil {
		return fantasy.NewTextErrorResponse("invalid artifact_ref: opaque artifact access is unavailable"), nil
	}
	maxRead := ArtifactMaxReadBytes(ctx)
	offset := 0
	if args.ByteOffset != nil {
		offset = *args.ByteOffset
	}
	limit := maxRead
	if args.ByteLimit != nil {
		limit = *args.ByteLimit
	}
	switch {
	case offset < 0:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("byte_offset must be non-negative, got %d", offset)), nil
	case limit <= 0:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("byte_limit must be positive, got %d", limit)), nil
	case limit > maxRead:
		return fantasy.NewTextErrorResponse(fmt.Sprintf("byte_limit %d exceeds the maximum of %d bytes per read", limit, maxRead)), nil
	}

	reader, err := cfg.ArtifactOpener(ctx, args.ArtifactRef)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid artifact_ref: %v", err)), nil
	}
	defer func() { _ = reader.Close() }()
	seeker, ok := reader.(io.ReadSeeker)
	if !ok {
		return fantasy.NewTextErrorResponse("artifact does not support byte-range reads"), nil
	}
	chunk, size, errText := readArtifactRange(seeker, int64(offset), int64(limit))
	if errText != "" {
		return fantasy.NewTextErrorResponse(errText), nil
	}

	end := int64(offset) + int64(len(chunk))
	var b strings.Builder
	fmt.Fprintf(&b, "<artifact:%s bytes=[%d,%d) total=%d>\n", args.ArtifactRef, offset, end, size)
	b.Write(chunk)
	fmt.Fprintf(&b, "\n</artifact:%s>\n", args.ArtifactRef)
	if end < size {
		fmt.Fprintf(&b, "[next byte_offset=%d]", end)
	} else {
		b.WriteString("[end of artifact]")
	}
	return fantasy.NewTextResponse(b.String()), nil
}

// readArtifactRange returns at most limit bytes starting at offset, cut back
// to a UTF-8 boundary. A non-empty errText is a recoverable, path-free error.
func readArtifactRange(seeker io.ReadSeeker, offset, limit int64) ([]byte, int64, string) {
	size, err := seeker.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, 0, "failed to read artifact size"
	}
	if offset > size {
		return nil, size, fmt.Sprintf("byte_offset %d is beyond the end of the artifact (%d bytes)", offset, size)
	}
	if offset == size {
		return nil, size, ""
	}
	// Read up to UTFMax-1 bytes before the offset so a start inside a rune
	// can name the boundary to use instead.
	windowStart := max(0, offset-(utf8.UTFMax-1))
	if _, err := seeker.Seek(windowStart, io.SeekStart); err != nil {
		return nil, size, "failed to seek in artifact"
	}
	lead := int(offset - windowStart)
	buf := make([]byte, int64(lead)+min(limit, size-offset))
	if _, err := io.ReadFull(seeker, buf); err != nil {
		return nil, size, "failed to read artifact"
	}
	if !utf8.RuneStart(buf[lead]) {
		boundary := windowStart
		for i := lead - 1; i >= 0; i-- {
			if utf8.RuneStart(buf[i]) {
				boundary = windowStart + int64(i)
				break
			}
		}
		return nil, size, fmt.Sprintf("byte_offset %d is inside a UTF-8 character; use byte_offset %d", offset, boundary)
	}
	chunk := buf[lead:]
	if offset+int64(len(chunk)) < size {
		chunk = trimIncompleteRune(chunk)
	}
	if len(chunk) == 0 || !utf8.Valid(chunk) {
		return nil, size, "artifact is not UTF-8 text in the requested range"
	}
	return chunk, size, ""
}

// trimIncompleteRune drops a rune split by the end of a chunk.
func trimIncompleteRune(chunk []byte) []byte {
	for i := len(chunk) - 1; i >= 0 && i >= len(chunk)-utf8.UTFMax; i-- {
		if utf8.RuneStart(chunk[i]) {
			if !utf8.FullRune(chunk[i:]) {
				return chunk[:i]
			}
			return chunk
		}
	}
	return chunk
}
