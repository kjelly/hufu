package team

import (
	"encoding/json/v2"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/media"
)

func validateNativeMediaRoute(item *TodoItem, policy *ExecutionPolicySnapshot) error {
	route := item.ExecutionRoute
	if route == nil || len(route.Candidates) < 2 {
		return nil
	}
	if len(route.Candidates) > executionRouteMaxCandidates || route.Candidates[0] != item.ExecutionTarget ||
		route.Digest != executionRouteDigest(route.Name, route.Candidates, route.FallbackOn) {
		return media.Reject(media.Unsupported, "image route differs from its frozen execution contract")
	}
	for _, frozen := range policy.ExecutionRoutes {
		if frozen.Owner == "agent:"+strings.ToLower(item.Agent) && frozen.Route == route.Name && frozen.Digest == route.Digest {
			return nil
		}
	}
	return media.Reject(media.Unsupported, "image route is not bound by the frozen worker policy")
}

type nativeMediaRouteState struct {
	attempt, index, used, revision int
	pending                        *nativeMediaRouteFallback
}

type nativeMediaRouteFallback struct {
	FromAttempt  int                       `json:"from_attempt"`
	FromTarget   execution.ExecutionTarget `json:"from_target"`
	ToTarget     execution.ExecutionTarget `json:"to_target"`
	Candidate    int                       `json:"candidate_index"`
	FailureClass ProviderFailureClass      `json:"failure_class"`
	Route        string                    `json:"route"`
}

// nativeMediaAttemptTarget replays the runtime's actual attempt choices. A
// model appearing in a frozen candidate list is insufficient permission to
// receive images. A fallback decision must precede the next started attempt;
// an ordinary retry returns to the primary, matching executionFallbackState.
func nativeMediaAttemptTarget(item *TodoItem, runID string, attempt int, events []RunEvent) (execution.ExecutionTarget, error) {
	if item == nil {
		return execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image route has no canonical occurrence")
	}
	route := item.ExecutionRoute
	if route == nil || len(route.Candidates) < 2 {
		return item.ExecutionTarget, nil
	}
	if len(route.Candidates) > executionRouteMaxCandidates || route.Candidates[0] != item.ExecutionTarget {
		return execution.ExecutionTarget{}, media.Reject(media.BindingConflict, "image route has an invalid primary target")
	}
	state := nativeMediaRouteState{}
	for _, event := range events {
		if event.RunID != runID || event.TaskID != item.ID {
			continue
		}
		switch event.Type {
		case string(EventTaskCreated):
			state = nativeMediaRouteState{}
		case string(EventTaskStarted):
			if err := state.start(event); err != nil {
				return execution.ExecutionTarget{}, err
			}
		case string(EventExecutionFallbackDecided):
			if err := state.fallback(event, route); err != nil {
				return execution.ExecutionTarget{}, err
			}
		}
	}
	if attempt < 1 || state.attempt != attempt || state.revision != item.OccurrenceRevision || state.pending != nil {
		return execution.ExecutionTarget{}, media.Reject(media.AccessDenied, "image route attempt is missing, stale or already falling back")
	}
	return route.Candidates[state.index], nil
}

func (s *nativeMediaRouteState) start(event RunEvent) error {
	var started struct {
		Attempt  int `json:"dispatch_attempt"`
		Revision int `json:"occurrence_revision"`
	}
	if err := json.Unmarshal(event.Payload, &started); err != nil {
		return media.Reject(media.BindingConflict, "image route attempt checkpoint is invalid")
	}
	if started.Revision < 1 {
		return media.Reject(media.AccessDenied, "image route attempt has no occurrence revision")
	}
	if started.Attempt == 1 {
		*s = nativeMediaRouteState{attempt: 1, revision: started.Revision}
		return nil
	}
	s.revision = started.Revision
	if started.Attempt == 0 {
		// Same-status checkpoints do not choose another route candidate.
		return nil
	}
	if started.Attempt != s.attempt+1 || s.attempt < 1 {
		return media.Reject(media.AccessDenied, "image route skipped its preceding attempt")
	}
	s.attempt = started.Attempt
	s.index = 0
	if s.pending != nil {
		s.index = s.pending.Candidate
		s.used++
	}
	s.pending = nil
	return nil
}

func (s *nativeMediaRouteState) fallback(event RunEvent, route *ExecutionRouteBinding) error {
	var fallback nativeMediaRouteFallback
	if err := json.Unmarshal(event.Payload, &fallback); err != nil {
		return media.Reject(media.BindingConflict, "image route fallback checkpoint is invalid")
	}
	if s.attempt < 1 || s.pending != nil || event.Actor != "execution-route" || event.Attempt != fallback.FromAttempt || fallback.FromAttempt != s.attempt || fallback.Route != route.Name ||
		fallback.Candidate != s.index+1 || fallback.Candidate >= len(route.Candidates) || s.used >= len(route.Candidates)-1 ||
		fallback.FromTarget != route.Candidates[s.index] || fallback.ToTarget != route.Candidates[fallback.Candidate] ||
		!slices.Contains(route.FallbackOn, fallback.FailureClass) {
		return media.Reject(media.AccessDenied, "image route fallback is outside its current frozen candidate")
	}
	s.pending = &fallback
	return nil
}
