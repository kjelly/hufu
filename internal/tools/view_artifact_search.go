//go:build linux || darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
)

const maxArtifactSearchScanBytes = 4 * 1024 * 1024

func validateArtifactSearch(args viewItemArgs) error {
	if len(args.SearchText) == 0 || len(args.SearchText) > 256 || !utf8.ValidString(args.SearchText) {
		return fmt.Errorf("search_text must contain 1 to 256 bytes of UTF-8 literal text")
	}
	if args.MaxMatches < 0 || args.MaxMatches > 8 || args.ContextBytes < 0 || args.ContextBytes > 1024 {
		return fmt.Errorf("search requires max_matches 1..8 and context_bytes 1..1024, or omitted defaults")
	}
	if args.MaxScanBytes != 0 && (args.MaxScanBytes < 1024 || args.MaxScanBytes > maxArtifactSearchScanBytes) {
		return fmt.Errorf("max_scan_bytes must be between 1024 and %d", maxArtifactSearchScanBytes)
	}
	if args.ByteOffset != nil && *args.ByteOffset < 0 {
		return fmt.Errorf("byte_offset must be non-negative")
	}
	return nil
}

// Search scans only an authorized immutable artifact. It emits bounded literal
// excerpts and exact navigation offsets, without interpreting task language or
// treating a scan's unseen content as observed evidence.
func executeViewArtifactSearch(ctx context.Context, args viewItemArgs, cfg ToolConfig) (fantasy.ToolResponse, error) {
	reject := func(message string) (fantasy.ToolResponse, error) { return fantasy.NewTextErrorResponse(message), nil }
	if cfg.ArtifactOpener == nil {
		return reject("invalid artifact_ref: opaque artifact access is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return reject("cancelled artifact search")
	}
	if err := validateArtifactSearch(args); err != nil {
		return reject(err.Error())
	}
	matches, around, scanLimit := args.MaxMatches, args.ContextBytes, args.MaxScanBytes
	if matches == 0 {
		matches = 1
	}
	if around == 0 {
		around = 256
	}
	if scanLimit == 0 {
		scanLimit = maxArtifactSearchScanBytes
	}
	outputBudget := min(8192, ArtifactMaxReadBytes(ctx))
	if matches*(2*around+len(args.SearchText)+160)+512+2*len(args.ArtifactRef) > outputBudget {
		return reject("search excerpts exceed the output budget; reduce max_matches or context_bytes")
	}
	reader, err := cfg.ArtifactOpener(ctx, args.ArtifactRef)
	if err != nil {
		return reject(fmt.Sprintf("invalid artifact_ref: %v", err))
	}
	defer func() { _ = reader.Close() }()
	seeker, ok := reader.(io.ReadSeeker)
	if !ok {
		return reject("artifact does not support search with byte offsets")
	}
	offset := 0
	if args.ByteOffset != nil {
		offset = *args.ByteOffset
	}
	data, size, errText := readArtifactRange(seeker, int64(offset), int64(scanLimit))
	if errText != "" {
		return reject(errText)
	}
	if err := ctx.Err(); err != nil {
		return reject("cancelled artifact search")
	}
	var out strings.Builder
	fmt.Fprintf(&out, "<artifact:%s search bytes=[%d,%d) total=%d>\n", args.ArtifactRef, offset, int64(offset)+int64(len(data)), size)
	out.WriteString("[literal search: only the shown match excerpts were returned; other scanned content is not observed source evidence]\n")
	needle := []byte(args.SearchText)
	position, count := 0, 0
	for count < matches && position < len(data) {
		relative := bytes.Index(data[position:], needle)
		if relative < 0 {
			break
		}
		found := position + relative
		start, end := max(0, found-around), min(len(data), found+len(needle)+around)
		for start < found && !utf8.RuneStart(data[start]) {
			start++
		}
		snippet := trimIncompleteRune(data[start:end])
		fmt.Fprintf(&out, "<match byte_offset=%d bytes=[%d,%d)>\n", offset+found, offset+start, offset+start+len(snippet))
		out.Write(snippet)
		out.WriteString("\n</match>\n")
		count++
		position = found + len(needle)
	}
	fmt.Fprintf(&out, "</artifact:%s>\n", args.ArtifactRef)
	if count == 0 {
		out.WriteString("[no matches in the scanned range]\n")
	}
	next := len(data)
	if count == matches {
		next = position
		out.WriteString("[match limit reached]\n")
	} else if int64(offset)+int64(len(data)) < size {
		// Retain enough overlap to find a literal crossing the next scan boundary.
		next = max(0, len(data)-len(needle)+1)
		for next > 0 && next < len(data) && !utf8.RuneStart(data[next]) {
			next--
		}
		if next == 0 {
			return reject("scan window cannot advance; increase max_scan_bytes")
		}
		out.WriteString("[scan limit reached; unseen content was not searched]\n")
	}
	if int64(offset)+int64(next) < size {
		fmt.Fprintf(&out, "[next search byte_offset=%d]", offset+next)
	} else {
		out.WriteString("[end of artifact]")
	}
	if out.Len() > outputBudget {
		return reject("search output budget exceeded; reduce max_matches or context_bytes")
	}
	return artifactViewResponse(out.String(), args.ArtifactRef), nil
}
