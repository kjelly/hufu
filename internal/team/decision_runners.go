package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/sidecar"
)

// Production stage runners for the decision engine
// (docs/hufu-decision-aware-runtime-spec.md Phase 3.5).
//
// Every decision stage runs on the judge sidecar. That choice does real work:
// the sidecar has no tool surface, so "decision workers are read-only by
// default" (§38.1) holds by construction rather than by an enforcement check
// that could be forgotten. It also keeps the engine free of any provider
// knowledge, which §39 item 4 requires.
//
// Each runner does the same three things: render the prompt the engine already
// built, ask the sidecar, and decode a typed struct. Nothing here scores,
// aggregates or decides — that stays in Go.

const (
	// decisionStageTimeout bounds one stage dispatch. A decision that cannot
	// answer in this window fails the stage rather than stalling the run.
	decisionStageTimeout = 90 * time.Second
)

// Decision stage purposes. Every auxiliary invocation names a purpose from a
// closed registry (context_purpose.go); an unregistered name is rejected
// before the sidecar is called. They are constants shared by the runner and
// the registry so the two cannot drift — an unregistered stage purpose is not
// a degraded decision, it is a decision that cannot dispatch at all.
const (
	decisionPurposeJudge                   = "decision-judge"
	decisionPurposeReferenceEvidence       = "decision-reference-evidence"
	decisionPurposeOptions                 = "decision-options"
	decisionPurposeChallenge               = "decision-challenge"
	decisionPurposePremortem               = "decision-premortem"
	decisionPurposeRevision                = "decision-revision"
	decisionPurposeFinalizationCoordinator = "decision-finalization-coordinator"
	decisionPurposeFinalizationJudge       = "decision-finalization-judge"
)

// decisionStagePurposes is every purpose a decision stage may dispatch under.
var decisionStagePurposes = []string{
	decisionPurposeJudge,
	decisionPurposeReferenceEvidence,
	decisionPurposeOptions,
	decisionPurposeChallenge,
	decisionPurposePremortem,
	decisionPurposeRevision,
	decisionPurposeFinalizationCoordinator,
	decisionPurposeFinalizationJudge,
}

// coordinatorDecisionRunners implements every stage runner over one
// coordinator's judge sidecar.
type coordinatorDecisionRunners struct {
	coordinator *Coordinator
	todoID      string
}

// newDecisionRunners builds the stage runners for a task.
func newDecisionRunners(c *Coordinator, todoID string) *coordinatorDecisionRunners {
	return &coordinatorDecisionRunners{coordinator: c, todoID: todoID}
}

// available reports whether a decision can run at all. Without a judge model
// there is no way to form one, and silently degrading to zero judges is exactly
// what §34 forbids.
func (r *coordinatorDecisionRunners) available() bool {
	return r != nil && r.coordinator != nil && r.coordinator.AgentPool() != nil &&
		r.coordinator.AgentPool().JudgeSidecar() != nil
}

// ask dispatches one stage prompt and returns the raw response.
func (r *coordinatorDecisionRunners) ask(ctx context.Context, purpose, prompt string) (string, error) {
	if !r.available() {
		return "", fmt.Errorf("decision stage %q needs a judge model; none is configured", purpose)
	}
	s := r.coordinator.AgentPool().JudgeSidecar()

	stageCtx, cancel := context.WithTimeout(ctx, decisionStageTimeout)
	defer cancel()
	r.coordinator.report(r.coordinator.newEvent("sidecar_call").withMessage("decision:" + purpose))

	response, err := s.ExecuteProfile(sidecar.WithPurpose(stageCtx, purpose), prompt, sidecar.JudgeProfile)
	if err != nil {
		return "", fmt.Errorf("decision stage %q: %w", purpose, err)
	}
	if strings.TrimSpace(response) == "" {
		return "", fmt.Errorf("decision stage %q returned an empty response", purpose)
	}
	return response, nil
}

// decodeStage extracts the fenced JSON payload and decodes it. Malformed
// output is an error the engine turns into its bounded repair attempt; it is
// never patched up into a plausible-looking judgment.
func decodeStage(response string, target any) error {
	payload := extractJSONPayload(response)
	if err := json.Unmarshal([]byte(payload), target); err != nil {
		return fmt.Errorf("response was not the required JSON object: %w", err)
	}
	return nil
}

