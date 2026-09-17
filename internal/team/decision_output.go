package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

const DecisionOutputRedactionPolicyV1 = "hufu-secret-redaction@v1"

type DecisionOutputTeamV1 struct {
	ID               string `json:"id"`
	DefinitionDigest string `json:"definition_digest"`
}

type DecisionOutputProfileV1 struct {
	Requested    string `json:"requested"`
	ResolvedRef  string `json:"resolved_ref"`
	Origin       string `json:"origin"`
	Version      string `json:"version"`
	PolicyDigest string `json:"policy_digest"`
	BundleDigest string `json:"bundle_digest"`
}

type DecisionOutputPrimaryV1 struct {
	State              string                `json:"state"`
	TaskID             *string               `json:"task_id"`
	Generation         uint64                `json:"generation"`
	DecisionID         *string               `json:"decision_id"`
	BindingEventID     *string               `json:"binding_event_id"`
	RecordRef          *DecisionArtifactRef  `json:"record_ref"`
	BaseEvidenceRef    *DecisionArtifactRef  `json:"base_evidence_ref"`
	SealedEvidenceHash *string               `json:"sealed_evidence_hash"`
	View               *DecisionRecordViewV1 `json:"view"`
}

type DecisionOutputAcceptanceV1 struct {
	DecisionProcess string `json:"decision_process"`
	Team            string `json:"team"`
	Combined        string `json:"combined"`
}

type DecisionOutputCoverageReasonsV1 struct {
	Duplicate        uint64 `json:"duplicate"`
	OutOfScope       uint64 `json:"out_of_scope"`
	Superseded       uint64 `json:"superseded"`
	RedactedUnusable uint64 `json:"redacted_unusable"`
	OptionalOversize uint64 `json:"optional_oversize"`
	Budget           uint64 `json:"budget"`
	UnsupportedMedia uint64 `json:"unsupported_media"`
}

type DecisionOutputCoverageV1 struct {
	InventoryRef              DecisionArtifactRef             `json:"inventory_ref"`
	InventoryCount            uint64                          `json:"inventory_count"`
	SelectedCount             uint64                          `json:"selected_count"`
	ExcludedCount             uint64                          `json:"excluded_count"`
	ExcludedReasonCounts      DecisionOutputCoverageReasonsV1 `json:"excluded_reason_counts"`
	MandatorySelectedCount    uint64                          `json:"mandatory_selected_count"`
	KnownIndependentGroups    uint64                          `json:"known_independent_groups"`
	UnresolvedProvenanceCount uint64                          `json:"unresolved_provenance_count"`
}

type DecisionOutputBudgetV1 struct {
	UsedTokens     uint64 `json:"used_tokens"`
	ReservedTokens uint64 `json:"reserved_tokens"`
	UsedDurationMS uint64 `json:"used_duration_ms"`
}

type DecisionOutputMessageV1 struct {
	Code      string  `json:"code"`
	Message   string  `json:"message"`
	Retryable bool    `json:"retryable"`
	FieldPath *string `json:"field_path"`
}

type DecisionOutputRedactionV1 struct {
	Applied             bool   `json:"applied"`
	PolicyVersion       string `json:"policy_version"`
	RecordViewTruncated bool   `json:"record_view_truncated"`
}

type DecisionOutputContinuationV1 struct {
	ResumeLogicalRunID string `json:"resume_logical_run_id"`
	RequiresOperator   bool   `json:"requires_operator"`
}

type DecisionRecordOptionViewV1 struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Title  string `json:"title"`
	Origin string `json:"origin"`
}

type DecisionRecordCommentV1 struct {
	SubjectID       string   `json:"subject_id"`
	Text            string   `json:"text"`
	EvidenceItemIDs []string `json:"evidence_item_ids"`
}

type DecisionRecordViewV1 struct {
	RecordSchemaVersion     int                          `json:"record_schema_version"`
	SelectedOptionID        string                       `json:"selected_option_id"`
	Options                 []DecisionRecordOptionViewV1 `json:"options"`
	ForecastPPM             *uint64                      `json:"forecast_ppm"`
	ForecastIsCalibrated    bool                         `json:"forecast_is_calibrated"`
	Rationale               []DecisionRecordCommentV1    `json:"rationale"`
	Dissent                 []DecisionRecordCommentV1    `json:"dissent"`
	CriticalAssumptions     []DecisionRecordCommentV1    `json:"critical_assumptions"`
	MissingInformation      []string                     `json:"missing_information"`
	PremortemFindings       []string                     `json:"premortem_findings"`
	FalsificationConditions []string                     `json:"falsification_conditions"`
	Limitations             []string                     `json:"limitations"`
}

