//go:build linux || darwin
// +build linux darwin

package tools

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildBashResponseTruncationNotice(t *testing.T) {
	manyLines := strings.Repeat("line\n", 2500)
	bigLines := strings.Repeat(strings.Repeat("x", 100)+"\n", 600)
	longLine := strings.Repeat("y", 2500)
	cases := []struct {
		name        string
		stdout      string
		exitCode    int
		wantNotice  bool
		wantOmitted bool
		wantLongCut bool
	}{
		{name: "short output is unchanged", stdout: "hello", wantNotice: false},
		{name: "more than 2000 lines", stdout: manyLines, wantNotice: true, wantOmitted: true},
		{name: "more than 50 KiB", stdout: bigLines, wantNotice: true, wantOmitted: true},
		{name: "one long line only", stdout: longLine, wantNotice: true, wantLongCut: true},
		{name: "many lines and a long line", stdout: longLine + "\n" + manyLines, exitCode: 3, wantNotice: true, wantOmitted: true, wantLongCut: true},
		{name: "long line inside the kept tail", stdout: manyLines + longLine, wantNotice: true, wantOmitted: true, wantLongCut: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assembled := assembleBashOutput(tc.stdout, "", tc.exitCode)
			response := buildBashResponse(tc.stdout, "", tc.exitCode)
			if response.IsError != (tc.exitCode != 0) {
				t.Fatalf("IsError = %v for exit %d", response.IsError, tc.exitCode)
			}
			lines := strings.Split(response.Content, "\n")
			footer := fmt.Sprintf("Exit code: %d", tc.exitCode)
			if lines[len(lines)-1] != footer {
				t.Fatalf("last line = %q, want %q", lines[len(lines)-1], footer)
			}
			if !tc.wantNotice {
				if response.Content != assembled {
					t.Fatalf("untruncated output changed:\n%q\nwant\n%q", response.Content, assembled)
				}
				return
			}
			notice := lines[0]
			if !strings.HasPrefix(notice, "[output truncated by hufu: kept ") || strings.HasPrefix(notice, "Exit code: ") {
				t.Fatalf("first line is not the notice: %q", notice)
			}
			kept := strings.TrimPrefix(response.Content, notice+"\n")
			wantCounts := fmt.Sprintf("kept %d of %d lines (%d of %d bytes)",
				strings.Count(kept, "\n")+1, strings.Count(assembled, "\n")+1, len(kept), len(assembled))
			if !strings.Contains(notice, wantCounts) {
				t.Fatalf("notice %q does not contain %q", notice, wantCounts)
			}
			if got := strings.Contains(notice, "earlier output was omitted"); got != tc.wantOmitted {
				t.Fatalf("omitted clause = %v, want %v: %q", got, tc.wantOmitted, notice)
			}
			if got := strings.Contains(notice, "lines over 2000 characters were cut"); got != tc.wantLongCut {
				t.Fatalf("long-line clause = %v, want %v: %q", got, tc.wantLongCut, notice)
			}
			if bashOutputWouldTruncate(assembled) != true {
				t.Fatal("bashOutputWouldTruncate = false for truncated output")
			}
		})
	}
}

func TestAssembleBashOutputKeepsFooterLast(t *testing.T) {
	cases := []struct {
		name           string
		stdout, stderr string
		exitCode       int
		want           string
	}{
		{name: "no output", exitCode: 0, want: "Exit code: 0"},
		{name: "stdout only", stdout: "ok", exitCode: 0, want: "ok\nExit code: 0"},
		{name: "stderr only", stderr: "boom", exitCode: 2, want: "STDERR:\nboom\nExit code: 2"},
		{name: "both", stdout: "a", stderr: "b", exitCode: 1, want: "a\nSTDERR:\nb\nExit code: 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := assembleBashOutput(tc.stdout, tc.stderr, tc.exitCode); got != tc.want {
				t.Fatalf("assembleBashOutput = %q, want %q", got, tc.want)
			}
			if bashOutputWouldTruncate(tc.want) {
				t.Fatal("short output reported as truncated")
			}
		})
	}
}
