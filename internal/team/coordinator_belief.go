package team

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/kjelly/hufu/internal/belief"
)

type coordinatorBeliefs struct {
	store       *EventStore
	branchID    string
	journal     *BeliefJournal
	views       []BeliefRunState
	needsRepair bool
}

// BeliefStates returns a defensive copy of the replayable state component.
// Execution status, verification and authorization retain their existing owners.
func (c *Coordinator) BeliefStates() []BeliefRunState {
	c.beliefMu.Lock()
	defer c.beliefMu.Unlock()
	if c.beliefRuntime == nil {
		return nil
	}
	result := make([]BeliefRunState, len(c.beliefRuntime.views))
	for i, view := range c.beliefRuntime.views {
		result[i] = cloneBeliefRunState(view)
	}
	return result
}

// refreshBeliefs is synchronous bookkeeping after policy admission or durable
// lifecycle append. It never returns an execution error or reruns source work.
func (c *Coordinator) refreshBeliefs(source *RunEvent) {
	if c == nil || c.session == nil || c.session.Belief == nil || c.eventStore == nil || c.modelFanout != nil {
		return
	}
	c.beliefMu.Lock()
	defer c.beliefMu.Unlock()
	ctx := context.Background()
	branchID := c.eventStore.BranchID()
	if source == nil && c.beliefRuntime != nil && c.beliefRuntime.store == c.eventStore && c.beliefRuntime.branchID == branchID && !c.beliefRuntime.needsRepair {
		return
	}
	if c.beliefRuntime == nil || c.beliefRuntime.store != c.eventStore || c.beliefRuntime.branchID != branchID {
		tree, err := LoadSessionTree(c.session.Workspace)
		if err != nil {
			log.Print("warning: belief projection unavailable (lineage unavailable)")
			return
		}
		journal, err := NewBeliefJournal(c.EventJournal(), c.decisionArtifactStore(), tree, branchID, c.eventStore.runID, c.eventStore.sessionID)
		if err != nil {
			log.Print("warning: belief projection unavailable (journal unavailable)")
			return
		}
		c.beliefRuntime = &coordinatorBeliefs{store: c.eventStore, branchID: branchID, journal: journal}
		// A fresh owner repairs all visible historical source/admission gaps.
		source = nil
	}
	runtime := c.beliefRuntime
	// Follow the canonical journal service if the host replaces its adapter
	// (for example to inject a durability fault); never bypass that boundary.
	runtime.journal.journal = c.EventJournal()
	runtime.needsRepair = false
	events, err := runtime.journal.events(ctx)
	if err != nil {
		runtime.unavailable(ctx, "", "", "replay_unavailable")
		return
	}
	if err := runtime.bindCurrent(ctx, c.session.Belief, events); err != nil {
		runtime.unavailable(ctx, "", "", "model_binding_unavailable")
	}
	events, err = runtime.journal.events(ctx)
	if err != nil {
		return
	}
	bindings, err := runtimeBeliefBindings(events)
	if err != nil {
		runtime.unavailable(ctx, "", "", "replay_unavailable")
		return
	}
	for _, binding := range bindings {
		if source != nil && source.RunID != binding.event.RunID {
			continue
		}
		writer, writerErr := NewBeliefJournal(runtime.journal.journal, runtime.journal.artifacts, runtime.journal.tree, branchID, binding.event.RunID, binding.event.SessionID)
		if writerErr != nil {
			continue
		}
		model, reason := loadBoundBeliefModel(ctx, writer.artifacts, *binding.payload.ModelRef, binding.payload.ModelDigest, binding.payload.ReducerVersion)
		if reason != "" || validateBeliefVerificationExperimentValue(model, binding.payload.ExperimentID) != nil {
			runtime.diagnostic(ctx, writer, "", "", "model_binding_unavailable")
			continue
		}
		candidates := events
		if source != nil {
			candidates = []RunEvent{*source}
		}
		for _, event := range candidates {
			if event.RunID != binding.event.RunID {
				continue
			}
			if err := admitBeliefSource(ctx, writer, *model, binding.payload.ExperimentID, event, events); err != nil {
				runtime.diagnostic(ctx, writer, event.TaskID, event.ID, "source_binding_unavailable")
			}
		}
	}
	// The cache is expendable. Every publication is reconstructed from the
	// durable lineage, including admissions repaired above.
	runtime.rebuild(ctx)
	runtime.suggestPolicies(ctx)
}

