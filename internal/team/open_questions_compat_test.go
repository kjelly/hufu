package team

import (
	"encoding/json"
	"fmt"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestOpenQuestionsTextCompatibilityBoundary(t *testing.T) {
	for _, gated := range []bool{false, true} {
		for _, tc := range []struct {
			name, input string
			valid       bool
		}{
			{"plain", `{"open_questions":["Follow up"]}`, true},
			{"legacy", `{"open_questions":[{"question":"Follow up"}]}`, true},
			{"wrapped", `{"open_questions":[{"$text":" Follow up "}]}`, true},
			{"empty", `{"open_questions":[{"$text":""}]}`, false},
			{"blank", `{"open_questions":[{"$text":" \n\t "}]}`, false},
			{"null", `{"open_questions":[{"$text":null}]}`, false},
			{"number", `{"open_questions":[{"$text":42}]}`, false},
			{"nested", `{"open_questions":[{"$text":{"$text":"Follow up"}}]}`, false},
			{"extra", `{"open_questions":[{"$text":"Follow up","extra":true}]}`, false},
			{"mixed", `{"open_questions":[{"$text":"Follow up","question":"other"}]}`, false},
			{"case", `{"open_questions":[{"$TEXT":"Follow up"}]}`, false},
			{"duplicate", `{"open_questions":[{"$text":"first","$text":"Follow up"}]}`, false},
			{"summary", `{"summary":{"$text":"Follow up"}}`, false},
			{"receipt", `{"receipt_ids":[{"$text":"Follow up"}]}`, false},
			{"evidence", `{"evidence":[{"$text":"Follow up"}]}`, false},
		} {
			t.Run(fmt.Sprintf("%s/gated=%t", tc.name, gated), func(t *testing.T) {
				c := &Coordinator{taskTracker: NewTaskTracker()}
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "question compatibility"}})[0]
				var tool fantasy.AgentTool = &submitResultTool{coordinator: c, todoID: item.ID}
				if gated {
					tool = c.gatePolicyTools([]fantasy.AgentTool{tool})[0]
				}
				ctx := tools.SetToolsAllowed(occurrenceTestContext(c, item.ID, 1), []string{submitResultToolName})
				// Avoid duplicate summary keys while preserving deliberately duplicate wrapper keys.
				input := `{"status":"success",` + tc.input[1:]
				if tc.name != "summary" {
					input = `{"status":"success","summary":"done",` + tc.input[1:]
				}
				response, err := tool.Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: input})
				if err != nil {
					t.Fatal(err)
				}
				got := c.GetTaskResult(item.ID)
				if !tc.valid {
					if !response.IsError || got != nil {
						t.Fatalf("invalid input accepted: response=%#v result=%#v", response, got)
					}
					return
				}
				if response.IsError || got == nil {
					t.Fatalf("valid input rejected: %#v", response)
				}
				raw, err := json.Marshal(got.OpenQuestions)
				if err != nil || string(raw) != `["Follow up"]` {
					t.Fatalf("noncanonical questions: %s error=%v", raw, err)
				}
			})
		}
	}
}
