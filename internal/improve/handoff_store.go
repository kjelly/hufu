package improve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

type HandoffAuditFunc func(context.Context, team.RunEvent) error

type HandoffStore struct {
	workspace string
	audit     HandoffAuditFunc
}

var handoffProcessLocks sync.Map

func NewHandoffStore(workspace string) *HandoffStore {
	return &HandoffStore{
		workspace: workspace,
		audit: func(ctx context.Context, event team.RunEvent) error {
			return appendHandoffAudit(context.WithValue(ctx, handoffWorkspaceKey{}, workspace), event)
		},
	}
}

func (s *HandoffStore) Create(ctx context.Context, handoff ImprovementHandoff) (ImprovementHandoff, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var result ImprovementHandoff
	err := s.withExclusiveLock(ctx, handoff.ID, func() error {
		var err error
		result, err = s.createUnlocked(ctx, handoff)
		return err
	})
	return result, err
}

func (s *HandoffStore) createUnlocked(ctx context.Context, handoff ImprovementHandoff) (ImprovementHandoff, error) {
	if err := handoff.Validate(); err != nil {
		return ImprovementHandoff{}, err
	}
	if handoff.Status != HandoffProposed || handoff.Revision != 1 {
		return ImprovementHandoff{}, errors.New("new handoff must start at proposed revision 1")
	}
	path := s.path(handoff.ID)
	data, err := marshalHandoff(handoff)
	if err != nil {
		return ImprovementHandoff{}, err
	}
	if err := team.AtomicCreateFile(path, data, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			current, getErr := s.getUnlocked(handoff.ID)
			if getErr == nil && sameHandoffIdentity(current, handoff) {
				if auditErr := s.audit(ctx, handoffEvent("handoff_created", handoff, handoff.Revision)); auditErr != nil {
					return current, fmt.Errorf("audit idempotent handoff creation: %w", auditErr)
				}
				return current, nil
			}
		}
		return ImprovementHandoff{}, fmt.Errorf("create handoff: %w", err)
	}
	if err := s.audit(ctx, handoffEvent("handoff_created", handoff, handoff.Revision)); err != nil {
		return handoff, fmt.Errorf("audit handoff creation: %w", err)
	}
	return handoff, nil
}

func (s *HandoffStore) Get(id string) (ImprovementHandoff, error) {
	if err := validateArtifactID(id); err != nil {
		return ImprovementHandoff{}, err
	}
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return ImprovementHandoff{}, err
	}
	var handoff ImprovementHandoff
	if err := json.Unmarshal(data, &handoff); err != nil {
		return ImprovementHandoff{}, fmt.Errorf("parse handoff: %w", err)
	}
	if handoff.ID != id {
		return ImprovementHandoff{}, fmt.Errorf("handoff id mismatch: path=%q record=%q", id, handoff.ID)
	}
	if err := handoff.Validate(); err != nil {
		return ImprovementHandoff{}, fmt.Errorf("validate handoff: %w", err)
	}
	return handoff, nil
}

func (s *HandoffStore) Transition(ctx context.Context, id string, expectedRevision int64, next ImprovementHandoff) (ImprovementHandoff, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var result ImprovementHandoff
	err := s.withExclusiveLock(ctx, id, func() error {
		var err error
		result, err = s.transitionUnlocked(ctx, id, expectedRevision, next)
		return err
	})
	return result, err
}

func (s *HandoffStore) transitionUnlocked(ctx context.Context, id string, expectedRevision int64, next ImprovementHandoff) (ImprovementHandoff, error) {
	current, err := s.getUnlocked(id)
	if err != nil {
		return ImprovementHandoff{}, err
	}
	if current.Revision != expectedRevision {
		return current, fmt.Errorf("handoff %q revision conflict: expected %d, got %d", id, expectedRevision, current.Revision)
	}
	if !sameHandoffIdentity(next, current) {
		return current, errors.New("handoff immutable bindings cannot change")
	}
	if err := validateBoundHandoffRefs(current, next); err != nil {
		return current, err
	}
	if !validHandoffTransition(current.Status, next.Status) {
		return current, fmt.Errorf("invalid handoff transition %s -> %s", current.Status, next.Status)
	}
	next.Revision = current.Revision + 1
	next.CreatedAt = current.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	if err := next.Validate(); err != nil {
		return current, err
	}
	data, err := marshalHandoff(next)
	if err != nil {
		return current, err
	}
	if err := team.AtomicWriteFile(s.path(id), data, 0o600); err != nil {
		return current, fmt.Errorf("update handoff: %w", err)
	}
	if err := s.audit(ctx, handoffEvent(handoffAuditType(next.Status), next, expectedRevision)); err != nil {
		return next, fmt.Errorf("audit handoff transition: %w", err)
	}
	return next, nil
}