// DecisionOutputV1 is the only public decision-intent result shape. Pointer
// fields deliberately omit omitempty so unavailable values remain explicit
// JSON nulls instead of becoming ambiguous absent fields.
type DecisionOutputV1 struct {
	SchemaVersion      int                           `json:"schema_version"`
	Kind               string                        `json:"kind"`
	LogicalRunID       *string                       `json:"logical_run_id"`
	ExecutionRunID     *string                       `json:"execution_run_id"`
	BranchID           *string                       `json:"branch_id"`
	Team               *DecisionOutputTeamV1         `json:"team"`
	Intent             string                        `json:"intent"`
	TerminalPersisted  bool                          `json:"terminal_persisted"`
	LogicalDisposition string                        `json:"logical_disposition"`
	Outcome            string                        `json:"outcome"`
	ExitCode           int                           `json:"exit_code"`
	GoalSatisfied      bool                          `json:"goal_satisfied"`
	Profile            *DecisionOutputProfileV1      `json:"profile"`
	Primary            DecisionOutputPrimaryV1       `json:"primary"`
	Acceptance         DecisionOutputAcceptanceV1    `json:"acceptance"`
	Coverage           *DecisionOutputCoverageV1     `json:"coverage"`
	Budget             DecisionOutputBudgetV1        `json:"budget"`
	Reasons            []DecisionOutputMessageV1     `json:"reasons"`
	Warnings           []DecisionOutputMessageV1     `json:"warnings"`
	Redaction          DecisionOutputRedactionV1     `json:"redaction"`
	Continuation       *DecisionOutputContinuationV1 `json:"continuation"`
}

func NewDecisionOutputV1(kind string) DecisionOutputV1 {
	return DecisionOutputV1{
		SchemaVersion: 1, Kind: kind, Intent: "decision", LogicalDisposition: "not_created",
		Outcome: "failed", ExitCode: 2, Primary: DecisionOutputPrimaryV1{State: "missing"},
		Acceptance: DecisionOutputAcceptanceV1{DecisionProcess: "not_evaluated", Team: "not_evaluated", Combined: "not_evaluated"},
		Reasons:    []DecisionOutputMessageV1{}, Warnings: []DecisionOutputMessageV1{},
		Redaction: DecisionOutputRedactionV1{PolicyVersion: DecisionOutputRedactionPolicyV1},
	}
}

func (value DecisionOutputV1) Validate() error {
	if value.SchemaVersion != 1 || value.Intent != "decision" || value.Kind != "decision_run_result" && value.Kind != "decision_run_preview" {
		return fmt.Errorf("invalid decision output envelope")
	}
	if !stringIn(value.LogicalDisposition, "not_created", "active", "suspended", "closed") ||
		!stringIn(value.Outcome, "preview", "completed", "unverified", "partial", "blocked", "failed", "cancelled", "stalled", "recovery_required") ||
		!intIn(value.ExitCode, 0, 1, 2, 7, 124, 130, 143) {
		return fmt.Errorf("invalid decision output disposition, outcome, or exit code")
	}
	if err := validateDecisionOutputIdentity(value); err != nil {
		return err
	}
	if err := validateDecisionOutputPrimary(value.Primary); err != nil {
		return err
	}
	if err := validateDecisionOutputAcceptance(value.Acceptance); err != nil {
		return err
	}
	if err := validateDecisionOutputMessages(value); err != nil {
		return err
	}
	if value.Primary.View != nil {
		if value.Primary.State != "bound" {
			return fmt.Errorf("only a bound primary may expose a record view")
		}
		if err := value.Primary.View.Validate(); err != nil {
			return err
		}
	}
	if err := validateDecisionOutputCoverage(value.Coverage); err != nil {
		return err
	}
	if value.Continuation != nil && !decisionLogicalIDPattern.MatchString(value.Continuation.ResumeLogicalRunID) {
		return fmt.Errorf("invalid decision continuation")
	}
	if value.Outcome == "completed" {
		if !value.TerminalPersisted || value.LogicalDisposition != "closed" || !value.GoalSatisfied || value.ExitCode != 0 || value.Primary.State != "bound" ||
			value.Primary.RecordRef == nil || value.Primary.BindingEventID == nil || value.Acceptance.Combined != "passed" || value.Acceptance.DecisionProcess != "passed" {
			return fmt.Errorf("completed decision output violates success invariant")
		}
	}
	if value.Kind == "decision_run_preview" && (value.Outcome != "preview" || value.TerminalPersisted || value.LogicalRunID != nil) {
		return fmt.Errorf("decision preview must not create or persist a logical run")
	}
	return nil
}

