package team

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/llmtimeout"
	"github.com/kjelly/hufu/internal/sidecar"
)

// Production stage runners for the decision engine
// (docs/architecture/decision-runtime.md Phase 3.5).
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

	stageCtx, cancel := context.WithTimeout(ctx, llmtimeout.Review(decisionStageTimeout))
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
	if err := validateSealedChoicePacket(req.Packet); err != nil {
		return DecisionOpinion{}, err
	}
	if decisionInferenceRepair(ctx) {
		return DecisionOpinion{}, fmt.Errorf("decision_repair_inference_denied")
	}
	var opinion DecisionOpinion
	restored, err := r.restorePrimaryGeneration(ctx, "runtime_primary_judge", req, &opinion)
	if err != nil {
		return DecisionOpinion{}, err
	}
	if !restored {
		if req.RoutingRole != nil {
			opinion, err = r.runJudgeViaCapabilityRouting(ctx, req)
		} else {
			var response string
			response, err = r.ask(ctx, decisionPurposeJudge, req.Context.Prompt)
			if err == nil {
				opinion, err = decodeJudgeOpinion(response, req.JudgeID)
			}
		}
		if err != nil {
			return DecisionOpinion{}, err
		}
	}
	opinion.PreferredOption = ""
	choice, err := r.choosePacketOption(ctx, "runtime_primary_judge", "Choose the preferred option using the sealed evidence. Generated scores and explanations are non-authoritative.", req, opinion, req.Packet)
	if err != nil {
		return DecisionOpinion{}, err
	}
	opinion.PreferredOption = choice
	return opinion, nil
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

// proposedOptionKind folds a proposer's kind spelling ("Defer",
// "reduce-scope") onto the declared snake_case kinds; anything else is left
// for NormalizeProposedOptions to classify as custom.
func proposedOptionKind(raw string) DecisionOptionKind {
	return DecisionOptionKind(strings.ReplaceAll(strings.ToLower(strings.TrimSpace(raw)), "-", "_"))
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
			Kind:        proposedOptionKind(option.Kind),
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
	if err := validateSealedChoicePacket(req.Packet); err != nil {
		return DecisionChallenge{}, err
	}
	if decisionInferenceRepair(ctx) {
		return DecisionChallenge{}, fmt.Errorf("decision_repair_inference_denied")
	}
	var challenge DecisionChallenge
	restored, err := r.restorePrimaryGeneration(ctx, "runtime_primary_challenge", req, &challenge)
	if err != nil {
		return DecisionChallenge{}, err
	}
	if !restored {
		if req.RoutingRole != nil {
			challenge, err = r.runChallengeViaCapabilityRouting(ctx, req)
		} else {
			var response string
			response, err = r.ask(ctx, decisionPurposeChallenge, req.Prompt)
			if err == nil {
				challenge, err = decodeChallengeResponse(response, req.ChallengerID)
			}
		}
		if err != nil {
			return DecisionChallenge{}, err
		}
	}
	challenge.TargetOption = ""
	choice, err := r.choosePacketOption(ctx, "runtime_primary_challenge", "Choose the option targeted by the countercase, grounded in sealed evidence. Generated text is non-authoritative.", req, challenge, req.Packet)
	if err != nil {
		return DecisionChallenge{}, err
	}
	challenge.TargetOption = choice
	return challenge, nil
}