// decodeReferenceEvidence is deliberately separate from the shared judge
// decoder. Reference producers may return one raw JSON document or one whole
// fenced JSON document, but never prose surrounding it or multiple documents.
func decodeReferenceEvidence(response string, target any) error {
	if len([]byte(response)) > ReferenceEvidenceProducerMaxRawBytes {
		return fmt.Errorf("reference evidence response exceeds %d bytes", ReferenceEvidenceProducerMaxRawBytes)
	}
	payload, err := referenceEvidencePayload(response)
	if err != nil {
		return err
	}
	if err := decodeReferenceJSON([]byte(payload), target, ReferenceEvidenceProducerMaxRawBytes, "reference evidence response"); err != nil {
		return fmt.Errorf("response was not the required JSON object: %w", err)
	}
	return nil
}

func referenceEvidencePayload(response string) (string, error) {
	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		return "", fmt.Errorf("reference evidence response is empty")
	}
	if !strings.Contains(trimmed, "```") {
		return trimmed, nil
	}

	lines := strings.Split(trimmed, "\n")
	if len(lines) < 3 {
		return "", fmt.Errorf("reference evidence response must contain one complete JSON fence")
	}
	opening := strings.TrimSpace(lines[0])
	if opening != "```" && opening != "```json" {
		return "", fmt.Errorf("reference evidence response contains prose or an unsupported fence")
	}
	if strings.TrimSpace(lines[len(lines)-1]) != "```" {
		return "", fmt.Errorf("reference evidence response must contain one complete JSON fence")
	}
	body := strings.Join(lines[1:len(lines)-1], "\n")
	if strings.Contains(body, "```") {
		return "", fmt.Errorf("reference evidence response contains multiple fences")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", fmt.Errorf("reference evidence response fence is empty")
	}
	return body, nil
}

// judgeResponse is the wire shape of one judge's answer. It is decoded into a
// DecisionOpinion rather than unmarshalled directly so a judge cannot set
// runtime-owned fields such as Valid or EvidenceHash.
type judgeResponse struct {
	OptionScores []struct {
		OptionID string             `json:"option_id"`
		Criteria map[string]float64 `json:"criteria"`
		Overall  float64            `json:"overall"`
	} `json:"option_scores"`
	PreferredOption       string   `json:"preferred_option"`
	SuccessProbability    float64  `json:"success_probability"`
	KeyAssumptions        []string `json:"key_assumptions"`
	DisconfirmingEvidence []string `json:"disconfirming_evidence"`
	MissingInformation    []string `json:"missing_information"`
	Confidence            float64  `json:"confidence"`
}

// RunJudge implements JudgeRunner.
func (r *coordinatorDecisionRunners) RunJudge(ctx context.Context, req JudgeRequest) (DecisionOpinion, error) {
	if req.RoutingRole != nil {
		return r.runJudgeViaCapabilityRouting(ctx, req)
	}
	response, err := r.ask(ctx, decisionPurposeJudge, req.Context.Prompt)
	if err != nil {
		return DecisionOpinion{}, err
	}
	return decodeJudgeOpinion(response, req.JudgeID)
}

// decodeJudgeOpinion decodes one judge's raw response into a DecisionOpinion.
// Shared by the legacy sidecar path and the capability-routed path so the
// opinion-shape contract never depends on who produced the text.
func decodeJudgeOpinion(response, judgeID string) (DecisionOpinion, error) {
	var decoded judgeResponse
	if err := decodeStage(response, &decoded); err != nil {
		return DecisionOpinion{}, fmt.Errorf("judge %s: %w", judgeID, err)
	}
	opinion := DecisionOpinion{
		PreferredOption:       strings.TrimSpace(decoded.PreferredOption),
		SuccessProbability:    decoded.SuccessProbability,
		KeyAssumptions:        decoded.KeyAssumptions,
		DisconfirmingEvidence: decoded.DisconfirmingEvidence,
		MissingInformation:    decoded.MissingInformation,
		Confidence:            decoded.Confidence,
	}
	for _, score := range decoded.OptionScores {
		opinion.OptionScores = append(opinion.OptionScores, OptionScore{
			OptionID: strings.TrimSpace(score.OptionID),
			Criteria: score.Criteria,
			Overall:  score.Overall,
		})
	}
	return opinion, nil
}