func validateDecisionOutputIdentity(value DecisionOutputV1) error {
	for _, id := range []*string{value.LogicalRunID, value.ExecutionRunID, value.BranchID, value.Primary.TaskID, value.Primary.DecisionID, value.Primary.BindingEventID} {
		if id != nil && !validDecisionIdentifier(*id) {
			return fmt.Errorf("invalid decision output identifier %q", *id)
		}
	}
	if value.LogicalRunID != nil && !decisionLogicalIDPattern.MatchString(*value.LogicalRunID) {
		return fmt.Errorf("invalid decision output logical run id")
	}
	if value.Team != nil && (!validDecisionIdentifier(value.Team.ID) || !validDecisionDigest(value.Team.DefinitionDigest)) {
		return fmt.Errorf("invalid decision output team identity")
	}
	if value.Profile == nil {
		return nil
	}
	profile := value.Profile
	if profile.Requested == "" || len(profile.Requested) > 160 || profile.ResolvedRef == "" || len(profile.ResolvedRef) > 160 || profile.Version == "" || len(profile.Version) > 32 ||
		!stringIn(profile.Origin, "builtin", "team-inline", "legacy-inline") || !validDecisionDigest(profile.PolicyDigest) || !validDecisionDigest(profile.BundleDigest) {
		return fmt.Errorf("invalid decision output profile")
	}
	return nil
}

func validateDecisionOutputPrimary(value DecisionOutputPrimaryV1) error {
	if !stringIn(value.State, "missing", "in_progress", "blocked", "bound", "invalidated") {
		return fmt.Errorf("invalid decision output primary state")
	}
	for _, ref := range []*DecisionArtifactRef{value.RecordRef, value.BaseEvidenceRef} {
		if ref != nil {
			if err := validateDecisionArtifactRef(*ref); err != nil {
				return err
			}
		}
	}
	if value.SealedEvidenceHash != nil && !validDecisionDigest(*value.SealedEvidenceHash) {
		return fmt.Errorf("invalid sealed evidence hash")
	}
	return nil
}

func validateDecisionOutputMessages(value DecisionOutputV1) error {
	if len(value.Reasons) > 128 || len(value.Warnings) > 128 || value.Redaction.PolicyVersion == "" || len(value.Redaction.PolicyVersion) > 128 {
		return fmt.Errorf("invalid decision output messages or redaction")
	}
	for _, message := range append(slices.Clone(value.Reasons), value.Warnings...) {
		if !validDecisionIdentifier(message.Code) || len(message.Message) > 4096 || message.FieldPath != nil && len(*message.FieldPath) > 512 {
			return fmt.Errorf("invalid decision output message")
		}
	}
	return nil
}

func validateDecisionOutputCoverage(value *DecisionOutputCoverageV1) error {
	if value == nil {
		return nil
	}
	if err := validateDecisionArtifactRef(value.InventoryRef); err != nil {
		return fmt.Errorf("invalid decision output coverage: %w", err)
	}
	if value.SelectedCount+value.ExcludedCount != value.InventoryCount || value.MandatorySelectedCount > value.SelectedCount || value.KnownIndependentGroups > value.SelectedCount || value.UnresolvedProvenanceCount > value.SelectedCount {
		return fmt.Errorf("invalid decision output coverage counts")
	}
	excluded := value.ExcludedReasonCounts
	if excluded.Duplicate+excluded.OutOfScope+excluded.Superseded+excluded.RedactedUnusable+excluded.OptionalOversize+excluded.Budget+excluded.UnsupportedMedia != value.ExcludedCount {
		return fmt.Errorf("invalid decision output excluded reason counts")
	}
	return nil
}

