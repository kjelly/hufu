package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/utils"
)

// ReliabilityFault identifies a replayable fault-injection scenario in the
// HF-AR-006 corpus. Scenarios contain only structured evidence, never prompts
// or model output.
type ReliabilityFault string

const (
	FaultProviderTimeout      ReliabilityFault = "provider_timeout"
	FaultSidecarFailure       ReliabilityFault = "sidecar_failure"
	FaultVerifyWrongPolarity  ReliabilityFault = "verify_wrong_polarity"
	FaultPartialExternalWrite ReliabilityFault = "partial_external_write"
	FaultRepeatedFailure      ReliabilityFault = "repeated_failure"
	FaultCorruptCheckpoint    ReliabilityFault = "corrupt_checkpoint"
	FaultIncorrectAcceptance  ReliabilityFault = "incorrect_acceptance"
	FaultSecretLeakage        ReliabilityFault = "secret_leakage"
)

type ReliabilityScenario struct {
	ID                  string
	Fault               ReliabilityFault
	Injection           FaultInjection
	ExpectedDisposition RetryDisposition
	SecretNeedle        string
}

// FaultInjection is the replay input for one HF-AR-006 scenario. It models
// the boundary observation produced by a failing provider, verifier,
// checkpoint, or acceptance gate; the diagnosis policy never receives a
// hand-authored policy decision.
type FaultInjection struct {
	FailureClass        TaskFailureClass
	SideEffect          SideEffectClass
	Recovery            RecoveryPolicy
	Replayable          bool
	EvidenceComplete    bool
	SidecarAvailable    bool
	UnfixableVerify     bool
	FailureFingerprint  string
	PreviousFingerprint string
	Detail              string
}

// ReliabilityFaultInjector converts a scenario's boundary fault into the
// structured observations consumed by the production diagnosis policy.
// Keeping this as an explicit seam makes replay independent from live
// providers, sidecars, external writes, and checkpoint storage.
type ReliabilityFaultInjector interface {
	Inject(ReliabilityScenario) (DiagnosisInput, error)
}

// ReliabilityBoundaryEvidence proves that a replay crossed the same durable
// boundary used by production rather than only constructing a policy input.
type ReliabilityBoundaryEvidence struct {
	Source               string `json:"source"`
	EventReplayed        bool   `json:"event_replayed,omitempty"`
	CheckpointCorrupted  bool   `json:"checkpoint_corrupted,omitempty"`
	CheckpointRejected   bool   `json:"checkpoint_rejected,omitempty"`
	AcceptanceRejected   bool   `json:"acceptance_rejected,omitempty"`
	RecoveryState        string `json:"recovery_state,omitempty"`
	PolarityDetected     bool   `json:"polarity_detected,omitempty"`
	RepeatedFailureFound bool   `json:"repeated_failure_found,omitempty"`
	BoundaryRedacted     bool   `json:"boundary_redacted,omitempty"`
}

type defaultReliabilityFaultInjector struct{}

func (defaultReliabilityFaultInjector) Inject(s ReliabilityScenario) (DiagnosisInput, error) {
	in := s.Injection
	switch s.Fault {
	case FaultProviderTimeout, FaultSidecarFailure, FaultVerifyWrongPolarity,
		FaultPartialExternalWrite, FaultRepeatedFailure, FaultCorruptCheckpoint,
		FaultIncorrectAcceptance, FaultSecretLeakage:
	default:
		return DiagnosisInput{}, fmt.Errorf("unknown reliability fault %q", s.Fault)
	}
	// These mutations represent the boundary where the fault was observed,
	// rather than a precomputed disposition. They make replay cases explicit
	// and preserve the same safety signals production receives.
	switch s.Fault {
	case FaultSidecarFailure:
		in.SidecarAvailable = false
	case FaultVerifyWrongPolarity:
		in.UnfixableVerify = true
	case FaultPartialExternalWrite:
		in.SideEffect, in.Replayable, in.Recovery = SideEffectExternalWrite, false, RecoveryReconcile
	case FaultRepeatedFailure:
		in.FailureFingerprint, in.PreviousFingerprint = "same-digest", "same-digest"
	case FaultCorruptCheckpoint:
		in.FailureClass, in.EvidenceComplete, in.Replayable, in.Recovery = FailureProtocol, false, false, RecoveryManual
	case FaultIncorrectAcceptance:
		in.FailureClass, in.Recovery = FailureContract, RecoveryRetry
	case FaultSecretLeakage:
		if in.Detail == "" {
			in.Detail = `provider returned api_key = "sk-test-secret-123"`
		}
	}
	return DiagnosisInput{RecoveryDecisionInput: RecoveryDecisionInput{
		FailureClass: in.FailureClass, SideEffect: in.SideEffect, RecoveryPolicy: in.Recovery,
		Attempt: 1, MaxRetries: 3, EvidenceComplete: in.EvidenceComplete, Replayable: in.Replayable,
		FailureFingerprint: in.FailureFingerprint, PreviousFingerprint: in.PreviousFingerprint,
		UnfixableVerify: in.UnfixableVerify,
	}, FailureClass: in.FailureClass, TaskID: "eval-task", RunID: "eval-run", Attempt: 1,
		Detail: in.Detail, SideEffect: in.SideEffect, Recovery: in.Recovery,
		SidecarAvailable: in.SidecarAvailable}, nil
}

