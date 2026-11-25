package main

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type deletedFindingLines map[string]map[int][]deletedSourceLine

type deletedSourceLine struct {
	revision string
	text     string
	number   int
}

func citesAssignedDiff(item sourceTask, references []string) bool {
	for _, ref := range item.Binding.Inputs {
		if ref.Kind == "review_diff" && slices.Contains(references, ref.ID) {
			return true
		}
	}
	return false
}

func (d deletedFindingLines) resolve(path string, line int, excerpt, revision string) (match deletedSourceLine, found, ambiguous bool) {
	radius := 0
	needle := strings.TrimSpace(excerpt)
	if excerpt != "" {
		if needle == "" || strings.ContainsAny(needle, "\r\n") {
			return match, false, false
		}
		radius = 2
	}
	bestDistance := radius + 1
	for offset := -radius; offset <= radius; offset++ {
		for _, candidate := range d[path][line+offset] {
			if revision != "" && candidate.revision != revision || needle != "" && strings.TrimSpace(candidate.text) != needle {
				continue
			}
			if found && match.revision != candidate.revision {
				return match, true, true
			}
			distance := findingLineDistance(offset, 0)
			switch {
			case distance < bestDistance:
				match, found, bestDistance, ambiguous = candidate, true, distance, false
			case distance == bestDistance && match != candidate:
				ambiguous = true
			}
		}
	}
	return match, found, ambiguous
}

func validOldRevision(revision string) bool {
	if (len(revision) != 40 && len(revision) != 64) || revision != strings.ToLower(revision) {
		return false
	}
	_, err := hex.DecodeString(revision)
	return err == nil
}

func readDeletedFindingLines(current sourceSession, item sourceTask) (deletedFindingLines, error) {
	lines := make(deletedFindingLines)
	seen := false
	for _, ref := range item.Binding.Inputs {
		if ref.Kind != "review_diff" {
			continue
		}
		if seen {
			return nil, fmt.Errorf("review %s has multiple assigned diffs", item.ID)
		}
		seen = true
		data, err := readAssignedFindingArtifact(current, item, ref)
		if err != nil {
			return nil, err
		}
		lines.index(string(data))
	}
	return lines, nil
}

type deletedDiffLine struct {
	number int
	text   string
}

type findingDiffHunk struct {
	oldLine  int
	oldCount int
	newCount int
	deleted  []deletedDiffLine
}

func (d deletedFindingLines) index(diff string) {
	var path, revision string
	var hunk findingDiffHunk
	awaitingOldPath := false
	for _, line := range strings.Split(diff, "\n") {
		if hunk.oldCount > 0 || hunk.newCount > 0 {
			if hunk.consume(line) {
				if hunk.oldCount == 0 && hunk.newCount == 0 {
					d.add(path, revision, hunk.deleted)
				}
				continue
			}
			hunk = findingDiffHunk{}
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			path, revision = "", ""
			awaitingOldPath = true
		case awaitingOldPath && strings.HasPrefix(line, "hufu-old-revision "):
			revision = strings.TrimPrefix(line, "hufu-old-revision ")
			if !validOldRevision(revision) {
				awaitingOldPath = false
			}
		case awaitingOldPath && strings.HasPrefix(line, "--- "):
			path = findingDiffPath(strings.TrimPrefix(line, "--- "))
			awaitingOldPath = false
		case path != "" && strings.HasPrefix(line, "@@ "):
			hunk = parseFindingDiffHunk(line)
		}
	}
}

func (d deletedFindingLines) add(path, revision string, deleted []deletedDiffLine) {
	if path == "" || len(deleted) == 0 {
		return
	}
	if d[path] == nil {
		d[path] = make(map[int][]deletedSourceLine)
	}
	for _, line := range deleted {
		d[path][line.number] = append(d[path][line.number], deletedSourceLine{revision: revision, text: line.text, number: line.number})
	}
}

func findingDiffPath(header string) string {
	header, _, _ = strings.Cut(header, "\t")
	if strings.HasPrefix(header, `"`) {
		unquoted, err := strconv.Unquote(header)
		if err != nil {
			return ""
		}
		header = unquoted
	}
	if !strings.HasPrefix(header, "a/") {
		return ""
	}
	return strings.TrimPrefix(header, "a/")
}

func parseFindingDiffHunk(line string) findingDiffHunk {
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[3] != "@@" {
		return findingDiffHunk{}
	}
	oldLine, oldCount := findingDiffRange(fields[1], "-")
	_, newCount := findingDiffRange(fields[2], "+")
	if oldCount < 0 || newCount < 0 {
		return findingDiffHunk{}
	}
	return findingDiffHunk{oldLine: oldLine, oldCount: oldCount, newCount: newCount}
}

func findingDiffRange(field, sign string) (int, int) {
	if !strings.HasPrefix(field, sign) {
		return 0, -1
	}
	start, count, hasCount := strings.Cut(field[len(sign):], ",")
	if !hasCount {
		count = "1"
	}
	line, lineErr := strconv.Atoi(start)
	n, countErr := strconv.Atoi(count)
	if lineErr != nil || countErr != nil || line < 0 || n < 0 || (n > 0 && line == 0) || line+n < line {
		return 0, -1
	}
	return line, n
}

func (h *findingDiffHunk) consume(line string) bool {
	if line == `\ No newline at end of file` {
		return true
	}
	if len(line) == 0 {
		return false
	}
	switch line[0] {
	case '-':
		if h.oldCount == 0 {
			return false
		}
		h.deleted = append(h.deleted, deletedDiffLine{number: h.oldLine, text: line[1:]})
		h.oldLine++
		h.oldCount--
	case '+':
		if h.newCount == 0 {
			return false
		}
		h.newCount--
	case ' ':
		if h.oldCount == 0 || h.newCount == 0 {
			return false
		}
		h.oldLine++
		h.oldCount--
		h.newCount--
	default:
		return false
	}
	return true
}