func validateBeliefVerificationExperimentValue(model *belief.Model, experimentID string) error {
	if model == nil {
		return fmt.Errorf("belief model unavailable")
	}
	return validateBeliefVerificationExperiment(*model, experimentID)
}

type runtimeBeliefBinding struct {
	event   RunEvent
	payload beliefEvent
}

func runtimeBeliefBindings(events []RunEvent) ([]runtimeBeliefBinding, error) {
	var bindings []runtimeBeliefBinding
	seen := map[string]beliefEvent{}
	for _, event := range events {
		if event.Type != EventBeliefModelBound {
			continue
		}
		var payload beliefEvent
		if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil || payload.SchemaVersion != 1 || event.Actor != beliefActor || payload.ModelRef == nil {
			return nil, fmt.Errorf("invalid runtime belief binding")
		}
		if payload.ExperimentID == "" { // Pure journal clients do not opt into runtime admission.
			continue
		}
		if prior, ok := seen[event.RunID]; ok {
			if !sameRuntimeBeliefBinding(prior, payload) {
				return nil, fmt.Errorf("conflicting runtime belief adapter")
			}
			continue
		}
		seen[event.RunID] = payload
		bindings = append(bindings, runtimeBeliefBinding{event, payload})
	}
	return bindings, nil
}

func (r *coordinatorBeliefs) bindCurrent(ctx context.Context, compiled *CompiledBelief, events []RunEvent) error {
	hasSource := false
	for _, event := range events {
		if event.RunID != r.journal.runID {
			continue
		}
		if event.Type == EventBeliefModelBound {
			var payload beliefEvent
			if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.ModelDigest != compiled.snapshot.ModelDigest || payload.ExperimentID != compiled.snapshot.ExperimentID || payload.PolicyDigest != compiled.snapshot.PolicyDigest {
				return fmt.Errorf("belief runtime binding mismatch")
			}
			return nil
		}
		if isBeliefVerificationSource(event.Type) {
			// An old source without a historical binding cannot be reconstructed
			// by quietly using today's team file.
			hasSource = true
		}
	}
	if hasSource {
		return fmt.Errorf("belief historical model binding missing")
	}
	return r.journal.BindRuntimeModel(ctx, compiled.model, compiled.snapshot.ExperimentID, compiled.policy)
}

func sameRuntimeBeliefBinding(a, b beliefEvent) bool {
	if a.ModelDigest != b.ModelDigest || a.ReducerVersion != b.ReducerVersion || a.ExperimentID != b.ExperimentID || a.PolicyDigest != b.PolicyDigest || a.PolicyVersion != b.PolicyVersion {
		return false
	}
	if a.ModelRef == nil || b.ModelRef == nil || a.ModelRef.Kind != b.ModelRef.Kind || !sameArtifactIdentity(*a.ModelRef, *b.ModelRef) {
		return false
	}
	if a.PolicyRef == nil || b.PolicyRef == nil {
		return a.PolicyRef == nil && b.PolicyRef == nil
	}
	return a.PolicyRef.Kind == b.PolicyRef.Kind && sameArtifactIdentity(*a.PolicyRef, *b.PolicyRef)
}

func isBeliefVerificationSource(kind string) bool {
	return slices.Contains([]string{"task_completed", "task_failed", "task_blocked", "task_protocol_incomplete"}, kind)
}