type ReliabilityScenarioResult struct {
	ScenarioID       string                       `json:"scenario_id"`
	Fault            ReliabilityFault             `json:"fault"`
	Disposition      RetryDisposition             `json:"disposition"`
	Expected         RetryDisposition             `json:"expected"`
	Passed           bool                         `json:"passed"`
	SecretSafe       bool                         `json:"secret_safe"`
	BoundaryReplayed bool                         `json:"boundary_replayed"`
	BoundaryEvidence *ReliabilityBoundaryEvidence `json:"boundary_evidence,omitempty"`
	Error            string                       `json:"error,omitempty"`
}

type ReliabilityEvalMetrics struct {
	TotalScenarios        int     `json:"total_scenarios"`
	PassedScenarios       int     `json:"passed_scenarios"`
	ProductionRuns        int     `json:"production_runs"`
	FalseCompletionRate   float64 `json:"false_completion_rate"`
	EvidenceCoverage      float64 `json:"evidence_coverage"`
	DiagnosticDeterminism float64 `json:"diagnostic_determinism"`
	RepairConvergence     float64 `json:"repair_convergence"`
	UnsafeReplayRate      float64 `json:"unsafe_replay_rate"`
}

type ReliabilityEvalReport struct {
	Version                int                         `json:"version"`
	GeneratedAt            time.Time                   `json:"generated_at"`
	Scenarios              []ReliabilityScenarioResult `json:"scenarios"`
	Metrics                ReliabilityEvalMetrics      `json:"metrics"`
	ProductionObservation  *ReliabilityObservation     `json:"production_observation,omitempty"`
	ProductionObservations []ReliabilityObservation    `json:"production_observations,omitempty"`
	Rollout                ReliabilityRolloutMode      `json:"rollout,omitempty"`
}

type ReliabilityRolloutViolation struct {
	Mode    ReliabilityRolloutMode
	Reasons []string
}

func (e *ReliabilityRolloutViolation) Error() string {
	return fmt.Sprintf("reliability rollout %s rejected terminal result: %s", e.Mode, strings.Join(e.Reasons, "; "))
}

