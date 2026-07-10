package tools

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeEditInput(t *testing.T) {
	tests := []struct {
		name    string
		args    editArgs
		wantLen int
		wantErr bool
	}{
		{
			name: "valid edits",
			args: editArgs{
				Edits: []Edit{
					{OldText: "old1", NewText: "new1"},
					{OldText: "old2", NewText: "new2"},
				},
			},
			wantLen: 2,
			wantErr: false,
		},
		{
			name: "empty edits array",
			args: editArgs{
				Edits: []Edit{},
			},
			wantLen: 0,
			wantErr: true,
		},
		{
			name: "missing old_text",
			args: editArgs{
				Edits: []Edit{
					{OldText: "", NewText: "new1"},
				},
			},
			wantLen: 0,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeEditInput(tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("normalizeEditInput() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(got) != tt.wantLen {
				t.Errorf("normalizeEditInput() got length = %v, want %v", len(got), tt.wantLen)
			}
		})
	}
}

func TestFindMatch(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		edit      replacement
		wantStart int
		wantEnd   int
		wantFuzzy bool
		wantErr   bool
	}{
		{
			name:    "exact match single",
			content: "hello world\nfoo bar",
			edit: replacement{
				oldText: "world",
				newText: "universe",
				index:   0,
			},
			wantStart: 6,
			wantEnd:   11,
			wantFuzzy: false,
			wantErr:   false,
		},
		{
			name:    "exact match replace_all",
			content: "hello world\nworld peace",
			edit: replacement{
				oldText:    "world",
				newText:    "universe",
				replaceAll: true,
				index:      0,
			},
			wantStart: 6,
			wantEnd:   11,
			wantFuzzy: false,
			wantErr:   false,
		},
		{
			name:    "fuzzy match single",
			content: `He said “hello”`,
			edit: replacement{
				oldText: `He said "hello"`,
				newText: `He said 'hello'`,
				index:   0,
			},
			wantStart: 0,
			wantEnd:   19,
			wantFuzzy: true,
			wantErr:   false,
		},
		{
			name:    "fuzzy match replace_all",
			content: `“hello” and “world”`,
			edit: replacement{
				oldText:    `"hello"`,
				newText:    `'hello'`,
				replaceAll: true,
				index:      0,
			},
			wantStart: 0,
			wantEnd:   11,
			wantFuzzy: true,
			wantErr:   false,
		},
		{
			name:    "multiple matches without replace_all",
			content: "hello world\nhello universe",
			edit: replacement{
				oldText: "hello",
				newText: "hi",
				index:   0,
			},
			wantErr: true,
		},
		{
			name:    "not found",
			content: "hello world",
			edit: replacement{
				oldText: "foo",
				newText: "bar",
				index:   0,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findMatch(tt.content, tt.edit)
			if (err != nil) != tt.wantErr {
				t.Errorf("findMatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.start != tt.wantStart || got.end != tt.wantEnd || got.usedFuzzyMatch != tt.wantFuzzy {
					t.Errorf("findMatch() = %+v, want start %v, end %v, fuzzy %v", got, tt.wantStart, tt.wantEnd, tt.wantFuzzy)
				}
			}
		})
	}
}

func TestApplyEditsAndWrite(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "test.txt")
	content := "hello world\nfoo bar\nbaz qux"
	err := os.WriteFile(filePath, []byte(content), 0644)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		replacements []replacement
		wantContent  string
		wantErr      bool
	}{
		{
			name: "single exact edit",
			replacements: []replacement{
				{oldText: "world", newText: "universe", index: 0},
			},
			wantContent: "hello universe\nfoo bar\nbaz qux",
			wantErr:     false,
		},
		{
			name: "multiple edits",
			replacements: []replacement{
				{oldText: "world", newText: "universe", index: 0},
				{oldText: "bar", newText: "baz", index: 1},
			},
			wantContent: "hello universe\nfoo baz\nbaz qux",
			wantErr:     false,
		},
		{
			name: "replace all",
			replacements: []replacement{
				{oldText: "o", newText: "0", replaceAll: true, index: 0},
			},
			wantContent: "hell0 w0rld\nf00 bar\nbaz qux",
			wantErr:     false,
		},
		{
			name: "fuzzy edit",
			replacements: []replacement{
				{oldText: "foo bar", newText: "foo baz", index: 0},
			},
			wantContent: "hello world\nfoo baz\nbaz qux",
			wantErr:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.WriteFile(filePath, []byte(content), 0644)
			_, err := applyEditsAndWrite(filePath, "test.txt", tt.replacements)
			if (err != nil) != tt.wantErr {
				t.Errorf("applyEditsAndWrite() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				got, _ := os.ReadFile(filePath)
				if string(got) != tt.wantContent {
					t.Errorf("applyEditsAndWrite() got = %q, want %q", string(got), tt.wantContent)
				}
			}
		})
	}
}
