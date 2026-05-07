package tools

import (
	"testing"
)

func TestRewriteBashRedirects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple redirect",
			input: "echo hello > file.txt",
			want:  "echo hello | tee file.txt",
		},
		{
			name:  "append redirect",
			input: "echo hello >> file.txt",
			want:  "echo hello | tee -a file.txt",
		},
		{
			name:  "no redirect",
			input: "ls -la",
			want:  "ls -la",
		},
		{
			name:  "redirect with path",
			input: "cat input.txt > /tmp/output.txt",
			want:  "cat input.txt | tee /tmp/output.txt",
		},
		{
			name:  "append with path",
			input: "echo log >> /var/log/app.log",
			want:  "echo log | tee -a /var/log/app.log",
		},
		{
			name:  "multiline with redirect",
			input: "echo a\necho b > out.txt\necho c",
			want:  "echo a\necho b | tee out.txt\necho c",
		},
		{
			name:  "complex command with pipe",
			input: "grep pattern file | sort > output.txt",
			want:  "grep pattern file | sort > output.txt",
		},
		{
			name:  "complex command with &&",
			input: "cd dir && echo done > out.txt",
			want:  "cd dir && echo done > out.txt",
		},
		{
			name:  "complex command with ||",
			input: "cmd1 || echo fail > err.txt",
			want:  "cmd1 || echo fail > err.txt",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "redirect with quoted filename containing spaces",
			input: `echo data > "my file.txt"`,
			want:  `echo data > "my file.txt"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteBashRedirects(tt.input)
			if got != tt.want {
				t.Errorf("rewriteBashRedirects(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRewriteLineRedirects(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "simple redirect",
			input: "echo hello > file.txt",
			want:  "echo hello | tee file.txt",
		},
		{
			name:  "append redirect",
			input: "echo hello >> file.txt",
			want:  "echo hello | tee -a file.txt",
		},
		{
			name:  "no redirect",
			input: "ls -la",
			want:  "ls -la",
		},
		{
			name:  "pipe command not rewritten",
			input: "grep foo | sort > out.txt",
			want:  "grep foo | sort > out.txt",
		},
		{
			name:  "&& command not rewritten",
			input: "cd /tmp && ls > out.txt",
			want:  "cd /tmp && ls > out.txt",
		},
		{
			name:  "|| command not rewritten",
			input: "cmd || echo fail > err.txt",
			want:  "cmd || echo fail > err.txt",
		},
		{
			name:  "redirect with absolute path",
			input: "echo data > /tmp/out.txt",
			want:  "echo data | tee /tmp/out.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rewriteLineRedirects(tt.input)
			if got != tt.want {
				t.Errorf("rewriteLineRedirects(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}