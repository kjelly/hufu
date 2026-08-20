package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
)

func TestSplitLeadingJSONValue(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantOK      bool
		wantHead    string
		wantTrailer string
	}{
		{
			name:        "two concatenated objects",
			input:       `{"file_path":"a.go"}{"pattern":"b","path":"c"}`,
			wantOK:      true,
			wantHead:    `{"file_path":"a.go"}`,
			wantTrailer: `{"pattern":"b","path":"c"}`,
		},
		{
			name:        "three concatenated objects",
			input:       `{"a":1}{"b":2}{"c":3}`,
			wantOK:      true,
			wantHead:    `{"a":1}`,
			wantTrailer: `{"b":2}{"c":3}`,
		},
		{
			name:   "single clean object",
			input:  `{"file_path":"a.go"}`,
			wantOK: false,
		},
		{
			name:   "single clean object with trailing whitespace",
			input:  "{\"file_path\":\"a.go\"}  \n",
			wantOK: false,
		},
		{
			name:   "truncated json",
			input:  `{"file_path":`,
			wantOK: false,
		},
		{
			name:   "trailing garbage that is not json",
			input:  `{"a":1}not json`,
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head, trailing, ok := splitLeadingJSONValue(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (head=%q trailing=%q)", ok, tc.wantOK, head, trailing)
			}
			if !tc.wantOK {
				return
			}
			if head != tc.wantHead {
				t.Errorf("head = %q, want %q", head, tc.wantHead)
			}
			if trailing != tc.wantTrailer {
				t.Errorf("trailing = %q, want %q", trailing, tc.wantTrailer)
			}
		})
	}
}

func TestRepairConcatenatedToolCall(t *testing.T) {
	t.Run("recovers the first tool call from a concatenated pair", func(t *testing.T) {
		original := fantasy.ToolCallContent{
			ToolCallID: "call_1",
			ToolName:   "view",
			Input:      `{"file_path":"a.go"}{"pattern":"b","path":"c","include":"*.go"}`,
			Invalid:    true,
		}
		got, err := RepairConcatenatedToolCall(context.Background(), fantasy.ToolCallRepairOptions{
			OriginalToolCall: original,
			ValidationError:  errors.New("invalid JSON input: invalid character '{' after top-level value"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected a repaired tool call, got nil")
		}
		if got.Input != `{"file_path":"a.go"}` {
			t.Errorf("Input = %q, want the first concatenated object only", got.Input)
		}
		if got.ToolName != original.ToolName || got.ToolCallID != original.ToolCallID {
			t.Errorf("repaired call changed identity: got %+v", got)
		}
		if got.Invalid {
			t.Errorf("repaired call should not still be marked Invalid")
		}
	})

	t.Run("falls back to jsonrepair for ordinary malformed json", func(t *testing.T) {
		original := fantasy.ToolCallContent{
			ToolCallID: "call_2",
			ToolName:   "view",
			// Trailing comma: not the concatenation shape, but jsonrepair can
			// fix it, matching fantasy's own default behavior.
			Input: `{"file_path":"a.go",}`,
		}
		got, err := RepairConcatenatedToolCall(context.Background(), fantasy.ToolCallRepairOptions{
			OriginalToolCall: original,
			ValidationError:  errors.New("invalid JSON input: invalid character '}' looking for beginning of object key string"),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Fatal("expected jsonrepair to recover a repaired tool call, got nil")
		}
		if got.Input != `{"file_path": "a.go"}` {
			t.Errorf("Input = %q, want the trailing comma stripped", got.Input)
		}
	})

	t.Run("gives up on input that cannot be recovered at all", func(t *testing.T) {
		original := fantasy.ToolCallContent{
			ToolCallID: "call_3",
			ToolName:   "view",
			Input:      "",
		}
		got, err := RepairConcatenatedToolCall(context.Background(), fantasy.ToolCallRepairOptions{
			OriginalToolCall: original,
			ValidationError:  errors.New("invalid JSON input: unexpected end of JSON input"),
		})
		if err == nil {
			t.Fatal("expected an error for unrecoverable input")
		}
		if got != nil {
			t.Errorf("expected a nil tool call on failure, got %+v", got)
		}
	})
}