func validateDecisionOutputAcceptance(value DecisionOutputAcceptanceV1) error {
	if !stringIn(value.DecisionProcess, "passed", "failed", "not_evaluated") || !stringIn(value.Team, "passed", "failed", "not_configured", "not_evaluated") || !stringIn(value.Combined, "passed", "failed", "not_evaluated") {
		return fmt.Errorf("invalid decision output acceptance")
	}
	return nil
}

func (value DecisionRecordViewV1) Validate() error {
	if value.RecordSchemaVersion < 1 || !validDecisionIdentifier(value.SelectedOptionID) || len(value.Options) == 0 || len(value.Options) > 16 || value.ForecastIsCalibrated {
		return fmt.Errorf("invalid decision record view")
	}
	selected := false
	for _, option := range value.Options {
		if !validDecisionIdentifier(option.ID) || !stringIn(option.Kind, "execute", "defer", "negotiate", "request_information", "reduce_scope", "abandon", "custom") || !stringIn(option.Origin, "declared", "proposed", "runtime") || len(option.Title) > 4096 {
			return fmt.Errorf("invalid decision record option view")
		}
		selected = selected || option.ID == value.SelectedOptionID
	}
	if !selected || value.ForecastPPM != nil && *value.ForecastPPM > 1_000_000 {
		return fmt.Errorf("invalid selected option or forecast")
	}
	for _, values := range [][]DecisionRecordCommentV1{value.Rationale, value.Dissent, value.CriticalAssumptions} {
		if len(values) > 64 {
			return fmt.Errorf("decision record comments exceed limit")
		}
		for _, comment := range values {
			if !validDecisionIdentifier(comment.SubjectID) || len(comment.Text) > 8192 || len(comment.EvidenceItemIDs) > 128 {
				return fmt.Errorf("invalid decision record comment")
			}
		}
	}
	for _, values := range [][]string{value.MissingInformation, value.PremortemFindings, value.FalsificationConditions, value.Limitations} {
		if len(values) > 128 {
			return fmt.Errorf("decision record string list exceeds limit")
		}
		for _, item := range values {
			if len(item) > 4096 {
				return fmt.Errorf("decision record string exceeds limit")
			}
		}
	}
	return nil
}

// DecisionRecordView projects only the bounded, shareable fields declared by
// DecisionOutputV1. It never serializes the complete internal record.
func DecisionRecordView(record DecisionRecord) (*DecisionRecordViewV1, bool, error) {
	view := &DecisionRecordViewV1{
		RecordSchemaVersion: record.SchemaVersion, SelectedOptionID: record.FinalOption,
		Options: []DecisionRecordOptionViewV1{}, Rationale: []DecisionRecordCommentV1{}, Dissent: []DecisionRecordCommentV1{}, CriticalAssumptions: []DecisionRecordCommentV1{},
		MissingInformation: []string{}, PremortemFindings: []string{}, FalsificationConditions: []string{}, Limitations: []string{},
	}
	truncated := false
	for index, option := range record.Options {
		if index == 16 {
			truncated = true
			break
		}
		view.Options = append(view.Options, DecisionRecordOptionViewV1{ID: option.ID, Kind: string(option.Kind), Title: boundedDecisionText(option.Title, 4096, &truncated), Origin: string(option.EffectiveOrigin())})
	}
	if forecast, ok := decisionForecast(record); ok {
		if math.IsNaN(forecast) || math.IsInf(forecast, 0) || forecast < 0 || forecast > 1 {
			return nil, false, fmt.Errorf("decision forecast is outside 0..1")
		}
		ppm := uint64(math.RoundToEven(forecast * 1_000_000))
		view.ForecastPPM = &ppm
	}
	for _, opinion := range record.Opinions {
		if !opinion.Valid {
			continue
		}
		text := strings.TrimSpace(opinion.PreferredOption)
		if text != "" {
			appendDecisionComment(&view.Rationale, DecisionRecordCommentV1{SubjectID: opinion.ID, Text: text, EvidenceItemIDs: []string{}}, &truncated)
		}
		appendDecisionStrings(&view.MissingInformation, opinion.MissingInformation, &truncated)
	}
	for _, challenge := range record.Challenges {
		if strings.TrimSpace(challenge.StrongestCountercase) != "" {
			appendDecisionComment(&view.Dissent, DecisionRecordCommentV1{SubjectID: challenge.ID, Text: challenge.StrongestCountercase, EvidenceItemIDs: []string{}}, &truncated)
		}
		appendDecisionStrings(&view.MissingInformation, challenge.MissingEvidence, &truncated)
		appendDecisionStrings(&view.FalsificationConditions, challenge.FalsificationTests, &truncated)
	}
	for _, assumption := range record.Assumptions {
		if assumption.Critical {
			appendDecisionComment(&view.CriticalAssumptions, DecisionRecordCommentV1{SubjectID: assumption.ID, Text: assumption.Statement, EvidenceItemIDs: artifactIDs(assumption.EvidenceRefs)}, &truncated)
		}
	}
	if record.Premortem != nil {
		for _, failure := range record.Premortem.FailureModes {
			appendDecisionStrings(&view.PremortemFindings, []string{failure.Description}, &truncated)
		}
	}
	appendDecisionStrings(&view.FalsificationConditions, record.FalsificationConditions, &truncated)
	appendDecisionStrings(&view.Limitations, record.FinalizationWarnings, &truncated)
	appendDecisionStrings(&view.Limitations, record.SharedOriginWarnings, &truncated)
	if err := view.Validate(); err != nil {
		return nil, false, err
	}
	return view, truncated, nil
}