// DefaultReliabilityScenarioCorpus is the deterministic fault corpus required
// by Phase 5. Every expected disposition comes from DiagnosisPolicy, not from
// a model or scenario text.
func DefaultReliabilityScenarioCorpus() []ReliabilityScenario {
	base := func(class TaskFailureClass) FaultInjection {
		return FaultInjection{FailureClass: class, SideEffect: SideEffectNone, Recovery: RecoveryRetry,
			EvidenceComplete: true, Replayable: true, SidecarAvailable: true, Detail: "structured fault evidence"}
	}
	partial := base(FailureTimeout)
	partial.SideEffect = SideEffectExternalWrite
	partial.Replayable = false
	partial.Recovery = RecoveryReconcile
	repeated := base(FailureExecution)
	repeated.FailureFingerprint, repeated.PreviousFingerprint = "same-digest", "same-digest"
	corrupt := base(FailureProtocol)
	corrupt.EvidenceComplete = false
	corrupt.Replayable = false
	corrupt.Recovery = RecoveryManual
	secret := base(FailureExecution)
	secret.Detail = `provider returned api_key = "sk-test-secret-123"`
	return []ReliabilityScenario{
		{ID: "provider-timeout", Fault: FaultProviderTimeout, Injection: base(FailureTimeout), ExpectedDisposition: RetryWorker},
		{ID: "sidecar-failure", Fault: FaultSidecarFailure, Injection: func() FaultInjection { in := base(FailureTimeout); in.SidecarAvailable = false; return in }(), ExpectedDisposition: RetryWorker},
		{ID: "verify-wrong-polarity", Fault: FaultVerifyWrongPolarity, Injection: func() FaultInjection { in := base(FailureVerify); in.UnfixableVerify = true; return in }(), ExpectedDisposition: ReplanRequired},
		{ID: "partial-external-write", Fault: FaultPartialExternalWrite, Injection: partial, ExpectedDisposition: ReconcileOnly},
		{ID: "repeated-failure", Fault: FaultRepeatedFailure, Injection: repeated, ExpectedDisposition: ReplanRequired},
		{ID: "corrupt-checkpoint", Fault: FaultCorruptCheckpoint, Injection: corrupt, ExpectedDisposition: ReconcileOnly},
		{ID: "incorrect-acceptance", Fault: FaultIncorrectAcceptance, Injection: func() FaultInjection {
			in := base(FailureContract)
			in.Detail = "acceptance contract changed"
			return in
		}(), ExpectedDisposition: ReplanRequired},
		{ID: "secret-leakage", Fault: FaultSecretLeakage, Injection: secret, ExpectedDisposition: RetryWorker, SecretNeedle: "sk-test-secret-123"},
	}
}

func ReplayReliabilityScenario(s ReliabilityScenario) (ReliabilityScenarioResult, error) {
	result := ReliabilityScenarioResult{ScenarioID: s.ID, Fault: s.Fault, Expected: s.ExpectedDisposition}
	evidence, err := replayReliabilityBoundary(s)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.BoundaryReplayed = true
	result.BoundaryEvidence = &evidence
	input, err := (defaultReliabilityFaultInjector{}).Inject(s)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	packet, err := (DiagnosisPolicy{}).Diagnose(input)
	if err != nil {
		result.Error = err.Error()
		return result, err
	}
	result.Disposition = packet.Disposition
	result.Passed = packet.Disposition == s.ExpectedDisposition
	result.SecretSafe = s.SecretNeedle == "" || !strings.Contains(packet.FailureSummary, s.SecretNeedle)
	if !result.SecretSafe {
		result.Passed = false
		result.Error = "diagnostic packet contains scenario secret"
	}
	return result, nil
}