func admitBeliefSource(ctx context.Context, writer *BeliefJournal, model belief.Model, experimentID string, event RunEvent, lineage []RunEvent) error {
	if !isBeliefVerificationSource(event.Type) {
		return nil
	}
	receipts, err := beliefSourceReceipts(event)
	if err != nil {
		return err
	}
	if len(receipts) == 0 {
		var claims struct {
			Verification *struct{} `json:"verify_result"`
		}
		if err := json.Unmarshal(event.Payload, &claims); err != nil || claims.Verification != nil {
			return fmt.Errorf("belief verifier result has no source receipt")
		}
	}
	var admissionErrors error
	for _, receipt := range receipts {
		if receipt.VerifyResult == nil || receipt.RunID != event.RunID {
			continue
		}
		key := beliefReceiptKey(receipt)
		if inheritedBeliefAdmission(lineage, key, writer.branchID) {
			continue
		}
		admissionErrors = errors.Join(admissionErrors, admitBeliefReceipt(ctx, writer, model, experimentID, event, receipt))
	}
	return admissionErrors
}

func admitBeliefReceipt(ctx context.Context, writer *BeliefJournal, model belief.Model, experimentID string, event RunEvent, receipt ExecutionReceipt) error {
	scope := belief.Scope{RunID: event.RunID, SessionID: writer.sessionID, BranchID: writer.branchID, TaskID: event.TaskID, Attempt: cmp.Or(receipt.OccurrenceAttempt, receipt.Attempt)}
	epoch, err := BeliefVerificationEpoch(model, scope, event, beliefReceiptKey(receipt))
	if err != nil {
		return err
	}
	if err := writer.OpenEpoch(ctx, epoch); err != nil {
		return err
	}
	if err := writer.AdmitVerification(ctx, epoch.ID, event.ID, experimentID, beliefReceiptKey(receipt)); err != nil {
		return err
	}
	// The scope is one completed receipt. Later receipts open new epochs.
	return writer.CloseEpoch(ctx, epoch.ID, event.ID)
}

func inheritedBeliefAdmission(events []RunEvent, receiptKey, branchID string) bool {
	for _, event := range events {
		if event.Type != EventBeliefObservationAdmitted || effectiveBranchID(event.BranchID) == branchID {
			continue
		}
		var payload beliefEvent
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Source != nil && payload.Source.ReceiptKey == receiptKey {
			return true
		}
	}
	return false
}

func (r *coordinatorBeliefs) diagnostic(ctx context.Context, writer *BeliefJournal, taskID, sourceID, code string) {
	r.needsRepair = true
	log.Printf("warning: belief projection unavailable (%s)", code)
	payload, err := json.Marshal(BeliefDiagnostic{SchemaVersion: 1, Code: code, SourceEventID: sourceID, TaskID: taskID})
	if err != nil {
		return
	}
	_, _ = writer.journal.Append(ctx, RunEvent{Type: EventBeliefUnavailable, Actor: beliefActor, RunID: writer.runID, SessionID: writer.sessionID, BranchID: writer.branchID, TaskID: taskID, IdempotencyKey: "belief-diagnostic:" + beliefIdentity("diagnostic", []string{writer.runID, taskID, sourceID, code}), Payload: payload})
}

func (r *coordinatorBeliefs) unavailable(ctx context.Context, taskID, sourceID, code string) {
	r.diagnostic(ctx, r.journal, taskID, sourceID, code)
	r.views = nil // Do not continue presenting a previously valid cache as current.
}

func (r *coordinatorBeliefs) rebuild(ctx context.Context) {
	events, err := r.journal.events(ctx)
	if err != nil {
		r.unavailable(ctx, "", "", "replay_unavailable")
		return
	}
	runs := map[string]bool{}
	for _, event := range events {
		if isBeliefEvent(event.Type) || event.Type == EventBeliefUnavailable {
			runs[event.RunID] = true
		}
	}
	views := make([]BeliefRunState, 0, len(runs))
	for runID := range runs {
		view, err := ProjectBeliefRun(ctx, events, r.journal.artifacts, runID)
		if err != nil {
			r.unavailable(ctx, "", "", "replay_unavailable")
			return
		}
		views = append(views, view)
	}
	slices.SortFunc(views, func(a, b BeliefRunState) int { return cmp.Compare(a.RunID, b.RunID) })
	r.views = views
}
