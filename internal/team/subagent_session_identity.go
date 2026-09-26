package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
)

// Session identity rule. A task's backend session identity is
// (backend, session ID) and never changes on a branch. Attempt, execution
// world, cwd, and turn are execution details that may change between
// attempts.
//
// The anchor is the first backend_session_bound event on the active lineage
// written under the per-task key backendSessionBoundKey. After the anchor,
// every binding a task's lineage shows, whether in a session-bound event or
// in a task transition's backend_binding, must name the anchor's identity;
// anything else is an ExecutionIdentityConflictError, never resolved by
// "last one wins". A task with no anchor predates the per-task key: its last
// binding wins, as in the replay reducer, and its next bind writes the
// anchor.
//
// The rule is enforced when an attempt resolves or binds its session, not in
// startup replay: hufu reconcile and hufu retry pass through that startup
// preflight, and one task's conflict must not lock them out.

// durableSessionBinding is a session binding read from the event log.
type durableSessionBinding struct {
	EventID string
	Binding BackendBinding
}

// resumableBackendSessionID returns the session an attempt must resume. The
// durable identity is the authority: a projected binding must match it, and
// when the projection is empty (after a crash, an unknown sync outcome, or a
// failed projection update) the durable session is resumed instead of a new
// one opened. Only a binding that exists solely in the projection, such as
// one restored from a checkpoint written before canonical session events,
// is used without durable backing; binding it writes the anchor.
func (c *Coordinator) resumableBackendSessionID(ctx context.Context, request AttemptRequest) (string, error) {
	durable, err := c.durableBackendSessionBinding(ctx, request.TaskID)
	if err != nil {
		return "", err
	}
	projected := ""
	if request.ProviderBinding != nil {
		projected = request.ProviderBinding.SessionID
	}
	if durable == nil {
		return projected, nil
	}
	target, err := c.sessionBindingTarget(request.TaskID)
	if err != nil {
		return "", err
	}
	if !execution.BackendNamesEqual(durable.Binding.Backend, target.Backend) {
		return "", &ExecutionIdentityConflictError{TaskID: request.TaskID, EventID: durable.EventID, Reason: fmt.Sprintf("durable session belongs to backend %q, not %q", durable.Binding.Backend, target.Backend)}
	}
	if projected != "" && projected != durable.Binding.SessionID {
		return "", &ExecutionIdentityConflictError{TaskID: request.TaskID, EventID: durable.EventID, Reason: fmt.Sprintf("projected session %q disagrees with durable session %q", projected, durable.Binding.SessionID)}
	}
	return durable.Binding.SessionID, nil
}

// durableBackendSessionBinding applies the session identity rule to the
// task's active branch lineage. It returns nil when no binding names a
// session.
func (c *Coordinator) durableBackendSessionBinding(ctx context.Context, taskID string) (*durableSessionBinding, error) {
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		// A failed sync invalidates the store's cached state although the
		// event may have reached disk. Re-verify the chain from disk once:
		// that either exposes such a binding or reports why it cannot.
		if verifyErr := c.EventJournal().VerifyHashChain(context.WithoutCancel(ctx)); verifyErr != nil {
			return nil, lineageReadFailure(verifyErr, err)
		}
		if events, err = c.readActiveLineageEvents(ctx); err != nil {
			return nil, lineageReadFailure(err, nil)
		}
	}
	return sessionIdentityFromLineage(events, taskID)
}

// lineageReadFailure classifies a lineage that could not be read. A
// file-system error is a storage fault an operator can fix and then retry.
// Anything else means the durable evidence itself did not validate (a broken
// hash chain, malformed branch metadata), so no worker may run on it.
func lineageReadFailure(cause, firstRead error) error {
	err := fmt.Errorf("read durable session binding: %w", cause)
	if firstRead != nil {
		err = fmt.Errorf("%w (first read: %v)", err, firstRead)
	}
	var pathErr *fs.PathError
	if errors.As(cause, &pathErr) {
		return withFailureClassOverride(err, FailureEnvironment)
	}
	return withFailureClassOverride(err, FailureIdentityConflict)
}

