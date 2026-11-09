package main

import (
	"errors"
	"io"
	"slices"
	"testing"
)

func TestReadTaskPrompt(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  string
		err   error
	}{
		{name: "single line", lines: []string{"inspect runtime"}, want: "inspect runtime"},
		{name: "continuation", lines: []string{"first\\", "  second\\", "third"}, want: "first\n  second\nthird"},
		{name: "literal slashes", lines: []string{`path\\`}, want: `path\\`},
		{name: "block", lines: []string{"/multiline", "first", "", "  第二行", "/end"}, want: "first\n\n  第二行"},
		{name: "literal terminator", lines: []string{"/multiline", `\/end`, " /end", "/end"}, want: "/end\n /end"},
		{name: "empty block", lines: []string{"/multiline", "/end"}},
		{name: "block preserves slashes", lines: []string{"/multiline", "first\\", "/end"}, want: "first\\"},
		{name: "cancel block", lines: []string{"/multiline", "partial"}, err: errInterrupted{}},
		{name: "eof continuation", lines: []string{"partial\\"}, err: io.EOF},
		{name: "eof initial", err: io.EOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := slices.Clone(tt.lines)
			got, err := readTaskPrompt(func(prompt string) (string, error) {
				if len(lines) == 0 {
					return "", tt.err
				}
				line := lines[0]
				lines = lines[1:]
				return line, nil
			}, "> ")
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("readTaskPrompt = (%q, %v), want (%q, %v)", got, err, tt.want, tt.err)
			}
			if len(lines) != 0 {
				t.Fatalf("unconsumed input: %q", lines)
			}
		})
	}
}
