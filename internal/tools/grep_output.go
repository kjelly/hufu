//go:build linux || darwin
// +build linux darwin

package tools

import (
	"fmt"
	"strings"
)

// grepOutputLine is one line of ripgrep or GNU grep output produced with
// --line-number and --null. A path, when printed, ends at a NUL byte. The line
// number that follows ends at ':' on a matching line and at '-' on a context
// line. Without the NUL a path containing ':' or '-' made the two kinds
// indistinguishable.
type grepOutputLine struct {
	path      string
	hasPath   bool
	number    string
	match     bool
	text      string
	separator bool
	raw       string
}

func parseGrepOutputLine(line string) (grepOutputLine, bool) {
	if line == "--" {
		return grepOutputLine{separator: true, raw: line}, true
	}
	parsed := grepOutputLine{raw: line}
	rest := line
	if i := strings.IndexByte(line, 0); i >= 0 {
		parsed.path, parsed.hasPath, rest = line[:i], true, line[i+1:]
	}
	digits := 0
	for digits < len(rest) && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits == len(rest) || (rest[digits] != ':' && rest[digits] != '-') {
		return grepOutputLine{raw: line}, false
	}
	parsed.number = rest[:digits]
	parsed.match = rest[digits] == ':'
	parsed.text = rest[digits+1:]
	return parsed, true
}

// render restores the backend's default text form: path, line number, and
// text joined by ':' on a matching line and by '-' on a context line.
func (l grepOutputLine) render() string {
	sep := "-"
	if l.match {
		sep = ":"
	}
	if l.hasPath {
		return l.path + sep + l.number + sep + l.text
	}
	return l.number + sep + l.text
}

// grepLimitedOutput is grep output cut to a number of matching lines.
type grepLimitedOutput struct {
	content string
	shown   int
	total   int
	// capped reports that some file reached the backend's per-file match
	// cap, so total undercounts the matches that exist.
	capped bool
}

// limitGrepMatches keeps the first limit matching lines with their context.
// Context lines do not count toward the limit. After the last kept match, its
// trailing context is kept up to the next group separator or match. perFileCap
// is the per-file match cap passed to the backend.
func limitGrepMatches(output string, limit, perFileCap int) grepLimitedOutput {
	var kept []string
	result := grepLimitedOutput{}
	perFile := make(map[string]int)
	full := false
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		parsed, ok := parseGrepOutputLine(line)
		if ok && parsed.match {
			result.total++
			perFile[parsed.path]++
			if perFileCap > 0 && perFile[parsed.path] >= perFileCap {
				result.capped = true
			}
		}
		if full {
			continue
		}
		switch {
		case !ok:
			// Backend notices such as "Binary file x matches" pass through.
			if result.shown < limit {
				kept = append(kept, line)
			}
		case parsed.separator:
			if result.shown >= limit {
				full = true
				continue
			}
			kept = append(kept, line)
		case parsed.match:
			if result.shown >= limit {
				full = true
				continue
			}
			result.shown++
			kept = append(kept, parsed.render())
		default:
			kept = append(kept, parsed.render())
		}
	}
	result.content = strings.Join(kept, "\n")
	return result
}

// grepTruncationNotice tells the model what was left out and how to get it,
// in match counts rather than output lines.
func grepTruncationNotice(limited grepLimitedOutput, bytesCut bool) string {
	var notes []string
	if limited.total > limited.shown || limited.capped {
		total := fmt.Sprintf("%d", limited.total)
		if limited.capped {
			total += "+"
		}
		notes = append(notes, fmt.Sprintf("showing %d of %s matching lines", limited.shown, total))
	}
	if bytesCut {
		notes = append(notes, fmt.Sprintf("output cut at %d bytes", defaultMaxBytes))
	}
	if len(notes) == 0 {
		return ""
	}
	return "\n[output truncated: " + strings.Join(notes, "; ") + ". Raise limit, lower context, or narrow path/include/glob to see the rest]"
}

// grepGlobPatterns returns every file glob the call supplied. include and glob
// used to be alternatives, and glob was silently dropped when both were set:
// {"include":"*.go","glob":"!*_test.go"} searched test files too.
func grepGlobPatterns(args grepArgs) []string {
	var globs []string
	for _, glob := range []string{args.Include, args.Glob} {
		glob = strings.TrimSpace(glob)
		if glob == "" {
			continue
		}
		duplicate := false
		for _, existing := range globs {
			if existing == glob {
				duplicate = true
				break
			}
		}
		if !duplicate {
			globs = append(globs, glob)
		}
	}
	return globs
}

// gnuGrepGlobArgs converts ripgrep-style globs, where a leading '!' excludes,
// into GNU grep --include and --exclude options.
func gnuGrepGlobArgs(globs []string) []string {
	args := make([]string, 0, len(globs))
	for _, glob := range globs {
		if excluded, ok := strings.CutPrefix(glob, "!"); ok {
			if excluded != "" {
				args = append(args, "--exclude="+excluded)
			}
			continue
		}
		args = append(args, "--include="+glob)
	}
	return args
}

// finishGrepOutput applies the match limit, the line-length cap, and the byte
// cap to raw backend output.
func finishGrepOutput(output string, limit int) string {
	limited := limitGrepMatches(output, limit, limit+1)
	lines := strings.Split(limited.content, "\n")
	for i, line := range lines {
		lines[i] = truncateLine(line, grepMaxLineLen)
	}
	content := strings.Join(lines, "\n")
	if content != "" && strings.HasSuffix(output, "\n") {
		// Keep the backend's trailing newline, as the unfiltered output did.
		content += "\n"
	}
	bytesCut := false
	if len(content) > defaultMaxBytes {
		content = safeTruncateHeadBytes(content, defaultMaxBytes)
		bytesCut = true
	}
	return content + grepTruncationNotice(limited, bytesCut)
}
