package improver

import (
	"bytes"
	"context"
	"crypto/rand"
	json "encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/providerproxy"
	"github.com/kjelly/hufu/internal/scripted"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
)

// StageEvidence binds an imported child to the exact durable admission. The
// completion marker is published only after all child resources have closed.
type StageEvidence struct {
	SchemaVersion int           `json:"schema_version"`
	Request       StageRequest  `json:"request"`
	Child         ChildEvidence `json:"child"`
	StopReason    string        `json:"stop_reason"`
	Script        *ScriptFacts  `json:"script,omitzero"`
}

type ScriptFacts struct {
	Calls      int      `json:"calls"`
	UsageKnown bool     `json:"usage_known"`
	Tokens     int64    `json:"tokens"`
	Issues     []string `json:"issues"`
}

// ProbeExecutor owns no parent journal or provider cursor. Each Execute opens
// a fresh runtime; Reconcile only reads a post-drain marker and imported bytes.
type ProbeExecutor struct {
	factory  *RuntimeFactory
	plan     paired.ExperimentPlan
	fixture  paired.Fixture
	program  *paired.Program
	inputs   Tree
	objects  Objects
	children string
}

func NewProbeExecutor(prepared Prepared, plan paired.ExperimentPlan, objects Objects, children string, settings RuntimeSettings) (*ProbeExecutor, error) {
	if objects == nil {
		return nil, errors.New("probe requires an object store")
	}
	if err := validateCommittedPlan(plan); err != nil {
		return nil, err
	}
	factory, err := NewRuntimeFactory(prepared, settings)
	if err != nil {
		return nil, err
	}
	models, err := factory.Preflight()
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(models, plan.Models) || prepared.Preview.Build.BinaryDigest != plan.BinaryDigest ||
		prepared.Config.ExecutionMode != plan.ExecutionMode || prepared.Config.Mode != plan.Mode ||
		prepared.Config.Limits != plan.Limits {
		return nil, errors.New("probe runtime differs from committed plan")
	}
	frozenPlan, err := cloneRuntimeValue(plan)
	if err != nil {
		return nil, err
	}
	fixture, err := cloneRuntimeValue(prepared.Fixture)
	if err != nil {
		return nil, err
	}
	program, err := cloneRuntimeValue(prepared.Program)
	if err != nil {
		return nil, err
	}
	inputs := Tree{files: make(map[string][]byte, len(prepared.Inputs.files))}
	for name, raw := range prepared.Inputs.files {
		inputs.files[name] = bytes.Clone(raw)
	}
	// The owner creates the private children directory under its workspace.
	root, err := openSourceRoot(children)
	if err != nil {
		return nil, err
	}
	info, statErr := root.Stat(".")
	if err := errors.Join(statErr, root.Close()); err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o700 {
		return nil, errors.New("probe child area must be private")
	}
	return &ProbeExecutor{factory, frozenPlan, fixture, program, inputs, objects, children}, nil
}

func (e *ProbeExecutor) checkRequest(request StageRequest) (paired.Case, string, error) {
	if request.Generation < 1 || request.Generation > e.plan.Limits.StageRetries+1 ||
		!opaqueID(request.Owner) || !paired.SafeRelativePath(request.ChildWorkspaceID) ||
		filepath.Base(request.ChildWorkspaceID) != request.ChildWorkspaceID ||
		request.Deadline.IsZero() || request.Limits != e.plan.Limits ||
		request.Spec.Kind != "executor" || request.StageID != request.Spec.ID {
		return paired.Case{}, "", errors.New("invalid probe admission")
	}
	stages, _, err := plannedStages(e.plan)
	if err != nil {
		return paired.Case{}, "", err
	}
	found := false
	for _, spec := range stages {
		found = found || spec == request.Spec
	}
	if !found {
		return paired.Case{}, "", errors.New("probe stage is outside the frozen plan")
	}
	for _, item := range e.fixture.Cases {
		if item.ID == request.Spec.Slot.Case {
			raw, ok := e.inputs.Read("cases/" + item.ID + ".txt")
			if !ok {
				return paired.Case{}, "", errors.New("frozen case input is missing")
			}
			return item, string(raw), nil
		}
	}
	return paired.Case{}, "", errors.New("probe case is outside the frozen fixture")
}

