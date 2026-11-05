package media

import (
	"encoding/json/v2"
	"os"
	"testing"
)

func testInputSnapshot(t *testing.T) InputSnapshot {
	t.Helper()
	r, err := Validate(t.Context(), testPNG(t), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := Digest("policy")
	if err != nil {
		t.Fatal(err)
	}
	s := InputSnapshot{RunID: "run", BranchID: "main", InvocationID: "invocation", TurnID: "turn", PolicyHash: policy,
		Input: TurnInput{Parts: []Part{{Type: "text", Text: "compare"}, {Type: "image", MediaID: r.ID}, {Type: "image", MediaID: r.ID}}}, Refs: []Ref{r}}
	for _, index := range []int{1, 2} {
		s.Occurrences = append(s.Occurrences, Occurrence{RunID: s.RunID, BranchID: s.BranchID, InvocationID: s.InvocationID, TurnID: s.TurnID, PartIndex: index, MediaID: r.ID, Origin: "user_file"})
	}
	s, err = SealSnapshot(s, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMediaInputSnapshot(t *testing.T) {
	s := testInputSnapshot(t)
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored InputSnapshot
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(Limits{}); err != nil {
		t.Fatal(err)
	}
	if len(restored.Occurrences) != 2 || restored.Input.Parts[1].MediaID != restored.Input.Parts[2].MediaID {
		t.Fatal("repeated image occurrence was lost")
	}
	for _, tt := range []struct {
		name   string
		mutate func(*InputSnapshot)
	}{
		{"text-and-image", func(s *InputSnapshot) { s.Input.Parts[1].Text = "injected" }},
		{"unknown-part", func(s *InputSnapshot) { s.Input.Parts[1].Type = "video" }},
		{"extra-owner", func(s *InputSnapshot) { s.Occurrences[0].RunID = "other" }},
		{"wrong-index", func(s *InputSnapshot) { s.Occurrences[0].PartIndex = 2 }},
		{"unattested-tool", func(s *InputSnapshot) { s.Occurrences[0].Origin = "mcp_tool" }},
		{"raw-identity", func(s *InputSnapshot) { s.TurnID = "data:image/png;base64,encoded" }},
		{"text-tamper", func(s *InputSnapshot) { s.Input.Parts[0].Text = "changed" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bad := s.Clone()
			tt.mutate(&bad)
			if err := bad.Validate(Limits{}); err == nil {
				t.Fatal("invalid snapshot accepted")
			}
		})
	}
	if err := s.Validate(Limits{MaxCount: 1}); err == nil {
		t.Fatal("repeated references bypassed the count limit")
	}
}

func TestMediaTaskBinding(t *testing.T) {
	s := testInputSnapshot(t)
	b := TaskBinding{Version: Version, RunID: s.RunID, BranchID: s.BranchID, TaskID: "1", Agent: "worker", Purpose: "image_analysis", PolicyHash: s.PolicyHash,
		Selections: []Selection{{SnapshotID: s.ID, SnapshotHash: s.Hash, TurnID: s.TurnID, PartIndex: 1}, {SnapshotID: s.ID, SnapshotHash: s.Hash, TurnID: s.TurnID, PartIndex: 2}}, Refs: []Ref{s.Refs[0], s.Refs[0]}}
	b, err := b.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Validate(Limits{}); err != nil {
		t.Fatal(err)
	}
	changed := *b.Clone()
	changed.Selections[0], changed.Selections[1] = changed.Selections[1], changed.Selections[0]
	changed, err = changed.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if changed.InputDigest == b.InputDigest || changed.Hash == b.Hash {
		t.Fatal("selection order was omitted from binding identity")
	}
	owner := *b.Clone()
	owner.TaskID = "2"
	if err := owner.Validate(Limits{}); err == nil {
		t.Fatal("owner substitution preserved binding validity")
	}
}

func TestMediaTaskInvocationModePreservesLegacyBinding(t *testing.T) {
	// This reference-only fixture uses the v1 field order and SHA contract
	// before invocation_mode existed; it is not an admitted CAS object.
	data, err := os.ReadFile("testdata/task-binding-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy TaskBinding
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Validate(Limits{}); err != nil {
		t.Fatal("optional invocation mode changed a persisted v1 binding hash", err)
	}
	for _, mode := range []InvocationMode{InvocationSingleTask, InvocationDirectAgent, "unknown"} {
		t.Run(string(mode), func(t *testing.T) {
			bound := *legacy.Clone()
			bound.InvocationMode = mode
			if err := bound.Validate(Limits{}); err == nil {
				t.Fatal("changing the invocation mode preserved an old grant")
			}
			bound, err := bound.Seal()
			if err != nil {
				t.Fatal(err)
			}
			if bound.Hash == legacy.Hash || bound.InputDigest != legacy.InputDigest {
				t.Fatal("invocation mode lost its authority or changed content identity")
			}
			err = bound.Validate(Limits{})
			if mode == "unknown" {
				if err == nil {
					t.Fatal("unknown invocation mode accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
