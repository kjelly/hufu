package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestProjectDecisionReplaysRequestContractBinding(t *testing.T) {
	j := &memoryJournal{}
	if err := appendDecisionEvent(context.Background(), j, agent.EventRequestContractCommitted, decisionEvent{
		DecisionID: "dec-1", TaskID: "todo-1", ContractRef: "sha256:contract", ContractRevision: 3,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := projectDecision(context.Background(), j, "dec-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.TaskID != "todo-1" || state.ContractRef != "sha256:contract" || state.ContractRevision != 3 {
		t.Fatalf("replayed contract binding = %#v", state)
	}
}

func TestReduceToSessionDataReplaysRequestContractProjection(t *testing.T) {
	payload, err := json.Marshal(decisionEvent{DecisionID: "d1", TaskID: "t1", ContractRef: "sha256:contract", ContractRevision: 2, ContractArtifact: ArtifactRef{SHA256: "sha256:artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	session := ReduceToSessionData([]RunEvent{{Type: agent.EventRequestContractCommitted, Payload: payload}})
	if len(session.RequestContractProjections) != 1 || session.RequestContractProjections[0].ContractRevision != 2 {
		t.Fatalf("contract projection was not replayed: %#v", session.RequestContractProjections)
	}
}

func TestBuildRequestContractHashesMetadataFreeRedactedMaterial(t *testing.T) {
	cfg := agent.RequestContractConfig{
		Objective:       "ship safely",
		SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "tests pass"}},
	}
	first, data, err := BuildRequestContract("token=secret", "Ship?", cfg, 1, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := BuildRequestContract("token=secret", "Ship?", cfg, 9, time.Unix(99, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.MaterialHash != second.MaterialHash {
		t.Fatalf("metadata changed material identity: %q vs %q", first.ID, second.ID)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("contract artifact leaked secret: %s", data)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestContractConfigRejectsMalformedCriteria(t *testing.T) {
	for name, cfg := range map[string]agent.RequestContractConfig{
		"missing objective":   {Enabled: true, SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "ok"}}},
		"blank criterion":     {Enabled: true, Objective: "objective", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "", Statement: "ok"}}},
		"duplicate criterion": {Enabled: true, Objective: "objective", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "a"}, {ID: "S1", Statement: "b"}}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: malformed contract config was accepted", name)
		}
	}
}