func (e *ProbeExecutor) script(request StageRequest) (*scripted.Provider, error) {
	if e.plan.ExecutionMode != "fixture" {
		return nil, nil
	}
	if e.program == nil {
		return nil, errors.New("fixture program is missing")
	}
	for _, script := range e.program.Scripts {
		if script.Stage == "executor" && script.CaseID == request.Spec.Slot.Case &&
			script.VariantSlotID == request.Spec.Slot.Variant && script.Attempt == request.Generation {
			return scripted.NewObservedProvider("scripted", script.Steps, func(ctx context.Context, usage *scripted.Usage) {
				var observed *agent.ProviderInvocationUsage
				if usage != nil {
					observed = &agent.ProviderInvocationUsage{InputTokens: usage.InputTokens,
						OutputTokens: usage.OutputTokens, TotalTokens: usage.InputTokens + usage.OutputTokens}
				}
				agent.ObserveProviderResponseUsage(ctx, observed)
			}), nil
		}
	}
	return nil, errors.New("fixture program has no script for the admitted stage")
}

func (e *ProbeExecutor) Execute(ctx context.Context, request StageRequest, beforeProvider func(paired.RuntimeIdentity) error) (result Settlement, retErr error) {
	item, input, err := e.checkRequest(request)
	if err != nil {
		return result, err
	}
	if beforeProvider == nil {
		return result, errors.New("durable binding callback is required")
	}
	ctx, cancel := context.WithDeadline(ctx, request.Deadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	provider, err := e.script(request)
	if err != nil {
		return result, err
	}
	var boundary agent.ProviderBoundaryFactory
	if provider != nil {
		boundary = func(ctx context.Context, _ string, _ providerproxy.Config) (providerproxy.Boundary, error) {
			return scripted.NewBoundary(ctx, provider)
		}
	}
	root := filepath.Join(e.children, request.ChildWorkspaceID)
	runtime, err := e.factory.open(root, request.Spec.Slot.Variant, "executor", boundary)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, runtime.close()) }()
	policy, err := runtimePolicyDigest(runtime.policy, runtime.overlay)
	if err != nil || policy != e.plan.Models["executor"].PolicyDigest {
		return result, errors.Join(err, errors.New("child runtime policy drift"))
	}
	var identity paired.RuntimeIdentity
	bound := false
	child := team.WithInvocationBinding(context.WithValue(ctx, tools.UnattendedKey, true), func(_ context.Context, id team.InvocationIdentity) error {
		identity = paired.RuntimeIdentity{InvocationID: id.InvocationID, RunID: id.RunID, BranchID: id.BranchID}
		if err := beforeProvider(identity); err != nil {
			return err
		}
		bound = true
		return nil
	})
	_, runErr := runtime.coordinator.RunTask(child, runtime.task(e.factory.config.Worker, input, item.Verify))
	if err := runtime.close(); err != nil {
		return result, errors.Join(runErr, err)
	}
	if !bound {
		return result, errors.Join(runErr, errors.New("child did not obtain durable parent binding"))
	}
	cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer done()
	evidence, err := importChildEvidence(cleanup, e.objects, runtime, identity)
	if err != nil {
		return result, errors.Join(runErr, err)
	}
	stage := StageEvidence{SchemaVersion: 1, Request: request, Child: evidence, StopReason: "completed"}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		stage.StopReason = "timed_out"
	} else if ctx.Err() != nil {
		stage.StopReason = "cancelled"
	}
	if provider != nil {
		stage.Script = scriptFacts(provider)
	}
	result, err = e.inspect(cleanup, stage)
	if err != nil {
		return Settlement{}, err
	}
	result.Evidence, err = e.objects.PutJSON("stage_evidence", stage)
	if err != nil {
		return Settlement{}, err
	}
	if err := publishCompletion(root, result.Evidence); err != nil {
		return Settlement{}, err
	}
	// Canonical task failure is a scored observation, not a controller fault.
	// The unmodified child run result and all receipts retain runErr's cause.
	return result, nil
}

