package inspect

import (
	"encoding/json/v2"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/buildinfo"
	"github.com/kjelly/hufu/internal/team"
)

func TestRunBuildProjection(t *testing.T) {
	original := &buildinfo.Info{Version: "1.0", Revision: "original", Source: "build_info"}
	other := &buildinfo.Info{Version: "2.0", Revision: "newer", Source: "build_info"}
	var events []team.RunEvent
	for _, item := range []struct {
		run   string
		build *buildinfo.Info
	}{{"original", original}, {"resumed", other}, {"legacy", nil}} {
		body, err := json.Marshal(team.LifecycleEventPayload{HufuBuild: item.build})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, team.RunEvent{Type: string(team.EventRunStarted), RunID: item.run, Payload: body})
	}
	var indexed []IndexedEvent
	for _, event := range events {
		indexed = append(indexed, IndexedEvent{Event: event})
	}
	for _, tc := range []struct {
		run  string
		want *buildinfo.Info
	}{{"original", original}, {"resumed", other}, {"legacy", nil}, {"missing", nil}} {
		if got := runBuild(events, tc.run); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("run %s build = %+v", tc.run, got)
		}
		data := projectTaskWithEvents(&team.TodoItem{ID: "1"}, InspectQuery{RunID: tc.run}, indexed)
		if !reflect.DeepEqual(data.HufuBuild, tc.want) {
			t.Fatalf("task run %s build = %+v", tc.run, data.HufuBuild)
		}
	}
}