// replayReliabilityBoundary executes a deterministic, local replay of each
// production boundary. The replay is deliberately network-free: provider and
// sidecar failures are recorded and reconstructed through EventStore, an
// interrupted external write goes through RepairController, acceptance uses
// CompletionGate, and checkpoint corruption goes through LoadSession.
func replayReliabilityBoundary(s ReliabilityScenario) (ReliabilityBoundaryEvidence, error) {
	evidence := ReliabilityBoundaryEvidence{}
	switch s.Fault {
	case FaultProviderTimeout, FaultSidecarFailure, FaultSecretLeakage:
		evidence.Source = "event_store"
		workspace, err := os.MkdirTemp("", "hufu-reliability-replay-")
		if err != nil {
			return evidence, err
		}
		defer func() { _ = os.RemoveAll(workspace) }()
		store, err := NewEventStore(workspace, "replay-run", "replay-session")
		if err != nil {
			return evidence, err
		}
		payload, _ := json.Marshal(map[string]string{"fault": string(s.Fault), "detail": utils.RedactSecrets(s.Injection.Detail)})
		err = store.Append(RunEvent{Type: "boundary_fault", Actor: "replay", Payload: payload})
		if err == nil {
			err = store.VerifyHashChain()
		}
		if err == nil {
			events, readErr := store.ReadEvents()
			err = readErr
			evidence.EventReplayed = len(events) == 1 && events[0].Type == "boundary_fault"
			if s.Fault == FaultSecretLeakage {
				stored, marshalErr := json.Marshal(events)
				err = marshalErr
				evidence.BoundaryRedacted = err == nil && !strings.Contains(string(stored), s.SecretNeedle)
			}
		}
		_ = store.Close()
		if err != nil {
			return evidence, err
		}
		if !evidence.EventReplayed {
			return evidence, fmt.Errorf("boundary event replay did not reconstruct %q", s.Fault)
		}
		if s.Fault == FaultSecretLeakage && !evidence.BoundaryRedacted {
			return evidence, fmt.Errorf("secret leakage replay was not redacted at event boundary")
		}
	case FaultVerifyWrongPolarity:
		evidence.Source = "verification_boundary"
		evidence.PolarityDetected = isUnfixableVerifyFailure(fmt.Errorf("%w", errWrongVerificationPolarity))
	case FaultPartialExternalWrite:
		evidence.Source = "repair_controller"
		allowReplay := false
		task := TaskDef{SideEffect: SideEffectExternalWrite, Recovery: RecoveryReconcile,
			ReconcileTool: "probe-state", Execution: ExecutionContract{AllowsReplay: &allowReplay}}
		outcome := NewRepairController().Execute(context.Background(), RepairRequest{
			Task: task, Attempt: 1, MaxAttempts: 3,
			Checkpoint: func(context.Context) error { return nil },
			Reconcile:  func(context.Context) (string, error) { return RecoveryStatePartial, nil },
		})
		evidence.RecoveryState = RecoveryStatePartial
		if outcome.Decision.Action != RepairReconcile || outcome.State != "blocked" {
			return evidence, fmt.Errorf("partial external replay chose %s/%s", outcome.Decision.Action, outcome.State)
		}
	case FaultRepeatedFailure:
		evidence.Source = "failure_fingerprint_boundary"
		evidence.RepeatedFailureFound = sameFailure("attempt 1 failed: same-digest", "attempt 2 failed: same-digest")
	case FaultCorruptCheckpoint:
		evidence.Source = "session_checkpoint"
		workspace, err := os.MkdirTemp("", "hufu-reliability-checkpoint-")
		if err != nil {
			return evidence, err
		}
		defer func() { _ = os.RemoveAll(workspace) }()
		if err := os.WriteFile(filepath.Join(workspace, sessionFile), []byte("{corrupt checkpoint"), 0o600); err != nil {
			return evidence, err
		}
		evidence.CheckpointCorrupted = true
		// Deliberately the quiet loader: this scenario asserts that a corrupt
		// checkpoint is rejected, so the rejection is the expected result and
		// must not print an operator warning about workspace damage into a real
		// run's stderr.
		loaded, _ := loadSessionQuiet(workspace)
		evidence.CheckpointRejected = loaded == nil
		if !evidence.CheckpointRejected {
			return evidence, fmt.Errorf("corrupt checkpoint was accepted")
		}
	case FaultIncorrectAcceptance:
		evidence.Source = "completion_gate"
		decision := EvaluateCompletionGate(context.Background(), CompletionGateInput{
			Result:     &RunResult{Outcome: RunOutcomeCompleted, GoalSatisfied: true},
			Acceptance: &AcceptanceResult{State: AcceptanceFailed},
		})
		evidence.AcceptanceRejected = !decision.Accepted
		if !evidence.AcceptanceRejected {
			return evidence, fmt.Errorf("incorrect acceptance was accepted")
		}
	default:
		return evidence, fmt.Errorf("unknown reliability fault %q", s.Fault)
	}
	return evidence, nil
}

func RunReliabilityEvalSuite(scenarios []ReliabilityScenario) ReliabilityEvalReport {
	if scenarios == nil {
		scenarios = DefaultReliabilityScenarioCorpus()
	}
	report := ReliabilityEvalReport{Version: 1, GeneratedAt: time.Now().UTC()}
	for _, scenario := range scenarios {
		result, err := ReplayReliabilityScenario(scenario)
		if err != nil && result.Error == "" {
			result.Error = err.Error()
		}
		report.Scenarios = append(report.Scenarios, result)
		if result.Passed {
			report.Metrics.PassedScenarios++
		}
	}
	report.Metrics.TotalScenarios = len(report.Scenarios)
	if report.Metrics.TotalScenarios > 0 {
		report.Metrics.DiagnosticDeterminism = diagnosticDeterminism(scenarios)
	}
	return report
}