// RedactDecisionOutput applies the process redactor to content fields before
// validating the public DTO. Numeric telemetry remains typed by the redactor's
// explicit allowlist.
func RedactDecisionOutput(value DecisionOutputV1) (DecisionOutputV1, error) {
	value.Redaction.PolicyVersion = DecisionOutputRedactionPolicyV1
	value.Redaction.Applied = false
	original, err := json.Marshal(value)
	if err != nil {
		return DecisionOutputV1{}, err
	}
	redacted, err := utils.RedactJSONCompact(original)
	if err != nil {
		return DecisionOutputV1{}, err
	}
	if err := json.Unmarshal(redacted, &value); err != nil {
		return DecisionOutputV1{}, fmt.Errorf("decode redacted decision output: %w", err)
	}
	value.Redaction.Applied = !bytes.Equal(original, redacted)
	if err := value.Validate(); err != nil {
		return DecisionOutputV1{}, err
	}
	return value, nil
}

func decisionForecast(record DecisionRecord) (float64, bool) {
	for _, aggregate := range record.Aggregates {
		if value, ok := aggregate.MeanProbability[record.FinalOption]; ok {
			return value, true
		}
	}
	for _, opinion := range record.Opinions {
		if opinion.Valid && opinion.PreferredOption == record.FinalOption {
			return record.Probability, true
		}
	}
	return 0, false
}

func appendDecisionComment(target *[]DecisionRecordCommentV1, value DecisionRecordCommentV1, truncated *bool) {
	if len(*target) >= 64 {
		*truncated = true
		return
	}
	value.Text = boundedDecisionText(value.Text, 8192, truncated)
	if len(value.EvidenceItemIDs) > 128 {
		value.EvidenceItemIDs = slices.Clone(value.EvidenceItemIDs[:128])
		*truncated = true
	}
	*target = append(*target, value)
}

func appendDecisionStrings(target *[]string, values []string, truncated *bool) {
	for _, value := range values {
		if len(*target) >= 128 {
			*truncated = true
			return
		}
		*target = append(*target, boundedDecisionText(value, 4096, truncated))
	}
}

func boundedDecisionText(value string, maxRunes int, truncated *bool) string {
	runes := []rune(utils.RedactSecrets(value))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	*truncated = true
	return string(runes[:maxRunes])
}

func artifactIDs(refs []ArtifactRef) []string {
	ids := make([]string, 0, min(len(refs), 128))
	for _, ref := range refs {
		if len(ids) == 128 {
			break
		}
		ids = append(ids, ref.ID)
	}
	return ids
}

func stringIn(value string, allowed ...string) bool { return slices.Contains(allowed, value) }
func intIn(value int, allowed ...int) bool          { return slices.Contains(allowed, value) }
