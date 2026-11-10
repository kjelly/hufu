package team

import (
	"encoding/json/v2"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

func TestNativeMediaRouteRequiresFrozenWorkerBinding(t *testing.T) {
	primary := execution.ExecutionTarget{Backend: "fixture", Model: "vision"}
	route := &ExecutionRouteBinding{Name: "vision-route", Candidates: []execution.ExecutionTarget{primary, {Backend: "fixture", Model: "next"}}, FallbackOn: []ProviderFailureClass{ProviderRateLimited}}
	route.Digest = executionRouteDigest(route.Name, route.Candidates, route.FallbackOn)
	item := &TodoItem{Agent: "worker", ExecutionTarget: primary, ExecutionRoute: route}
	policy := &ExecutionPolicySnapshot{ExecutionRoutes: []ExecutionRoutePolicySnapshot{{Owner: "agent:worker", Route: route.Name, Digest: route.Digest}}}
	for _, test := range []struct {
		name   string
		mutate func(*TodoItem, *ExecutionPolicySnapshot)
		valid  bool
	}{
		{"frozen", func(*TodoItem, *ExecutionPolicySnapshot) {}, true},
		{"different worker", func(item *TodoItem, _ *ExecutionPolicySnapshot) { item.Agent = "other" }, false},
		{"missing binding", func(_ *TodoItem, policy *ExecutionPolicySnapshot) { policy.ExecutionRoutes = nil }, false},
		{"changed digest", func(item *TodoItem, _ *ExecutionPolicySnapshot) { item.ExecutionRoute.Digest = "changed" }, false},
		{"changed primary", func(item *TodoItem, _ *ExecutionPolicySnapshot) { item.ExecutionTarget.Model = "different" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := *item
			current.ExecutionRoute = item.ExecutionRoute.clone()
			frozen := *policy
			test.mutate(&current, &frozen)
			err := validateNativeMediaRoute(&current, &frozen)
			if test.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireMediaCode(t, err, media.Unsupported)
			}
		})
	}
}

func TestNativeMediaAttemptTargetFollowsCommittedFallbackAndRetry(t *testing.T) {
	targets := []execution.ExecutionTarget{{Backend: "fixture", Model: "first"}, {Backend: "fixture", Model: "second"}, {Backend: "fixture", Model: "third"}}
	route := &ExecutionRouteBinding{Name: "vision-route", Candidates: targets, FallbackOn: []ProviderFailureClass{ProviderRateLimited}}
	encode := func(value any) []byte {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	start := func(attempt, revision int) RunEvent {
		return RunEvent{RunID: "run", TaskID: "task", Type: string(EventTaskStarted), Actor: "worker",
			Payload: encode(map[string]int{taskStartedDispatchAttemptKey: attempt, "occurrence_revision": revision})}
	}
	fallback := func(attempt, from, to int) RunEvent {
		return RunEvent{RunID: "run", TaskID: "task", Type: string(EventExecutionFallbackDecided), Actor: "execution-route", Attempt: attempt,
			Payload: encode(nativeMediaRouteFallback{FromAttempt: attempt, FromTarget: targets[from], ToTarget: targets[to], Candidate: to, FailureClass: ProviderRateLimited, Route: route.Name})}
	}
	for _, test := range []struct {
		name              string
		events            []RunEvent
		attempt, revision int
		index             int
		code              media.Code
	}{
		{name: "primary", events: []RunEvent{start(1, 2)}, attempt: 1, revision: 2},
		{name: "fallback", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3)}, attempt: 2, revision: 3, index: 1},
		{name: "successive fallback", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3), fallback(2, 1, 2), start(3, 4)}, attempt: 3, revision: 4, index: 2},
		{name: "ordinary retry returns to primary", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3), start(3, 4)}, attempt: 3, revision: 4},
		{name: "same status checkpoint keeps target", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3), start(0, 4)}, attempt: 2, revision: 4, index: 1},
		{name: "new dispatch resets target", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3), start(1, 4)}, attempt: 1, revision: 4},
		{name: "no started attempt", attempt: 1, revision: 2, code: media.AccessDenied},
		{name: "stale revision", events: []RunEvent{start(1, 2)}, attempt: 1, revision: 3, code: media.AccessDenied},
		{name: "stale attempt", events: []RunEvent{start(1, 2)}, attempt: 2, revision: 2, code: media.AccessDenied},
		{name: "skipped attempt", events: []RunEvent{start(1, 2), start(3, 3)}, attempt: 3, revision: 3, code: media.AccessDenied},
		{name: "skipped candidate", events: []RunEvent{start(1, 2), fallback(1, 0, 2), start(2, 3)}, attempt: 2, revision: 3, code: media.AccessDenied},
		{name: "fallback before current attempt stops old request", events: []RunEvent{start(1, 2), fallback(1, 0, 1)}, attempt: 1, revision: 2, code: media.AccessDenied},
		{name: "fallback bound exhausted across retries", events: []RunEvent{start(1, 2), fallback(1, 0, 1), start(2, 3), start(3, 4), fallback(3, 0, 1), start(4, 5), fallback(4, 1, 2), start(5, 6)}, attempt: 5, revision: 6, code: media.AccessDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := &TodoItem{ID: "task", Agent: "worker", ExecutionTarget: targets[0], ExecutionRoute: route, OccurrenceRevision: test.revision}
			got, err := nativeMediaAttemptTarget(item, "run", test.attempt, test.events)
			if test.code != "" {
				requireMediaCode(t, err, test.code)
			} else if err != nil || got != targets[test.index] {
				t.Fatal("image route did not select the committed attempt target", got, err)
			}
		})
	}
	for _, change := range []string{"run", "actor", "attempt", "class", "target", "route"} {
		t.Run("changed fallback "+change, func(t *testing.T) {
			event := fallback(1, 0, 1)
			var payload nativeMediaRouteFallback
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "run":
				event.RunID = "other"
			case "actor":
				event.Actor = "model"
			case "attempt":
				event.Attempt = 9
			case "class":
				payload.FailureClass = ProviderAuthFailed
			case "target":
				payload.ToTarget = targets[2]
			case "route":
				payload.Route = "other"
			}
			event.Payload = encode(payload)
			item := &TodoItem{ID: "task", ExecutionTarget: targets[0], ExecutionRoute: route, OccurrenceRevision: 3}
			got, err := nativeMediaAttemptTarget(item, "run", 2, []RunEvent{start(1, 2), event, start(2, 3)})
			if change == "run" {
				if err != nil || got != targets[0] {
					t.Fatal("foreign run granted its fallback target", got, err)
				}
			} else {
				requireMediaCode(t, err, media.AccessDenied)
			}
		})
	}
}
