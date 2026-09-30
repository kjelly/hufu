//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestParseGrepOutputLine(t *testing.T) {
	cases := []struct {
		name      string
		line      string
		ok        bool
		separator bool
		match     bool
		path      string
		rendered  string
	}{
		{name: "match with path", line: "a.go\x0012:func x()", ok: true, match: true, path: "a.go", rendered: "a.go:12:func x()"},
		{name: "context with path", line: "a.go\x0013-}", ok: true, path: "a.go", rendered: "a.go-13-}"},
		{name: "path with colon and dash", line: "dir/f-1:x.txt\x002:needle", ok: true, match: true, path: "dir/f-1:x.txt", rendered: "dir/f-1:x.txt:2:needle"},
		{name: "match without path", line: "7:needle: here", ok: true, match: true, rendered: "7:needle: here"},
		{name: "context without path", line: "8-plain", ok: true, rendered: "8-plain"},
		{name: "separator", line: "--", ok: true, separator: true},
		{name: "binary notice", line: "Binary file x.bin matches", ok: false},
		{name: "empty text", line: "a.go\x003:", ok: true, match: true, path: "a.go", rendered: "a.go:3:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseGrepOutputLine(tc.line)
			if ok != tc.ok || got.separator != tc.separator || got.match != tc.match || got.path != tc.path {
				t.Fatalf("parseGrepOutputLine(%q) = %+v, %v", tc.line, got, ok)
			}
			if tc.rendered != "" && got.render() != tc.rendered {
				t.Fatalf("render() = %q, want %q", got.render(), tc.rendered)
			}
		})
	}
}

func TestLimitGrepMatches(t *testing.T) {
	// Three matches in a.go with one line of context each, and one in b.go.
	output := strings.Join([]string{
		"a.go\x001-ctx", "a.go\x002:m1", "a.go\x003-ctx",
		"--",
		"a.go\x009-ctx", "a.go\x0010:m2", "a.go\x0011-ctx",
		"--",
		"a.go\x0020-ctx", "a.go\x0021:m3", "a.go\x0022-ctx",
		"--",
		"b.go\x004:m4",
	}, "\n") + "\n"
	cases := []struct {
		name       string
		limit      int
		perFileCap int
		want       []string
		shown      int
		total      int
		capped     bool
	}{
		{name: "context does not count", limit: 2, want: []string{
			"a.go-1-ctx", "a.go:2:m1", "a.go-3-ctx", "--", "a.go-9-ctx", "a.go:10:m2", "a.go-11-ctx",
		}, shown: 2, total: 4},
		{name: "everything fits", limit: 10, want: []string{
			"a.go-1-ctx", "a.go:2:m1", "a.go-3-ctx", "--", "a.go-9-ctx", "a.go:10:m2", "a.go-11-ctx",
			"--", "a.go-20-ctx", "a.go:21:m3", "a.go-22-ctx", "--", "b.go:4:m4",
		}, shown: 4, total: 4},
		{name: "per-file cap reached", limit: 2, perFileCap: 3, want: []string{
			"a.go-1-ctx", "a.go:2:m1", "a.go-3-ctx", "--", "a.go-9-ctx", "a.go:10:m2", "a.go-11-ctx",
		}, shown: 2, total: 4, capped: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := limitGrepMatches(output, tc.limit, tc.perFileCap)
			if got.content != strings.Join(tc.want, "\n") {
				t.Fatalf("content:\n%s\nwant:\n%s", got.content, strings.Join(tc.want, "\n"))
			}
			if got.shown != tc.shown || got.total != tc.total || got.capped != tc.capped {
				t.Fatalf("shown=%d total=%d capped=%v, want %d %d %v", got.shown, got.total, got.capped, tc.shown, tc.total, tc.capped)
			}
		})
	}
}

func TestGrepTruncationNoticeText(t *testing.T) {
	cases := []struct {
		name     string
		limited  grepLimitedOutput
		bytesCut bool
		want     string
	}{
		{name: "complete", limited: grepLimitedOutput{shown: 3, total: 3}, want: ""},
		{name: "matches omitted", limited: grepLimitedOutput{shown: 2, total: 5}, want: "showing 2 of 5 matching lines"},
		{name: "per-file cap", limited: grepLimitedOutput{shown: 2, total: 3, capped: true}, want: "showing 2 of 3+ matching lines"},
		{name: "bytes cut", limited: grepLimitedOutput{shown: 3, total: 3}, bytesCut: true, want: "output cut at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grepTruncationNotice(tc.limited, tc.bytesCut)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("notice = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) || !strings.Contains(got, "Raise limit") {
				t.Fatalf("notice = %q, want it to contain %q and a next step", got, tc.want)
			}
		})
	}
}

func writeGrepFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"main.go":      "package main\n\nfunc MatchedSkillNames() {}\n",
		"main_test.go": "package main\n\nfunc TestMatchedSkillNames() { MatchedSkillNames() }\n",
		"notes.md":     "MatchedSkillNames in prose\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestGrepAppliesIncludeAndGlobTogether(t *testing.T) {
	root := writeGrepFixture(t)
	cases := []struct {
		name    string
		input   string
		want    []string
		wantNot []string
	}{
		{name: "include with negated glob", input: `{"pattern":"MatchedSkillNames","include":"*.go","glob":"!*_test.go"}`,
			want: []string{"main.go"}, wantNot: []string{"main_test.go", "notes.md"}},
		{name: "negated include alone", input: `{"pattern":"MatchedSkillNames","include":"!*_test.go"}`,
			want: []string{"main.go", "notes.md"}, wantNot: []string{"main_test.go"}},
		{name: "glob alone", input: `{"pattern":"MatchedSkillNames","glob":"*.md"}`,
			want: []string{"notes.md"}, wantNot: []string{"main.go"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, err := executeGrep(context.Background(), fantasy.ToolCall{Input: tc.input}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
			if err != nil || response.IsError {
				t.Fatalf("executeGrep: response=%#v err=%v", response, err)
			}
			assertGrepFiles(t, response.Content, tc.want, tc.wantNot)
		})
	}
}

func TestGrepFallbackAppliesIncludeAndGlobTogether(t *testing.T) {
	root := writeGrepFixture(t)
	args := grepArgs{Pattern: "MatchedSkillNames", Include: "*.go", Glob: "!*_test.go"}
	response, err := grepFallbackCandidates(context.Background(), args, root, grepGlobPatterns(args), defaultGrepLimit, "workspace", false, nil)
	if err != nil || response.IsError {
		t.Fatalf("grepFallbackCandidates: response=%#v err=%v", response, err)
	}
	assertGrepFiles(t, response.Content, []string{"main.go"}, []string{"main_test.go", "notes.md"})
}

func assertGrepFiles(t *testing.T, content string, want, wantNot []string) {
	t.Helper()
	files := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		if name, _, ok := strings.Cut(line, ":"); ok {
			files[filepath.Base(name)] = true
		}
	}
	for _, name := range want {
		if !files[name] {
			t.Errorf("output lacks %s:\n%s", name, content)
		}
	}
	for _, name := range wantNot {
		if files[name] {
			t.Errorf("output includes %s:\n%s", name, content)
		}
	}
}

func TestGrepLimitCountsMatchesNotContextLines(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		fmt.Fprintf(&b, "filler %d\nneedle %d\n", i, i)
	}
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	input := `{"pattern":"needle","context":3,"limit":4}`
	for name, run := range map[string]func() (fantasy.ToolResponse, error){
		"ripgrep": func() (fantasy.ToolResponse, error) {
			return executeGrep(context.Background(), fantasy.ToolCall{Input: input}, root, ToolConfig{WorkDir: root, AllowedPaths: []string{root}})
		},
		"gnu grep": func() (fantasy.ToolResponse, error) {
			args := grepArgs{Pattern: "needle", Context: 3, Limit: 4}
			return grepFallbackCandidates(context.Background(), args, root, nil, 4, "workspace", false, nil)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response, err := run()
			if err != nil || response.IsError {
				t.Fatalf("grep: response=%#v err=%v", response, err)
			}
			matches := 0
			for _, line := range strings.Split(response.Content, "\n") {
				if strings.Contains(line, ":needle ") {
					matches++
				}
			}
			if matches != 4 {
				t.Fatalf("got %d matching lines, want 4:\n%s", matches, response.Content)
			}
			// The per-file cap stops each backend one match past the limit;
			// ripgrep also marks matches inside that match's trailing context,
			// so the lower bound it reports can be higher.
			if !strings.Contains(response.Content, "[output truncated: showing 4 of ") || !strings.Contains(response.Content, "+ matching lines") {
				t.Fatalf("notice does not report the omitted matches:\n%s", response.Content)
			}
		})
	}
}