func scriptFacts(provider *scripted.Provider) *ScriptFacts {
	facts := &ScriptFacts{UsageKnown: true}
	for _, usage := range provider.ObservedUsage() {
		facts.Calls++
		if usage == nil {
			facts.UsageKnown = false
		} else if usage.InputTokens < 0 || usage.OutputTokens < 0 ||
			usage.OutputTokens > math.MaxInt64-usage.InputTokens ||
			usage.InputTokens+usage.OutputTokens > math.MaxInt64-facts.Tokens {
			facts.UsageKnown = false
			facts.Issues = append(facts.Issues, "usage_overflow")
		} else {
			facts.Tokens += usage.InputTokens + usage.OutputTokens
		}
	}
	for _, issue := range provider.Issues() {
		facts.Issues = append(facts.Issues, issue.Reason)
	}
	return facts
}

func publishCompletion(dir string, ref paired.ArtifactRef) error {
	root, err := openSourceRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	raw, err := paired.CanonicalJSON(ref)
	if err != nil {
		return err
	}
	name := ".completed-" + rand.Text()
	file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(name) }()
	_, writeErr := file.Write(raw)
	if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := root.Link(name, "completion.json"); err != nil {
		return err
	}
	return syncObjectDirectory(root)
}

func (e *ProbeExecutor) Reconcile(ctx context.Context, request StageRequest) (Reconciliation, error) {
	if _, _, err := e.checkRequest(request); err != nil {
		return Reconciliation{State: "unknown"}, err
	}
	root, err := openSourceRoot(filepath.Join(e.children, request.ChildWorkspaceID))
	if err != nil {
		return Reconciliation{State: "unknown"}, err
	}
	defer func() { _ = root.Close() }()
	raw, err := readRegular(root, "completion.json", paired.MaxDataBytes)
	if err != nil {
		return Reconciliation{State: "unknown"}, err
	}
	var ref paired.ArtifactRef
	if err := json.Unmarshal(raw, &ref, json.RejectUnknownMembers(true)); err != nil {
		return Reconciliation{State: "unknown"}, err
	}
	result, err := e.ReadSettlement(ctx, request, ref)
	if err != nil {
		return Reconciliation{State: "unknown"}, err
	}
	return Reconciliation{State: "completed", Settlement: result}, nil
}

// ReadSettlement re-derives the settlement from canonical imported evidence,
// including when the disposable child directory no longer exists.
func (e *ProbeExecutor) ReadSettlement(ctx context.Context, request StageRequest, ref paired.ArtifactRef) (Settlement, error) {
	if err := ref.Validate("stage_evidence"); err != nil {
		return Settlement{}, err
	}
	raw, err := e.objects.Read(ref)
	if err != nil {
		return Settlement{}, err
	}
	var stage StageEvidence
	if err := json.Unmarshal(raw, &stage, json.RejectUnknownMembers(true)); err != nil {
		return Settlement{}, err
	}
	if !sameStageRequest(request, stage.Request) {
		return Settlement{}, errors.New("completion belongs to another admission")
	}
	result, err := e.inspect(ctx, stage)
	result.Evidence = ref
	return result, err
}

func sameStageRequest(a, b StageRequest) bool {
	return a.Lease == b.Lease && a.Spec == b.Spec && a.Deadline.Equal(b.Deadline) && a.Limits == b.Limits
}