func (r *coordinatorDecisionRunners) RunReferenceEvidence(ctx context.Context, req ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error) {
	if err := req.Validate(); err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence request: %w", err)
	}
	if req.RoutingRole != nil {
		return r.runReferenceEvidenceViaCapabilityRouting(ctx, req)
	}
	requestBytes, err := json.Marshal(req)
	if err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence request: %w", err)
	}
	prompt := referenceEvidencePrompt(string(requestBytes))
	response, err := r.ask(ctx, decisionPurposeReferenceEvidence, prompt)
	if err != nil {
		return ReferenceEvidenceDraft{}, err
	}
	var decoded ReferenceEvidenceDraft
	if err := decodeReferenceEvidence(response, &decoded); err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence: %w", err)
	}
	if err := decoded.Validate(); err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence: %w", err)
	}
	return decoded, nil
}

func referenceEvidencePrompt(requestJSON string) string {
	return "Return exactly one JSON object matching ReferenceEvidenceDraft: " +
		`{"schema_version":1,"entries":[{"reference_class":"...","metric":"...","sample_size":1,"distribution":{"mean":0,"median":0,"p10":0,"p90":0},"limitations":[],"source":{"id":"...","source_id":"...","source_type":"...","name":"...","title":"...","publisher":"...","citation":"...","description":"...","url":"...","uri":"...","locator":"...","declared_parent_source_ids":[]}}]}. ` +
		"Evidence only: do not select options, recommend, score, or return ArtifactRef, path, media type, digest, or filesystem fields. The runtime will publish artifacts and assign identity. Request: " + requestJSON
}

