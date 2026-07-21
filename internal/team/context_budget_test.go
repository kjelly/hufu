package team

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func toolResultMsg(id, text string) fantasy.Message {
	return fantasy.Message{
		Role: fantasy.MessageRoleTool,
		Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{
				ToolCallID: id,
				Output:     fantasy.ToolResultOutputContentText{Text: text},
			},
		},
	}
}

func TestMessageTextSize(t *testing.T) {
	cases := []struct {
		name string
		msg  fantasy.Message
		want int
	}{
		{"text part", fantasy.NewUserMessage("hello"), 5},
		{"tool result counted", toolResultMsg("c1", strings.Repeat("y", 300)), 300},
		{"tool call input counted", fantasy.Message{
			Role:    fantasy.MessageRoleAssistant,
			Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "bash", Input: strings.Repeat("z", 50)}},
		}, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageTextSize(tc.msg); got != tc.want {
				t.Errorf("size = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCapStepMessages(t *testing.T) {
	bigResult := strings.Repeat("a", 60000)

	t.Run("under budget returns nil", func(t *testing.T) {
		msgs := []fantasy.Message{fantasy.NewUserMessage("goal"), toolResultMsg("c1", "small")}
		if got := capStepMessages(msgs); got != nil {
			t.Fatal("expected nil for messages already under budget")
		}
	})

	t.Run("squeezes old results but protects head and recent messages", func(t *testing.T) {
		msgs := []fantasy.Message{
			fantasy.NewSystemMessage("system prompt"),
			fantasy.NewUserMessage("the original goal"),
		}
		for i := range 3 {
			msgs = append(msgs, toolResultMsg(string(rune('a'+i)), bigResult))
		}
		// Pad with recent small messages so the big ones fall outside the
		// protected tail window.
		for range recentMessagesProtected {
			msgs = append(msgs, fantasy.NewUserMessage("recent"))
		}

		got := capStepMessages(msgs)
		if got == nil {
			t.Fatal("expected shaping for over-budget messages")
		}
		if len(got) != len(msgs) {
			t.Fatalf("message count changed: %d -> %d; shaping must never drop messages", len(msgs), len(got))
		}
		for i := range headMessagesProtected {
			if messageTextSize(got[i]) != messageTextSize(msgs[i]) {
				t.Errorf("head message %d (system/goal) must not be squeezed", i)
			}
		}
		if messageTextSize(got[headMessagesProtected]) >= messageTextSize(msgs[headMessagesProtected]) {
			t.Error("old bulky tool result should have been squeezed")
		}
		totalTokens, _ := defaultCounter.CountMessages(context.Background(), "default", got)
		if totalTokens > defaultStepContextBudgetTokens {
			t.Errorf("total tokens after shaping = %d, want <= %d", totalTokens, defaultStepContextBudgetTokens)
		}
		// Original slice must be untouched.
		if messageTextSize(msgs[headMessagesProtected]) != len(bigResult) {
			t.Error("input slice was mutated")
		}
	})
}

func TestSqueezeTextKeepsHeadAndTail(t *testing.T) {
	s := strings.Repeat("H", 500) + strings.Repeat("T", 500)
	got := squeezeText(s, 100)
	if !strings.HasPrefix(got, "H") || !strings.HasSuffix(got, "T") {
		t.Errorf("head/tail not preserved: %q…%q", got[:10], got[len(got)-10:])
	}
	if !strings.Contains(got, "truncated") {
		t.Error("missing elision marker")
	}
}