func (e *ProbeExecutor) inspect(ctx context.Context, stage StageEvidence) (Settlement, error) {
	item, input, err := e.checkRequest(stage.Request)
	if err != nil {
		return Settlement{}, err
	}
	if stage.SchemaVersion != 1 || (e.plan.ExecutionMode == "fixture") != (stage.Script != nil) {
		return Settlement{}, errors.New("stage evidence version or execution mode mismatch")
	}
	digest, err := runtimePolicyDigest(stage.Child.Policy, stage.Child.Overlay)
	if err != nil || digest != e.plan.Models["executor"].PolicyDigest {
		return Settlement{}, errors.New("stage evidence policy differs from frozen plan")
	}
	observation, err := inspectChildEvidence(ctx, e.objects, stage.Child)
	if err != nil {
		return Settlement{}, err
	}
	outcome, err := classifyProbe(observation, e.factory.config.Worker, input, item.Verify, stage.Child.Overlay.ResultSchemaHash)
	if err != nil {
		return Settlement{}, err
	}
	if stage.Script != nil {
		facts := stage.Script
		if facts.Calls != observation.calls || facts.UsageKnown != observation.usageKnown ||
			facts.Tokens != observation.tokens || len(facts.Issues) != 0 {
			outcome = "unknown"
		}
		// Transport diagnostics can only downgrade canonical accounting.
		observation.usageKnown = observation.usageKnown && facts.UsageKnown
	}
	switch stage.StopReason {
	case "completed":
	case "timed_out", "cancelled":
		outcome = stage.StopReason
	default:
		return Settlement{}, errors.New("unknown child stop reason")
	}
	if !observation.usageKnown {
		outcome = "unknown"
	}
	return Settlement{Identity: stage.Child.Identity, Outcome: outcome, UsageKnown: observation.usageKnown,
		Tokens: observation.tokens, Stopped: true}, nil
}

func classifyProbe(observation childObservation, worker, input string, verifier paired.Verifier, schemaHash string) (string, error) {
	if len(observation.session.Tasks) != 1 {
		return "", errors.New("probe must contain exactly one canonical task")
	}
	task := observation.session.Tasks[0]
	spec := verifier.RuntimeSpec()
	if task.Agent != worker || task.Goal != input || !reflect.DeepEqual(task.VerifySpec, &spec) ||
		task.ResultContract == nil || task.ResultContract.SchemaSHA256 != schemaHash {
		return "", errors.New("canonical task differs from frozen worker, input or verifier")
	}
	receipt, typed := task.ExecutionReceipt, task.TypedResult
	if observation.providerFailed {
		return "unknown", nil
	}
	if receipt == nil || receipt.ResultContractSHA256 != schemaHash {
		return "unknown", nil
	}
	if typed != nil && typed.StructuredPayload != nil {
		payload := typed.StructuredPayload
		if typed.TaskID != task.ID || typed.Agent != task.Agent || typed.Source != "submitted" ||
			payload.Contract.SchemaSHA256 != schemaHash || payload.SHA256 != paired.BytesDigest(payload.Value) ||
			receipt.ResultPayloadSHA256 != payload.SHA256 || receipt.ResultValidation != team.ResultValidationValid {
			return "", errors.New("canonical result payload or receipt binding mismatch")
		}
		verification := task.VerifyResult
		if task.Status == team.TaskDone && typed.Status == team.TaskResultStatusSuccess &&
			verification != nil && !verification.TimedOut && !verification.Overturned &&
			verification.ExitCode == 0 && reflect.DeepEqual(verification.Spec, &spec) {
			return "passed", nil
		}
		if typed.Status == team.TaskResultStatusFailed {
			return "failed", nil
		}
	}
	if task.FailureEvent != nil {
		switch task.FailureEvent.FailureClass {
		case team.FailureTimeout:
			return "timed_out", nil
		case team.FailureCancelled:
			return "cancelled", nil
		case team.FailureVerify:
			if task.VerifyResult != nil && !task.VerifyResult.TimedOut && task.VerifyResult.ExitCode != 0 {
				return "failed", nil
			}
		}
	}
	if receipt.ResultValidation == team.ResultValidationInvalid && receipt.ResultValidationFailures > 0 &&
		observation.usageKnown {
		return "failed", nil
	}
	return "unknown", nil
}

var _ StageExecutor = (*ProbeExecutor)(nil)