// sessionIdentityFromLineage is the session identity rule over one lineage.
func sessionIdentityFromLineage(events []RunEvent, taskID string) (*durableSessionBinding, error) {
	anchorKey := backendSessionBoundKey(taskID)
	var anchor, last *durableSessionBinding
	for _, event := range events {
		observed, ok := sessionBindingObservation(event, taskID)
		if !ok {
			continue
		}
		if anchor == nil && event.Type == string(EventBackendSessionBound) && event.IdempotencyKey == anchorKey {
			anchor = &observed
			continue
		}
		if anchor != nil {
			if err := backendSessionConflict(taskID, anchor, observed.Binding.Backend, observed.Binding.SessionID); err != nil {
				var conflict *ExecutionIdentityConflictError
				if errors.As(err, &conflict) {
					conflict.Reason = fmt.Sprintf("event %q rebinds the task from anchor %q: %s", observed.EventID, anchor.EventID, conflict.Reason)
				}
				return nil, err
			}
			continue
		}
		last = &observed
	}
	if anchor != nil {
		return anchor, nil
	}
	return last, nil
}

// sessionBindingObservation reads the session binding one event shows for
// the task: a session-bound event, or a task transition that carries the
// task's binding, as the replay reducer applies it.
func sessionBindingObservation(event RunEvent, taskID string) (durableSessionBinding, bool) {
	switch {
	case event.Type == string(EventBackendSessionBound) || event.Type == string(EventProviderSessionBound):
		if event.TaskID != taskID {
			return durableSessionBinding{}, false
		}
		return durableSessionBindingFromEvent(event)
	case strings.HasPrefix(event.Type, "task_") && event.Type != "task_removed":
		var payload struct {
			ID              string           `json:"id"`
			BackendBinding  *BackendBinding  `json:"backend_binding"`
			ProviderBinding *ProviderBinding `json:"provider_binding"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return durableSessionBinding{}, false
		}
		id := event.TaskID
		if id == "" {
			id = payload.ID
		}
		if id != taskID {
			return durableSessionBinding{}, false
		}
		var binding BackendBinding
		switch {
		case payload.BackendBinding != nil:
			binding = *payload.BackendBinding
		case payload.ProviderBinding != nil:
			binding = BackendBinding{Backend: execution.CanonicalBackendName(payload.ProviderBinding.Provider), SessionID: payload.ProviderBinding.SessionID}
		}
		if binding.SessionID == "" {
			return durableSessionBinding{}, false
		}
		return durableSessionBinding{EventID: event.ID, Binding: binding}, true
	default:
		return durableSessionBinding{}, false
	}
}

// durableSessionBindingFromEvent decodes a canonical or legacy session-bound
// event. Malformed payloads and bindings without a session are skipped, as
// the replay reducer skips them.
func durableSessionBindingFromEvent(event RunEvent) (durableSessionBinding, bool) {
	var binding BackendBinding
	switch event.Type {
	case string(EventBackendSessionBound):
		var payload BackendSessionBoundPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return durableSessionBinding{}, false
		}
		binding = BackendBinding{Backend: payload.Backend, SessionID: payload.SessionID, ExecutionWorldID: payload.ExecutionWorldID, CWD: payload.CWD}
	case string(EventProviderSessionBound):
		var payload ProviderSessionBoundPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return durableSessionBinding{}, false
		}
		binding = BackendBinding{Backend: execution.CanonicalBackendName(payload.Provider), SessionID: payload.SessionID, ExecutionWorldID: payload.ExecutionWorldID, CWD: payload.CWD}
	default:
		return durableSessionBinding{}, false
	}
	if binding.SessionID == "" {
		return durableSessionBinding{}, false
	}
	return durableSessionBinding{EventID: event.ID, Binding: binding}, true
}

// backendSessionConflict rejects a binding whose backend session differs
// from the durable one.
func backendSessionConflict(taskID string, durable *durableSessionBinding, backend, sessionID string) error {
	if durable == nil {
		return nil
	}
	if execution.BackendNamesEqual(durable.Binding.Backend, backend) && durable.Binding.SessionID == sessionID {
		return nil
	}
	return &ExecutionIdentityConflictError{
		TaskID: taskID, EventID: durable.EventID,
		Reason: fmt.Sprintf("task is bound to backend session %s/%s; refusing a second binding to %s/%s", durable.Binding.Backend, durable.Binding.SessionID, backend, sessionID),
	}
}