func SaveReliabilityEvalReport(workspace string, report ReliabilityEvalReport) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("workspace is required")
	}
	if report.Version == 0 {
		report.Version = 1
	}
	if report.GeneratedAt.IsZero() {
		report.GeneratedAt = time.Now().UTC()
	}
	dir := filepath.Join(workspace, logsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(filepath.Join(dir, "reliability_eval.json"), b, 0o644)
}

func diagnosticDeterminism(scenarios []ReliabilityScenario) float64 {
	if len(scenarios) == 0 {
		return 1
	}
	stable := 0
	for _, scenario := range scenarios {
		firstInput, firstInputErr := (defaultReliabilityFaultInjector{}).Inject(scenario)
		secondInput, secondInputErr := (defaultReliabilityFaultInjector{}).Inject(scenario)
		first, firstErr := (DiagnosisPolicy{}).Diagnose(firstInput)
		second, secondErr := (DiagnosisPolicy{}).Diagnose(secondInput)
		if firstInputErr == nil && secondInputErr == nil && firstErr == nil && secondErr == nil && first.Disposition == second.Disposition {
			stable++
		}
	}
	return float64(stable) / float64(len(scenarios))
}

type ReliabilityObservation struct {
	Accepted                bool `json:"accepted"`
	AcceptancePassed        bool `json:"acceptance_passed"`
	EvidenceComplete        bool `json:"evidence_complete"`
	RepairAttempted         bool `json:"repair_attempted"`
	RepairAccepted          bool `json:"repair_accepted"`
	RepairAttempts          int  `json:"repair_attempts,omitempty"`
	RepairSuccesses         int  `json:"repair_successes,omitempty"`
	ReplayAttempts          int  `json:"replay_attempts,omitempty"`
	UnsafeReplay            bool `json:"unsafe_replay"`
	UnsafeReplayCount       int  `json:"unsafe_replay_count,omitempty"`
	ReconciliationAttempts  int  `json:"reconciliation_attempts,omitempty"`
	ReconciliationSucceeded int  `json:"reconciliation_succeeded,omitempty"`
}

