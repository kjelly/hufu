package team

import (
	"errors"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestToolPairValidationDiagnosesInvalidRequests(t *testing.T) {
	call := func(id string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "view"}}}
	}
	result := func(id string) fantasy.Message {
		return fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id, Output: fantasy.ToolResultOutputContentText{Text: "observed"}}}}
	}
	const id = "private-provider-call-id"
	for _, tc := range []struct {
		name     string
		messages []fantasy.Message
		reason   string
		index    int
	}{
		{name: "empty"},
		{name: "complete", messages: []fantasy.Message{call(id), result(id)}},
		{name: "parallel", messages: []fantasy.Message{call(id), call("other"), result("other"), result(id)}},
		{name: "empty_call", messages: []fantasy.Message{call("")}, reason: "empty_call_id"},
		{name: "empty_result", messages: []fantasy.Message{result("")}, reason: "empty_result_id"},
		{name: "orphan", messages: []fantasy.Message{result(id)}, reason: "result_without_call"},
		{name: "duplicate_call", messages: []fantasy.Message{call(id), result(id), call(id)}, reason: "duplicate_call_id", index: 2},
		{name: "duplicate_result", messages: []fantasy.Message{call(id), result(id), result(id)}, reason: "duplicate_result_id", index: 2},
		{name: "missing_result", messages: []fantasy.Message{call(id), call("other")}, reason: "missing_result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateToolPairs(tc.messages)
			if tc.reason == "" {
				if err != nil || !toolPairsIntact(tc.messages) {
					t.Fatalf("valid pairs rejected: %v", err)
				}
				return
			}
			detail, ok := errors.AsType[*toolPairingError](err)
			if !ok || detail.Reason != tc.reason || detail.MessageIndex != tc.index || detail.PartIndex != 0 {
				t.Fatalf("pairing diagnostic: %+v", err)
			}
			if toolPairsIntact(tc.messages) || len(detail.IDHash) != 16 || strings.Contains(err.Error(), id) {
				t.Fatal("invalid pairing was accepted or leaked the raw provider ID")
			}
			manager := NewContextWindowManager(nil, nil)
			admission, err := manager.Admit(t.Context(), ContextWindowRequest{Messages: tc.messages})
			if admission.RejectionReason != contextWindowReasonInvalidPairing || admission.Decision != ContextWindowCannotFit {
				t.Fatalf("admission did not fail closed: %+v", admission)
			}
			if _, ok := errors.AsType[*toolPairingError](err); !ok {
				t.Fatalf("admission lost diagnostic: %v", err)
			}
		})
	}
}
