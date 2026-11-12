package inspect

import (
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestRecoveryTaskReplayRetainsPriorCheckpointWithinScope(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "result_only"}[recovery], func(t *testing.T) {
			payload := []byte(`{}`)
			if recovery {
				payload = []byte(`{"result_repair_grant":{"recovery":{}}}`)
			}
			events := []team.RunEvent{
				{ID: "foreign", SessionID: "other", RunID: "old", TaskID: "2", Type: string(team.EventTaskResultCollectionCheckpointed)},
				{ID: "created", SessionID: "session", RunID: "old", TaskID: "2", Type: string(team.EventTaskCreated)},
				{ID: "prior", SessionID: "session", RunID: "old", TaskID: "2", Type: string(team.EventTaskResultCollectionCheckpointed)},
				{ID: "unrelated", SessionID: "session", RunID: "old", TaskID: "3", Type: string(team.EventTaskCreated)},
				{ID: "start", SessionID: "session", RunID: "new", Type: "run_started"},
				{ID: "grant", SessionID: "session", RunID: "new", TaskID: "2", Type: string(team.EventTaskProtocolIncomplete), Payload: payload},
				{ID: "blocked", SessionID: "session", RunID: "new", TaskID: "2", Type: string(team.EventTaskBlocked)},
				{ID: "future", SessionID: "session", RunID: "later", TaskID: "2", Type: string(team.EventTaskResultCollectionCheckpointed)},
			}
			var lineage Lineage
			for _, event := range events {
				lineage.Events = append(lineage.Events, IndexedEvent{Event: event})
			}
			selected, err := selectRun(lineage, InspectQuery{RunID: "new"})
			if err != nil {
				t.Fatal(err)
			}
			ids := func(events []team.RunEvent) []string {
				var ids []string
				for _, event := range events {
					ids = append(ids, event.ID)
				}
				return ids
			}
			want := []string{"start", "grant", "blocked"}
			if !slices.Equal(ids(selected.runEvents), want) {
				t.Fatal("visible run timeline included another run")
			}
			if recovery {
				want = append([]string{"created", "prior"}, want...)
			}
			if !slices.Equal(ids(selected.taskEvents), want) {
				t.Fatalf("replay scope=%v, want %v", ids(selected.taskEvents), want)
			}
		})
	}
}

func TestRecoveryTaskReplayIncludesContractUpgradeAncestry(t *testing.T) {
	events := []team.RunEvent{
		{ID: "policy", SessionID: "parent", RunID: "old", Type: string(team.EventExecutionPolicySnapshot)},
		{ID: "source", SessionID: "parent", RunID: "old", TaskID: "1", Type: string(team.EventTaskCreated)},
		{ID: "work", SessionID: "parent", RunID: "old", TaskID: "2", Type: string(team.EventTaskCreated)},
		{ID: "upgrade", SessionID: "parent", Type: string(team.EventContractRecovery)},
		{ID: "start", SessionID: "child", RunID: "new", Type: "run_started"},
		{ID: "grant", SessionID: "child", RunID: "new", TaskID: "2", Type: string(team.EventTaskProtocolIncomplete)},
		{ID: "future", SessionID: "child", RunID: "later", Type: "run_started"},
	}
	var lineage Lineage
	for _, event := range events {
		lineage.Events = append(lineage.Events, IndexedEvent{Event: event})
	}
	selected, err := selectRun(lineage, InspectQuery{RunID: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.runEvents) != 2 || !slices.EqualFunc(selected.taskEvents, events[:6], func(a, b team.RunEvent) bool { return a.ID == b.ID }) {
		t.Fatal("contract upgrade replay omitted its parent sources or included future events")
	}
}
