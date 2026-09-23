package versionstore

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"strings"
)

// HufuignoreFile is the non-Git exclusion file at the subject root.
const HufuignoreFile = ".hufuignore"

// ignoreRule is one parsed .hufuignore line.
type ignoreRule struct {
	pattern  string
	anchored bool // matched against the leading path segments
	dirOnly  bool // matches directories only
	segments int  // number of path segments in an anchored pattern
}

// hufuIgnore is a minimal, dependency-free subset of gitignore:
//
//   - one pattern per line; blank lines and lines starting with '#' are ignored
//   - a leading '/' anchors the pattern at the subject root; a pattern that
//     contains '/' anywhere except at its end is anchored as well (gitignore
//     semantics), otherwise it matches a basename at any depth
//   - a trailing '/' matches directories only
//   - globs use path.Match; '!' negation and '**' are rejected
type hufuIgnore struct {
	rules []ignoreRule
}

func parseHufuignore(data []byte) (*hufuIgnore, error) {
	matcher := &hufuIgnore{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rule, err := parseIgnoreRule(line)
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", HufuignoreFile, lineNumber, err)
		}
		matcher.rules = append(matcher.rules, rule)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", HufuignoreFile, err)
	}
	return matcher, nil
}

func parseIgnoreRule(line string) (ignoreRule, error) {
	if strings.HasPrefix(line, "!") {
		return ignoreRule{}, fmt.Errorf("negation %q is not supported", line)
	}
	if strings.Contains(line, "**") {
		return ignoreRule{}, fmt.Errorf("'**' in %q is not supported", line)
	}
	rule := ignoreRule{pattern: line}
	if strings.HasSuffix(rule.pattern, "/") {
		rule.dirOnly = true
		rule.pattern = strings.TrimSuffix(rule.pattern, "/")
	}
	if strings.HasPrefix(rule.pattern, "/") {
		rule.anchored = true
		rule.pattern = strings.TrimPrefix(rule.pattern, "/")
	} else if strings.Contains(rule.pattern, "/") {
		rule.anchored = true
	}
	if rule.pattern == "" || strings.HasPrefix(rule.pattern, "/") || strings.HasSuffix(rule.pattern, "/") {
		return ignoreRule{}, fmt.Errorf("empty or malformed pattern %q", line)
	}
	if _, err := path.Match(rule.pattern, ""); err != nil {
		return ignoreRule{}, fmt.Errorf("invalid pattern %q: %w", line, err)
	}
	rule.segments = strings.Count(rule.pattern, "/") + 1
	return rule, nil
}

// matches reports whether rel (forward-slash, root-relative) is excluded.
// isDir tells whether rel itself is a directory.
func (m *hufuIgnore) matches(rel string, isDir bool) bool {
	if m == nil || len(m.rules) == 0 {
		return false
	}
	segments := strings.Split(rel, "/")
	for _, rule := range m.rules {
		if rule.anchored {
			if rule.segments > len(segments) {
				continue
			}
			prefix := strings.Join(segments[:rule.segments], "/")
			if ok, _ := path.Match(rule.pattern, prefix); !ok {
				continue
			}
			// The matched prefix is a directory whenever rel continues below it.
			if !rule.dirOnly || rule.segments < len(segments) || isDir {
				return true
			}
			continue
		}
		for index, segment := range segments {
			if ok, _ := path.Match(rule.pattern, segment); !ok {
				continue
			}
			if !rule.dirOnly || index < len(segments)-1 || isDir {
				return true
			}
		}
	}
	return false
}
