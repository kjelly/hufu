package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// PreparedHistoricalResultRequest is a single-use operator intake. It is not
// durable authority: the cycle and its grant must commit together before work.
type PreparedHistoricalResultRequest struct {
	owner         *Coordinator
	prompt        string
	bindings      []string
	prepared      *PreparedHistoricalResultGrants
	previousCycle string
	derivedFork   *DerivedForkRef
	state         atomic.Uint32 // 0=prepared, 1=claimed by Run, 2=consumed
}

type historicalResultRequestKey struct{}

// WithHistoricalResultRequest supplies a prepared intake to Run only. Model
// tools cannot create it, and a later invocation cannot reuse the intake.
func WithHistoricalResultRequest(ctx context.Context, request *PreparedHistoricalResultRequest) context.Context {
	return context.WithValue(ctx, historicalResultRequestKey{}, request)
}

// PrepareHistoricalResultRequest is read-only and must precede startup model
// consumers. The current implementation admits normal completed follow-ups;
// unfinished workflows retain their existing resume/repair boundaries.
func (c *Coordinator) PrepareHistoricalResultRequest(ctx context.Context, prompt string, bindings []string) (*PreparedHistoricalResultRequest, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("historical result intake requires a new request")
	}
	lineage, err := c.historicalRequestLineage(ctx)
	if err != nil {
		return nil, err
	}
	policy, err := executionPolicySnapshotFromEvents(lineage)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, fmt.Errorf("historical result intake requires a canonical admitted policy")
	}
	canonical, err := c.historicalRequestPredecessor(lineage)
	if err != nil {
		return nil, err
	}
	prepared, err := c.PrepareHistoricalResultGrants(ctx, bindings)
	if err != nil {
		return nil, err
	}
	return &PreparedHistoricalResultRequest{owner: c, prompt: strings.TrimSpace(prompt), bindings: slices.Clone(bindings),
		prepared: prepared, previousCycle: canonical.WorkflowCycleID, derivedFork: cloneDerivedForkRef(canonical.DerivedFork)}, nil
}

func (c *Coordinator) historicalRequestLineage(ctx context.Context) ([]RunEvent, error) {
	scope, err := c.historicalResultGrantScope(ctx)
	if err != nil {
		return nil, err
	}
	store := c.EventStore()
	if store != nil && store.BranchID() != scope.Branch {
		return nil, ErrHistoricalResultGrantDrift
	}
	if store == nil {
		store, err = OpenEventStoreReadOnly(scope.Workspace)
		if err != nil {
			return nil, err
		}
		defer func() { _ = store.Close() }()
	} else if err := store.VerifyHashChain(); err != nil {
		return nil, err
	}
	events, err := store.ReadEvents()
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTreeContext(ctx, scope.Workspace)
	if err != nil {
		return nil, err
	}
	if tree.ActiveBranch != scope.Branch {
		return nil, ErrHistoricalResultGrantDrift
	}
	return ProjectValidatedEventsForBranch(events, tree, scope.Branch)
}

func (c *Coordinator) historicalRequestPredecessor(lineage []RunEvent) (*SessionData, error) {
	if err := validateWorkflowCycleLineage(lineage); err != nil {
		return nil, err
	}
	canonical := ReduceToSessionData(lineage)
	derived := canonical.DerivedFork != nil && canonical.DerivedFork.AwaitingNewRequest
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || (!derived && (c.phaseWorkflow.State() != PhaseDone || canonical.WorkflowState != PhaseDone)) {
		return nil, fmt.Errorf("new historical result grants require a completed workflow; resume or repair unfinished work without new grants")
	}
	current := c.SessionData()
	if current == nil || current.RecoveryRequired || current.PendingTerminalCommit != nil || canonical.RecoveryRequired {
		return nil, fmt.Errorf("historical result intake requires settled canonical recovery state")
	}
	if current.WorkflowCycleID != canonical.WorkflowCycleID {
		return nil, ErrHistoricalResultGrantDrift
	}
	if derived && (current.DerivedFork == nil || *current.DerivedFork != *canonical.DerivedFork || canonical.WorkflowState != PhaseInit) {
		return nil, ErrDerivedForkIntent
	}
	for _, item := range canonical.Tasks {
		if item == nil || isInterruptedStatus(item.Status) {
			return nil, fmt.Errorf("historical result intake cannot discard unfinished work")
		}
	}
	return canonical, nil
}

func pendingHistoricalResultRequest(ctx context.Context) *PreparedHistoricalResultRequest {
	request, _ := ctx.Value(historicalResultRequestKey{}).(*PreparedHistoricalResultRequest)
	if request == nil || request.state.Load() == 2 {
		return nil
	}
	return request
}

func (c *Coordinator) claimHistoricalResultRequest(ctx context.Context, prompt string) (*PreparedHistoricalResultRequest, error) {
	request, _ := ctx.Value(historicalResultRequestKey{}).(*PreparedHistoricalResultRequest)
	if request == nil {
		return nil, nil
	}
	if resume, _ := ctx.Value(sessionResumeKey{}).(bool); resume || request.owner != c || request.prompt != strings.TrimSpace(prompt) ||
		!request.state.CompareAndSwap(0, 1) {
		return nil, fmt.Errorf("historical result intake is not a fresh request for this coordinator")
	}
	fresh, err := c.PrepareHistoricalResultRequest(ctx, prompt, request.bindings)
	if err != nil || !sameHistoricalResultRequest(request, fresh) {
		request.state.Store(2)
		if err != nil {
			return nil, err
		}
		return nil, ErrHistoricalResultGrantDrift
	}
	return request, nil
}

