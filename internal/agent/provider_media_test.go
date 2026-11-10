package agent

import (
	"testing"

	"charm.land/fantasy"
)

func TestMediaDetectionRejectsNilPartsWithoutDereferencingThem(t *testing.T) {
	for _, test := range []struct {
		name      string
		part      fantasy.MessagePart
		reference bool
	}{
		{name: "nil"},
		{name: "text pointer", part: (*fantasy.TextPart)(nil)},
		{name: "file pointer", part: (*fantasy.FilePart)(nil)},
		{name: "reference pointer", part: (*MediaReferencePart)(nil), reference: true},
		{name: "tool result pointer", part: (*fantasy.ToolResultPart)(nil)},
		{name: "nil tool text", part: fantasy.ToolResultPart{Output: (*fantasy.ToolResultOutputContentText)(nil)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			messages := []fantasy.Message{{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{test.part}}}
			request := ProviderRequest{Call: &fantasy.Call{Prompt: messages}}
			if !ProviderRequestHasMedia(request) {
				t.Fatal("malformed nil part reached text-only admission")
			}
			if providerRequestHasReferences(request) != test.reference {
				t.Fatal("reference detection changed or dereferenced a nil content part")
			}
		})
	}
}