// optionProposalResponse is the wire shape of the proposal stage's answer.
type optionProposalResponse struct {
	Options []struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"options"`
}

// ProposeOptions implements OptionProposer. It only carries the proposer's
// text across; ID slugging, kind validation, capping and the runtime injection
// that keeps the no-go gate meaningful all happen in NormalizeProposedOptions
// and EnsureRequiredAlternatives (spec §19.1).
func (r *coordinatorDecisionRunners) ProposeOptions(ctx context.Context, req OptionProposalRequest) ([]DecisionOption, error) {
	response, err := r.ask(ctx, decisionPurposeOptions, req.Prompt)
	if err != nil {
		return nil, err
	}
	var decoded optionProposalResponse
	if err := decodeStage(response, &decoded); err != nil {
		return nil, fmt.Errorf("option proposal: %w", err)
	}

	options := make([]DecisionOption, 0, len(decoded.Options))
	for _, option := range decoded.Options {
		options = append(options, DecisionOption{
			ID:          option.ID,
			Kind:        DecisionOptionKind(strings.TrimSpace(option.Kind)),
			Title:       option.Title,
			Description: option.Description,
		})
	}
	return options, nil
}

// challengeResponse is the wire shape of a challenger's answer.
type challengeResponse struct {
	TargetOption         string   `json:"target_option"`
	StrongestCountercase string   `json:"strongest_countercase"`
	FragileAssumptions   []string `json:"fragile_assumptions"`
	MissingEvidence      []string `json:"missing_evidence"`
	FalsificationTests   []string `json:"falsification_tests"`
	Severity             float64  `json:"severity"`
}

// RunChallenge implements ChallengeRunner.
func (r *coordinatorDecisionRunners) RunChallenge(ctx context.Context, req ChallengeRequest) (DecisionChallenge, error) {
	response, err := r.ask(ctx, decisionPurposeChallenge, req.Prompt)
	if err != nil {
		return DecisionChallenge{}, err
	}
	var decoded challengeResponse
	if err := decodeStage(response, &decoded); err != nil {
		return DecisionChallenge{}, fmt.Errorf("challenger %s: %w", req.ChallengerID, err)
	}
	return DecisionChallenge{
		TargetOption:         strings.TrimSpace(decoded.TargetOption),
		StrongestCountercase: decoded.StrongestCountercase,
		FragileAssumptions:   decoded.FragileAssumptions,
		MissingEvidence:      decoded.MissingEvidence,
		FalsificationTests:   decoded.FalsificationTests,
		Severity:             decoded.Severity,
	}, nil
}

// premortemResponse is the wire shape of a premortem's answer.
type premortemResponse struct {
	AssumedOutcome string `json:"assumed_outcome"`
	FailureModes   []struct {
		ID                  string   `json:"id"`
		Description         string   `json:"description"`
		Likelihood          float64  `json:"likelihood"`
		Impact              float64  `json:"impact"`
		EarlyWarningSignals []string `json:"early_warning_signals"`
		Mitigations         []string `json:"mitigations"`
	} `json:"failure_modes"`
}

// RunPremortem implements PremortemRunner.
func (r *coordinatorDecisionRunners) RunPremortem(ctx context.Context, req PremortemRequest) (PremortemResult, error) {
	response, err := r.ask(ctx, decisionPurposePremortem, req.Prompt)
	if err != nil {
		return PremortemResult{}, err
	}
	var decoded premortemResponse
	if err := decodeStage(response, &decoded); err != nil {
		return PremortemResult{}, fmt.Errorf("premortem: %w", err)
	}

	result := PremortemResult{AssumedOutcome: strings.TrimSpace(decoded.AssumedOutcome)}
	for _, mode := range decoded.FailureModes {
		result.FailureModes = append(result.FailureModes, FailureMode{
			ID:                  strings.TrimSpace(mode.ID),
			Description:         mode.Description,
			Likelihood:          mode.Likelihood,
			Impact:              mode.Impact,
			EarlyWarningSignals: mode.EarlyWarningSignals,
			Mitigations:         mode.Mitigations,
		})
	}
	return result, nil
}

// revisionResponse is the wire shape of one judge's revision.
type revisionResponse struct {
	RevisedScores []struct {
		OptionID string             `json:"option_id"`
		Criteria map[string]float64 `json:"criteria"`
		Overall  float64            `json:"overall"`
	} `json:"revised_scores"`
	RevisedProbability float64 `json:"revised_probability"`
	Changed            bool    `json:"changed"`
	Reason             string  `json:"reason"`
}

// RunRevision implements RevisionRunner.
func (r *coordinatorDecisionRunners) RunRevision(ctx context.Context, req RevisionRequest) (DecisionRevision, error) {
	response, err := r.ask(ctx, decisionPurposeRevision, req.Prompt)
	if err != nil {
		return DecisionRevision{}, err
	}
	var decoded revisionResponse
	if err := decodeStage(response, &decoded); err != nil {
		return DecisionRevision{}, fmt.Errorf("revision %s: %w", req.JudgeID, err)
	}

	revision := DecisionRevision{
		RevisedProbability: decoded.RevisedProbability,
		Changed:            decoded.Changed,
		Reason:             decoded.Reason,
	}
	for _, score := range decoded.RevisedScores {
		revision.RevisedScores = append(revision.RevisedScores, OptionScore{
			OptionID: strings.TrimSpace(score.OptionID),
			Criteria: score.Criteria,
			Overall:  score.Overall,
		})
	}
	return revision, nil
}

func (r *coordinatorDecisionRunners) RunCoordinatorFinalization(ctx context.Context, req CoordinatorFinalizationRequest) (FinalizationWireResult, error) {
	prompt, err := finalizationPrompt("coordinator", req.DecisionID, req.Packet, req.Aggregates, req.Challenges, req.Revisions, "")
	if err != nil {
		return FinalizationWireResult{}, err
	}
	response, err := r.ask(ctx, decisionPurposeFinalizationCoordinator, prompt)
	if err != nil {
		return FinalizationWireResult{}, err
	}
	return decodeFinalizationWireResult(response)
}

func (r *coordinatorDecisionRunners) RunJudgeFinalization(ctx context.Context, req JudgeFinalizationRequest) (FinalizationWireResult, error) {
	prompt, err := finalizationPrompt("named judge", req.DecisionID, req.Packet, req.Aggregates, req.Challenges, req.Revisions, req.JudgeID)
	if err != nil {
		return FinalizationWireResult{}, err
	}
	response, err := r.ask(ctx, decisionPurposeFinalizationJudge, prompt)
	if err != nil {
		return FinalizationWireResult{}, err
	}
	return decodeFinalizationWireResult(response)
}