// RetryAudit reconciles the audit event for the current durable state without
// changing the handoff. The durable record, rather than caller-supplied state,
// is authoritative. EventStore idempotency makes repeated calls safe.
func (s *HandoffStore) RetryAudit(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.withExclusiveLock(ctx, id, func() error {
		handoff, err := s.getUnlocked(id)
		if err != nil {
			return err
		}
		return s.retryAuditUnlocked(ctx, handoff)
	})
}

func (s *HandoffStore) retryAuditUnlocked(ctx context.Context, handoff ImprovementHandoff) error {
	inputRevision := handoff.Revision - 1
	eventType := handoffAuditType(handoff.Status)
	if handoff.Status == HandoffProposed && handoff.Revision == 1 {
		inputRevision = 1
		eventType = "handoff_created"
	}
	if inputRevision < 1 {
		return fmt.Errorf("handoff %q has invalid audit revision", handoff.ID)
	}
	return s.audit(ctx, handoffEvent(eventType, handoff, inputRevision))
}

func handoffAuditType(status HandoffStatus) string {
	switch status {
	case HandoffCandidateReady, HandoffBenchmarkBound:
		return "handoff_prepared"
	case HandoffEvaluated, HandoffEligibleForReview:
		return "handoff_evaluated"
	case HandoffApproved:
		return "handoff_approved"
	case HandoffRejected:
		return "handoff_rejected"
	case HandoffAdopted:
		return "handoff_adopted"
	case HandoffMonitoring, HandoffRollbackRecommended:
		return "handoff_monitoring"
	case HandoffStale:
		return "handoff_stale"
	default:
		return "handoff_updated"
	}
}

func (s *HandoffStore) getUnlocked(id string) (ImprovementHandoff, error) {
	data, err := os.ReadFile(s.path(id))
	if err != nil {
		return ImprovementHandoff{}, err
	}
	var handoff ImprovementHandoff
	if err := json.Unmarshal(data, &handoff); err != nil {
		return ImprovementHandoff{}, fmt.Errorf("parse handoff: %w", err)
	}
	if handoff.ID != id {
		return ImprovementHandoff{}, fmt.Errorf("handoff id mismatch: path=%q record=%q", id, handoff.ID)
	}
	if err := handoff.Validate(); err != nil {
		return ImprovementHandoff{}, fmt.Errorf("validate handoff: %w", err)
	}
	return handoff, nil
}

func (s *HandoffStore) path(id string) string {
	return filepath.Join(ImprovementRoot(s.workspace), "handoffs", id, "handoff.json")
}

func marshalHandoff(handoff ImprovementHandoff) ([]byte, error) {
	data, err := json.MarshalIndent(handoff, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal handoff: %w", err)
	}
	return append(data, '\n'), nil
}

func sameSources(a, b []SourceBinding) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func sameHandoffIdentity(a, b ImprovementHandoff) bool {
	return a.ID == b.ID && a.Version == b.Version && a.Kind == b.Kind && a.Scope == b.Scope && a.Proposal == b.Proposal && sameSources(a.Sources, b.Sources)
}

func (s *HandoffStore) withExclusiveLock(ctx context.Context, id string, fn func() error) error {
	if err := validateArtifactID(id); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lockPath := filepath.Join(ImprovementRoot(s.workspace), "handoffs", ".locks", id+".lock")
	lock, _ := handoffProcessLocks.LoadOrStore(lockPath, &sync.Mutex{})
	processLock := lock.(*sync.Mutex)
	processLock.Lock()
	defer processLock.Unlock()
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create handoff lock directory: %w", err)
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open handoff lock: %w", err)
	}
	if err := lockHandoffFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("acquire handoff lock: %w", err)
	}
	operationErr := fn()
	unlockErr := unlockHandoffFile(file)
	closeErr := file.Close()
	return errors.Join(operationErr, unlockErr, closeErr)
}

func handoffEvent(eventType string, handoff ImprovementHandoff, inputRevision int64) team.RunEvent {
	payload, _ := json.Marshal(map[string]any{
		"schema_version": HandoffSchemaVersion,
		"handoff_id":     handoff.ID,
		"kind":           handoff.Kind,
		"status":         handoff.Status,
		"revision":       handoff.Revision,
	})
	return team.RunEvent{
		Type: eventType, Actor: "improve", IdempotencyKey: fmt.Sprintf("handoff:%s:%s:%d", handoff.ID, eventType, inputRevision), Payload: payload,
	}
}

func appendHandoffAudit(ctx context.Context, event team.RunEvent) error {
	workspace := ""
	if value, ok := ctx.Value(handoffWorkspaceKey{}).(string); ok {
		workspace = value
	}
	if workspace == "" {
		return errors.New("handoff audit workspace is missing")
	}
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	_, err = store.AppendPersistedContext(ctx, event)
	return err
}

type handoffWorkspaceKey struct{}
