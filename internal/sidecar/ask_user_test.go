package sidecar

import (
	"context"
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

func TestNormalizeAskUserSelectionNumberedEcho(t *testing.T) {
	opts := []tools.AskUserTUIOption{{Label: "List the files"}, {Label: "Count only"}}
	tests := []struct {
		answer string
		want   string // "" means rejected
	}{
		{answer: "1. List the files", want: "List the files"},
		{answer: "2) count only", want: "Count only"},
		{answer: "2.Count only", want: "Count only"},
		{answer: "1. Count only"},
		{answer: "3. List the files"},
		{answer: "1. Something else"},
	}
	for _, test := range tests {
		t.Run(test.answer, func(t *testing.T) {
			resp, ok := normalizeAskUserSelection(tools.AskUserResponse{Answers: []string{test.answer}}, opts, "single_choice", false)
			if test.want == "" {
				if ok {
					t.Fatalf("accepted %+v, want rejection", resp)
				}
				return
			}
			if !ok || len(resp.Answers) != 1 || resp.Answers[0] != test.want {
				t.Fatalf("normalized = %+v ok=%v, want %q", resp, ok, test.want)
			}
		})
	}
}

// scriptedReplyAgent answers each call with the next scripted reply and
// records the prompts it received.
type scriptedReplyAgent struct {
	replies []string
	err     error
	prompts []string
}

func (a *scriptedReplyAgent) Generate(_ context.Context, call fantasy.AgentCall) (*fantasy.AgentResult, error) {
	a.prompts = append(a.prompts, call.Prompt)
	if a.err != nil {
		return nil, a.err
	}
	reply := a.replies[min(len(a.prompts), len(a.replies))-1]
	return &fantasy.AgentResult{Response: fantasy.Response{Content: fantasy.ResponseContent{fantasy.TextContent{Text: reply}}}}, nil
}

func (a *scriptedReplyAgent) Stream(context.Context, fantasy.AgentStreamCall) (*fantasy.AgentResult, error) {
	return nil, nil
}

func TestChooseAskUserResponseRetriesOnceWithTheProblem(t *testing.T) {
	opts := []tools.AskUserTUIOption{{Label: "Keep", Value: "keep"}, {Label: "Remove", Value: "remove"}}
	tests := []struct {
		name      string
		agent     *scriptedReplyAgent
		want      string
		wantErr   string
		wantCalls int
	}{
		{name: "valid first reply", agent: &scriptedReplyAgent{replies: []string{`{"answers":["Keep"]}`}}, want: "keep", wantCalls: 1},
		{name: "corrected on retry", agent: &scriptedReplyAgent{replies: []string{`{"answers":["maybe"]}`, `{"answers":["Remove"]}`}}, want: "remove", wantCalls: 2},
		{name: "still unusable", agent: &scriptedReplyAgent{replies: []string{`{"answers":["maybe"]}`, `not json`}}, wantErr: `"not json"`, wantCalls: 2},
		{name: "provider failure is not retried", agent: &scriptedReplyAgent{err: errors.New("provider down")}, wantErr: "provider down", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := &Sidecar{agent: test.agent}
			resp, err := s.ChooseAskUserResponse(t.Context(), "keep it?", "single_choice", opts, false)
			if len(test.agent.prompts) != test.wantCalls {
				t.Fatalf("selector calls = %d, want %d", len(test.agent.prompts), test.wantCalls)
			}
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want it to contain %s", err, test.wantErr)
				}
				return
			}
			if err != nil || len(resp.Answers) != 1 || resp.Answers[0] != test.want {
				t.Fatalf("response = %+v, %v; want %q", resp, err, test.want)
			}
			if test.wantCalls == 2 && !strings.Contains(test.agent.prompts[1], `"maybe"`) {
				t.Fatalf("retry prompt does not quote the rejected reply: %q", test.agent.prompts[1])
			}
		})
	}
}