// decodeChallengeResponse decodes one challenger's raw response into a
// DecisionChallenge. Shared by the legacy sidecar path and the
// capability-routed path so the response-shape contract never depends on
// who produced the text.
func decodeChallengeResponse(response, challengerID string) (DecisionChallenge, error) {
	var decoded challengeResponse
	if err := decodeStage(response, &decoded); err != nil {
		return DecisionChallenge{}, fmt.Errorf("challenger %s: %w", challengerID, err)
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
	if err := validateSealedChoicePacket(req.Packet); err != nil {
		return DecisionRevision{}, err
	}
	if decisionInferenceRepair(ctx) {
		return DecisionRevision{}, fmt.Errorf("decision_repair_inference_denied")
	}
	var revision DecisionRevision
	restored, err := r.restorePrimaryGeneration(ctx, "runtime_primary_revision", req, &revision)
	if err != nil {
		return DecisionRevision{}, err
	}
	if !restored {
		if req.RoutingRole != nil {
			revision, err = r.runRevisionViaCapabilityRouting(ctx, req)
		} else {
			var response string
			response, err = r.ask(ctx, decisionPurposeRevision, req.Prompt)
			if err == nil {
				revision, err = decodeRevisionResult(response, req.JudgeID)
			}
		}
		if err != nil {
			return DecisionRevision{}, err
		}
	}
	revision.Changed = false
	result, err := r.coordinator.runRuntimeChoice(r.choiceContext(ctx), r.coordinator.AgentPool().JudgeSidecar(), sidecar.DecisionInvocation{Name: "runtime_primary_revision", Question: "Does the sealed evidence and challenge justify changing the original judgment? Generated revision scores and reasons are non-authoritative.", Input: struct {
		Request                   RevisionRequest
		NonAuthoritativeGenerated DecisionRevision
	}{req, revision}, Options: []catalog.Option{{ID: "changed"}, {ID: "unchanged"}}, MaxCalls: 1000})
	if err != nil {
		return DecisionRevision{}, err
	}
	if result.Status != decisionrt.StatusDecided {
		return DecisionRevision{}, &runtimeDecisionFailure{fmt.Errorf("revision decision abstained")}
	}
	revision.Changed = result.Value.Choice == "changed"
	return revision, nil
}

// decodeRevisionResult decodes one judge's raw revision response into a
// DecisionRevision. Shared by the legacy sidecar path and the
// capability-routed path.
func decodeRevisionResult(response, judgeID string) (DecisionRevision, error) {
	var decoded revisionResponse
	if err := decodeStage(response, &decoded); err != nil {
		return DecisionRevision{}, fmt.Errorf("revision %s: %w", judgeID, err)
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
	choice, err := r.choosePacketOption(ctx, "runtime_primary_finalize_coordinator", "Choose the final option from the sealed evidence, aggregate judgments, challenges and revisions. Do not authorize execution.", req, nil, req.Packet)
	if err != nil {
		return FinalizationWireResult{}, err
	}
	return r.explainFinalChoice(ctx, decisionPurposeFinalizationCoordinator, "coordinator", req.DecisionID, "", choice, req.Packet, req.Aggregates, req.Challenges, req.Revisions)
}

func (r *coordinatorDecisionRunners) RunJudgeFinalization(ctx context.Context, req JudgeFinalizationRequest) (FinalizationWireResult, error) {
	choice, err := r.choosePacketOption(ctx, "runtime_primary_finalize_judge", "Choose the final option from the sealed evidence, aggregate judgments, challenges and revisions. Do not authorize execution.", req, nil, req.Packet)
	if err != nil {
		return FinalizationWireResult{}, err
	}
	return r.explainFinalChoice(ctx, decisionPurposeFinalizationJudge, "judge", req.DecisionID, req.JudgeID, choice, req.Packet, req.Aggregates, req.Challenges, req.Revisions)
}

func (r *coordinatorDecisionRunners) choosePacketOption(ctx context.Context, name, question string, request, generated any, packet DecisionEvidencePacket) (string, error) {
	if err := validateSealedChoicePacket(packet); err != nil {
		return "", err
	}
	if len(packet.Options) < 1 || len(packet.Options) > 21 {
		return "", fmt.Errorf("decision candidate capacity exceeded or empty")
	}
	if len(packet.Options) == 1 {
		return packet.Options[0].ID, nil
	}
	options := make([]catalog.Option, len(packet.Options))
	for i := range packet.Options {
		options[i] = catalog.Option{ID: fmt.Sprintf("option_%d", i), Description: fmt.Sprintf("Sealed option at index %d", i)}
	}
	result, err := r.coordinator.runRuntimeChoice(r.choiceContext(ctx), r.coordinator.AgentPool().JudgeSidecar(), sidecar.DecisionInvocation{Name: name, Question: question, Input: struct {
		Request                   any
		NonAuthoritativeGenerated any
		SealedPacket              DecisionEvidencePacket
	}{request, generated, packet}, Options: options, MaxCalls: 1000})
	if err != nil {
		return "", err
	}
	if result.Status != decisionrt.StatusDecided {
		return "", &runtimeDecisionFailure{fmt.Errorf("primary decision abstained")}
	}
	for i, option := range options {
		if option.ID == result.Value.Choice {
			return packet.Options[i].ID, nil
		}
	}
	return "", fmt.Errorf("unknown primary decision option")
}

// A divergent selection requires a separately billed explanation, not a fabricated rationale.
func (r *coordinatorDecisionRunners) explainFinalChoice(ctx context.Context, purpose, kind, id, judge, choice string, packet DecisionEvidencePacket, aggregates []DecisionAggregate, challenges []DecisionChallenge, revisions []DecisionRevision) (FinalizationWireResult, error) {
	result := FinalizationWireResult{OptionID: choice}
	if len(aggregates) > 0 && aggregates[len(aggregates)-1].PreferredOption == choice {
		return result, nil
	}
	if decisionInferenceRepair(ctx) {
		return result, fmt.Errorf("decision_repair_inference_denied")
	}
	prompt, err := finalizationPrompt(kind, id, packet, aggregates, challenges, revisions, judge)
	if err != nil {
		return result, err
	}
	prompt += "\nThe runtime has already selected option " + choice + ". Explain that selection using evidence; do not select or change the option. Return only a JSON object with a reason string."
	if exceeded, _ := r.coordinator.budgetExceeded(); exceeded {
		return result, fmt.Errorf("decision_budget_exceeded")
	}
	response, err := r.ask(ctx, purpose, prompt)
	if err != nil {
		return result, err
	}
	var explanation struct {
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(response))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&explanation); err != nil {
		return result, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return result, fmt.Errorf("finalization explanation contains trailing data")
	}
	if strings.TrimSpace(explanation.Reason) == "" {
		return result, fmt.Errorf("finalization explanation is empty")
	}
	result.Reason = "Non-authoritative explanation: " + explanation.Reason
	return result, nil
}

func (r *coordinatorDecisionRunners) choiceContext(ctx context.Context) context.Context {
	metadata, _ := invocationMetadataFromContext(ctx)
	metadata.TaskID = r.todoID
	if metadata.RunID == "" {
		metadata.RunID = r.coordinator.executionRunID
	}
	if metadata.AgentName == "" {
		metadata.AgentName = "coordinator"
	}
	if metadata.Attempt < 1 {
		metadata.Attempt = 1
	}
	return withInvocationMetadata(ctx, metadata)
}

func validateSealedChoicePacket(packet DecisionEvidencePacket) error {
	if !packet.Sealed || packet.Hash == "" {
		return fmt.Errorf("decision evidence is not sealed")
	}
	if err := packet.Validate(); err != nil {
		return err
	}
	hash, err := packet.EvidenceHash()
	if err != nil || hash != packet.Hash {
		return fmt.Errorf("decision evidence hash mismatch")
	}
	return nil
}