func sameHistoricalResultRequest(a, b *PreparedHistoricalResultRequest) bool {
	return a != nil && b != nil && a.previousCycle == b.previousCycle && a.prepared.scope == b.prepared.scope &&
		sameDerivedForkLineage(a.derivedFork, b.derivedFork) && a.prepared.policyFingerprint == b.prepared.policyFingerprint && slices.Equal(a.prepared.grants, b.prepared.grants)
}

// ValidateHistoricalResultStartupSources restores an unfinished cycle before
// CLI startup model consumers. Completed follow-ups clear old grants in Run.
func (c *Coordinator) ValidateHistoricalResultStartupSources(ctx context.Context) error {
	if c.phaseWorkflow != nil && c.phaseWorkflow.State() == PhaseDone {
		return nil
	}
	return c.ValidateHistoricalResultCycleSources(ctx)
}

// ValidateHistoricalResultCycleSources is the pre-model recovery boundary.
// The checkpoint is only a trigger; the canonical event/index supply authority.
func (c *Coordinator) ValidateHistoricalResultCycleSources(ctx context.Context) error {
	var checkpoint *runtimestore.HistoricalGrantCycleRef
	c.viewSessionData(func(sd *SessionData) { checkpoint = cloneHistoricalGrantCycleRef(sd.HistoricalResultGrants) })
	if checkpoint == nil {
		return nil
	}
	payload, ref, _, err := c.historicalResultCycleSnapshot(ctx)
	if err != nil {
		return err
	}
	if payload == nil || ref == nil || *ref != *checkpoint {
		return ErrHistoricalResultTaskBinding
	}
	bindings := make([]string, len(payload.Grants))
	for i, grant := range payload.Grants {
		bindings[i] = grant.Recipient + "=" + grant.ResultID
	}
	prepared, err := c.PrepareHistoricalResultGrants(ctx, bindings)
	if err != nil {
		return err
	}
	if prepared.scope.Workspace != payload.Workspace || prepared.scope.Team != payload.Team ||
		prepared.policyFingerprint != payload.PolicyFingerprint || !slices.Equal(prepared.grants, payload.Grants) {
		return ErrHistoricalResultGrantDrift
	}
	return nil
}

// appendHistoricalCycle captures the head before rereading any source. The
// conditional transaction commits both events and the immutable grant index,
// or neither. A competing writer is an admission error, never stale authority.
func (c *Coordinator) appendHistoricalCycle(ctx context.Context, boundary RunEvent, payload WorkflowCycleStartedPayload) (RunEvent, *runtimestore.HistoricalGrantCycleRef, error) {
	request := pendingHistoricalResultRequest(ctx)
	if request == nil || request.owner != c || request.state.Load() != 1 || request.previousCycle != payload.PreviousCycleID {
		return RunEvent{}, nil, fmt.Errorf("historical cycle has no claimed operator intake")
	}
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok {
		return RunEvent{}, nil, fmt.Errorf("historical result intake requires an atomic event journal")
	}
	if err := journal.VerifyHashChain(ctx); err != nil {
		return RunEvent{}, nil, err
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return RunEvent{}, nil, err
	}
	head := ""
	if len(events) > 0 {
		head = events[len(events)-1].ID
	}
	fresh, err := c.PrepareHistoricalResultRequest(ctx, request.prompt, request.bindings)
	if err != nil {
		return RunEvent{}, nil, err
	}
	if !sameHistoricalResultRequest(request, fresh) {
		return RunEvent{}, nil, ErrHistoricalResultGrantDrift
	}
	store := c.EventStore()
	if store == nil {
		return RunEvent{}, nil, fmt.Errorf("historical result intake requires the canonical event store")
	}
	if store.BranchID() != fresh.prepared.scope.Branch {
		return RunEvent{}, nil, ErrHistoricalResultGrantDrift
	}
	envelope, err := fresh.prepared.envelope(store.sessionID, boundary.RunID)
	if err != nil {
		return RunEvent{}, nil, err
	}
	data, err := json.Marshal(envelope, json.Deterministic(true))
	if err != nil {
		return RunEvent{}, nil, err
	}
	boundary.BranchID, boundary.SessionID = envelope.BranchID, envelope.SessionID
	grant := RunEvent{Type: string(EventHistoricalResultsGranted), Actor: "operator", RunID: boundary.RunID,
		SessionID: envelope.SessionID, BranchID: envelope.BranchID, Payload: data, IdempotencyKey: "historical-grants:" + boundary.RunID}
	durable, err := journal.AppendBatchAtHead(ctx, []RunEvent{boundary, grant}, head)
	if err != nil {
		return RunEvent{}, nil, err
	}
	if len(durable) != 2 {
		return RunEvent{}, nil, fmt.Errorf("historical cycle journal returned an incomplete batch")
	}
	ref := &runtimestore.HistoricalGrantCycleRef{BranchID: durable[1].BranchID, CycleID: durable[1].RunID,
		EventID: durable[1].ID, EventHash: durable[1].Hash}
	return durable[0], ref, nil
}