func reliabilityObservation(result *RunResult) ReliabilityObservation {
	if result == nil {
		return ReliabilityObservation{}
	}
	acceptancePassed := result.Acceptance != nil && result.Acceptance.IsPassed()
	evidenceComplete := result.EvidenceManifest != nil && result.EvidenceManifest.Status == "accepted" && result.EvidenceManifest.ManifestHash != ""
	repairs := result.Metrics.ProtocolRepairsAttempted
	for _, count := range result.Metrics.RepairAttemptsByCriterion {
		repairs += count
	}
	acceptedRepair := repairs > 0 && result.GoalSatisfied && acceptancePassed && evidenceComplete
	return ReliabilityObservation{
		Accepted:                result.Outcome == RunOutcomeCompleted && result.GoalSatisfied,
		AcceptancePassed:        acceptancePassed,
		EvidenceComplete:        evidenceComplete,
		RepairAttempted:         repairs > 0,
		RepairAccepted:          acceptedRepair,
		RepairAttempts:          repairs,
		RepairSuccesses:         boolInt(acceptedRepair) * repairs,
		ReplayAttempts:          result.Metrics.ReplayAttempts,
		UnsafeReplay:            result.Metrics.UnsafeReplaysDetected > 0,
		UnsafeReplayCount:       result.Metrics.UnsafeReplaysDetected,
		ReconciliationAttempts:  result.Metrics.ReconciliationAttempts,
		ReconciliationSucceeded: result.Metrics.ReconciliationSucceeded,
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// PersistReliabilityEvaluation runs the HF-AR-006 replay corpus on every
// production terminal path and adds the real run observation to its metrics.
// The rollout resolver is intentionally consulted here so a future strict
// mode cannot be enabled without an acceptance contract.
func (c *Coordinator) PersistReliabilityEvaluation(result *RunResult) (ReliabilityEvalReport, error) {
	if c == nil || c.session == nil {
		return ReliabilityEvalReport{}, fmt.Errorf("reliability evaluation requires a session")
	}
	acceptanceConfigured := acceptanceContractConfigured(c.session, result)
	requested := ReliabilityRolloutMode(strings.TrimSpace(c.session.Config.Reliability.Rollout))
	mode, err := ResolveReliabilityRollout(requested, acceptanceConfigured)
	if err != nil {
		return ReliabilityEvalReport{}, err
	}
	report := RunReliabilityEvalSuite(nil)
	observation := reliabilityObservation(result)
	report.ProductionObservation = &observation
	previous, historyErr := loadReliabilityEvalReport(c.session.Workspace)
	if historyErr != nil && !errors.Is(historyErr, os.ErrNotExist) {
		return report, fmt.Errorf("load historical reliability report: %w", historyErr)
	}
	observations := append([]ReliabilityObservation(nil), previous.ProductionObservations...)
	if len(observations) == 0 && previous.ProductionObservation != nil {
		observations = append(observations, *previous.ProductionObservation)
	}
	observations = append(observations, observation)
	report.ProductionObservations = observations
	report.Rollout = mode
	var strictViolation *ReliabilityRolloutViolation
	if reasons := reliabilityRolloutViolations(report); len(reasons) > 0 {
		switch mode {
		case RolloutWarnOnly:
			_ = c.emitEvent("reliability_warning", "coordinator", "", map[string]interface{}{"mode": mode, "reasons": reasons})
		case RolloutStrictOptIn, RolloutStrictDefault:
			violation := &ReliabilityRolloutViolation{Mode: mode, Reasons: reasons}
			// Strict enforcement changes the canonical terminal result before
			// the observation is persisted. The rejected run must never remain
			// an accepted production population member in historical metrics.
			downgradeReliabilityResult(result, violation)
			observation = reliabilityObservation(result)
			report.ProductionObservation = &observation
			observations[len(observations)-1] = observation
			report.ProductionObservations = observations
			strictViolation = violation
		}
	}
	applyProductionReliabilityMetrics(&report, observations)
	if err := SaveReliabilityEvalReport(c.session.Workspace, report); err != nil {
		return report, err
	}
	if strictViolation != nil {
		return report, strictViolation
	}
	return report, nil
}

func acceptanceContractConfigured(session *TeamSession, result *RunResult) bool {
	if session != nil {
		cfg := session.Config
		if strings.TrimSpace(cfg.Acceptance) != "" {
			return true
		}
		if spec := cfg.AcceptanceSpec; spec != nil {
			if len(spec.Commands) > 0 || len(spec.RequiredArtifacts) > 0 || spec.RequireNoUnresolvedTasks || len(spec.Verifications) > 0 || len(spec.Criteria) > 0 {
				return true
			}
		}
	}
	return result != nil && result.Acceptance != nil && result.Acceptance.EffectiveState() != AcceptanceNotConfigured
}

func downgradeReliabilityResult(result *RunResult, violation *ReliabilityRolloutViolation) {
	if violation == nil {
		return
	}
	downgradeReliabilityResultForError(result, violation)
}

func downgradeReliabilityResultForError(result *RunResult, cause error) {
	if result == nil || cause == nil || result.Outcome != RunOutcomeCompleted || !result.GoalSatisfied {
		return
	}
	result.Outcome = RunOutcomeBlocked
	result.GoalSatisfied = false
	result.StopReason = StopReasonRunFailed
	result.ExitCode = 7
	result.Reason = cause.Error()
}

func applyProductionReliabilityMetrics(report *ReliabilityEvalReport, observations []ReliabilityObservation) {
	if report == nil {
		return
	}
	quantitative := ComputeReliabilityMetrics(observations)
	report.Metrics.ProductionRuns = len(observations)
	report.Metrics.FalseCompletionRate = quantitative.FalseCompletionRate
	report.Metrics.EvidenceCoverage = quantitative.EvidenceCoverage
	report.Metrics.RepairConvergence = quantitative.RepairConvergence
	report.Metrics.UnsafeReplayRate = quantitative.UnsafeReplayRate
}

func loadReliabilityEvalReport(workspace string) (ReliabilityEvalReport, error) {
	data, err := os.ReadFile(filepath.Join(workspace, logsDir, "reliability_eval.json"))
	if err != nil {
		return ReliabilityEvalReport{}, err
	}
	var report ReliabilityEvalReport
	if err := json.Unmarshal(data, &report); err != nil {
		return ReliabilityEvalReport{}, err
	}
	return report, nil
}

func reliabilityRolloutViolations(report ReliabilityEvalReport) []string {
	observation := report.ProductionObservation
	if observation == nil || !observation.Accepted {
		return nil
	}
	var reasons []string
	if report.Metrics.FalseCompletionRate > 0 {
		reasons = append(reasons, "false completion detected")
	}
	if !observation.AcceptancePassed {
		reasons = append(reasons, "acceptance did not pass")
	}
	if !observation.EvidenceComplete {
		reasons = append(reasons, "evidence manifest is incomplete")
	}
	if observation.UnsafeReplayCount > 0 || observation.UnsafeReplay {
		reasons = append(reasons, "unsafe replay detected")
	}
	if observation.RepairAttempted && !observation.RepairAccepted {
		reasons = append(reasons, "repair did not converge through acceptance")
	}
	return reasons
}

func ComputeReliabilityMetrics(observations []ReliabilityObservation) ReliabilityEvalMetrics {
	metrics := ReliabilityEvalMetrics{TotalScenarios: len(observations)}
	var accepted, completeEvidence, repairs, converged, unsafe, replayAttempts int
	hasExplicitReplayCounts := false
	for _, observation := range observations {
		if observation.Accepted && !observation.AcceptancePassed {
			metrics.FalseCompletionRate++
		}
		if observation.Accepted {
			accepted++
			if observation.EvidenceComplete {
				completeEvidence++
			}
		}
		if observation.RepairAttempts > 0 {
			repairs += observation.RepairAttempts
			converged += observation.RepairSuccesses
		} else if observation.RepairAttempted {
			repairs++
			if observation.RepairAccepted {
				converged++
			}
		}
		if observation.ReplayAttempts > 0 {
			hasExplicitReplayCounts = true
			replayAttempts += observation.ReplayAttempts
			unsafe += observation.UnsafeReplayCount
		} else if observation.UnsafeReplay {
			replayAttempts++
			unsafe++
		}
	}
	if accepted > 0 {
		metrics.EvidenceCoverage = float64(completeEvidence) / float64(accepted)
	}
	if repairs > 0 {
		metrics.RepairConvergence = float64(converged) / float64(repairs)
	}
	if len(observations) > 0 {
		metrics.FalseCompletionRate /= float64(len(observations))
		if hasExplicitReplayCounts && replayAttempts > 0 {
			metrics.UnsafeReplayRate = float64(unsafe) / float64(replayAttempts)
		} else {
			metrics.UnsafeReplayRate = float64(unsafe) / float64(len(observations))
		}
	}
	return metrics
}

type ReliabilityRolloutMode string

const (
	RolloutShadow        ReliabilityRolloutMode = "shadow"
	RolloutWarnOnly      ReliabilityRolloutMode = "warn-only"
	RolloutStrictOptIn   ReliabilityRolloutMode = "strict-opt-in"
	RolloutStrictDefault ReliabilityRolloutMode = "strict-default"
)

func ResolveReliabilityRollout(requested ReliabilityRolloutMode, acceptanceConfigured bool) (ReliabilityRolloutMode, error) {
	if requested == "" {
		if acceptanceConfigured {
			return RolloutStrictDefault, nil
		}
		return RolloutShadow, nil
	}
	switch requested {
	case RolloutShadow, RolloutWarnOnly:
		return requested, nil
	case RolloutStrictOptIn, RolloutStrictDefault:
		if !acceptanceConfigured {
			return RolloutShadow, fmt.Errorf("%s requires an acceptance contract", requested)
		}
		return requested, nil
	default:
		return "", fmt.Errorf("unknown reliability rollout mode %q", requested)
	}
}
