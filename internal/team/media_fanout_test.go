package team

import (
	"bytes"
	"encoding/json/v2"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

func TestMediaFanoutGrantPreservesLegacyPayloadAndComparableRoundTrip(t *testing.T) {
	var legacy MediaProviderAdmittedPayload
	encoded, err := json.Marshal(legacy)
	if err != nil || bytes.Contains(encoded, []byte("model_fanout")) {
		t.Fatal("text or single-worker permission gained a fanout field", err)
	}
	grant := MediaProviderFanoutGrant{Version: mediaProviderFanoutGrantVersion, TopologyIndex: 1,
		ModelExecutionID: "model-execution-d9cbe281cf30758392ca8f5b182affce", ParentRevision: 7, ParentAttempt: 1, ParentDispatchID: "parent-dispatch"}
	for _, test := range []struct {
		name   string
		mutate func(*MediaProviderFanoutGrant)
		valid  bool
	}{
		{"valid", func(*MediaProviderFanoutGrant) {}, true},
		{"unknown version", func(g *MediaProviderFanoutGrant) { g.Version++ }, false},
		{"negative index", func(g *MediaProviderFanoutGrant) { g.TopologyIndex = -1 }, false},
		{"unlaunched index", func(g *MediaProviderFanoutGrant) { g.TopologyIndex = maxConcurrentModels }, false},
		{"missing model execution", func(g *MediaProviderFanoutGrant) { g.ModelExecutionID = "" }, false},
		{"missing parent revision", func(g *MediaProviderFanoutGrant) { g.ParentRevision = 0 }, false},
		{"missing parent attempt", func(g *MediaProviderFanoutGrant) { g.ParentAttempt = 0 }, false},
		{"missing parent lease", func(g *MediaProviderFanoutGrant) { g.ParentDispatchID = "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := grant
			test.mutate(&current)
			err := current.validate()
			if !test.valid {
				requireMediaCode(t, err, media.AccessDenied)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			payload := MediaProviderAdmittedPayload{ModelFanout: current}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var restored MediaProviderAdmittedPayload
			if err := json.Unmarshal(encoded, &restored); err != nil || restored != payload {
				t.Fatal("durable leaf permission does not compare equal at the final HTTP gate", err)
			}
		})
	}
}

func TestMediaFanoutProviderOwnerRequiresCanonicalParentAndSlot(t *testing.T) {
	primary, next := execution.ExecutionTarget{Backend: "fixture", Model: "vision"}, execution.ExecutionTarget{Backend: "fixture", Model: "next"}
	base := TodoItem{ID: "task", Agent: "worker", Status: TaskInProgress, OccurrenceRevision: 7, DispatchID: "parent-dispatch",
		ExecutionTarget: primary, ExecutionTopology: []execution.ExecutionTarget{primary, next}, ModelTopology: []string{"fixture/vision", "fixture/next"}}
	payload := MediaProviderAdmittedPayload{OccurrenceRevision: 8, Attempt: 1, DispatchID: "leaf-dispatch",
		ModelFanout: MediaProviderFanoutGrant{Version: mediaProviderFanoutGrantVersion, TopologyIndex: 1,
			ModelExecutionID: "model-execution-d9cbe281cf30758392ca8f5b182affce", ParentRevision: 7, ParentAttempt: 1, ParentDispatchID: "parent-dispatch"}}
	for _, test := range []struct {
		name   string
		mutate func(*TodoItem, *MediaProviderAdmittedPayload)
		valid  bool
	}{
		{"valid independent leaf", func(*TodoItem, *MediaProviderAdmittedPayload) {}, true},
		{"stale parent", func(item *TodoItem, _ *MediaProviderAdmittedPayload) { item.OccurrenceRevision++ }, false},
		{"different parent lease", func(item *TodoItem, _ *MediaProviderAdmittedPayload) { item.DispatchID = "different" }, false},
		{"different parent attempt", func(_ *TodoItem, p *MediaProviderAdmittedPayload) { p.ModelFanout.ParentAttempt++ }, false},
		{"parent reused as leaf", func(_ *TodoItem, p *MediaProviderAdmittedPayload) {
			p.OccurrenceRevision = p.ModelFanout.ParentRevision
		}, false},
		{"missing leaf grant", func(_ *TodoItem, p *MediaProviderAdmittedPayload) { p.ModelFanout = MediaProviderFanoutGrant{} }, false},
		{"wrong execution identity", func(_ *TodoItem, p *MediaProviderAdmittedPayload) { p.ModelFanout.ModelExecutionID = "other" }, false},
		{"wrong slot", func(_ *TodoItem, p *MediaProviderAdmittedPayload) { p.ModelFanout.TopologyIndex = 0 }, false},
		{"unknown slot", func(_ *TodoItem, p *MediaProviderAdmittedPayload) { p.ModelFanout.TopologyIndex = 9 }, false},
		{"fanout grant on singleton", func(item *TodoItem, _ *MediaProviderAdmittedPayload) {
			item.ExecutionTopology, item.ModelTopology = item.ExecutionTopology[:1], item.ModelTopology[:1]
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			item, current := base, payload
			test.mutate(&item, &current)
			target, err := verifyNativeMediaProviderOwner(RunEvent{RunID: "run", TaskID: "task", Actor: "worker"}, current, &item, nil)
			if test.valid {
				if err != nil || target != next {
					t.Fatal("valid leaf did not retain its admitted target", target, err)
				}
			} else {
				requireMediaCode(t, err, media.AccessDenied)
			}
		})
	}
}
