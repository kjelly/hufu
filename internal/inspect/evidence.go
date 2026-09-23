package inspect

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/auditverify"
	"github.com/kjelly/hufu/internal/team"
)

type ArtifactMetadata struct {
	ID        string `json:"id,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Kind      string `json:"kind,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Bytes     int64  `json:"bytes,omitzero"`
}

type EvidenceBindingData struct {
	RunID            string   `json:"run_id"`
	TaskID           string   `json:"task_id"`
	Attempt          int      `json:"attempt"`
	ModelExecutionID string   `json:"model_execution_id,omitempty"`
	ProducerID       string   `json:"producer_id,omitempty"`
	TranscriptRef    string   `json:"transcript_ref,omitempty"`
	ArtifactIDs      []string `json:"artifact_ids"`
}

type EvidenceRequirementData struct {
	RequirementID string               `json:"requirement_id"`
	Status        string               `json:"status"`
	Validator     string               `json:"validator,omitempty"`
	ArtifactRefs  []ArtifactMetadata   `json:"artifact_refs"`
	Binding       *EvidenceBindingData `json:"binding,omitempty"`
}

type EvidenceManifestData struct {
	Hash   string `json:"hash,omitempty"`
	Status string `json:"status"`
}

type EvidenceVerificationData struct {
	Verdict            string `json:"verdict"`
	Integrity          string `json:"integrity"`
	Provenance         string `json:"provenance"`
	Evidence           string `json:"evidence"`
	Acceptance         string `json:"acceptance"`
	SemanticRegression string `json:"semantic_regression"`
	Completion         string `json:"completion"`
}

type EvidenceFindingData struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	TaskID   string `json:"task_id,omitempty"`
	Attempt  int    `json:"attempt,omitzero"`
	Ref      string `json:"ref,omitempty"`
}

type InvariantCoverageData struct {
	TouchedPathCount         int `json:"touched_path_count"`
	ApplicableInvariantCount int `json:"applicable_invariant_count"`
	UncoveredPathCount       int `json:"uncovered_path_count"`
}

type OutcomeCoverageData struct {
	IncludedItemCount int `json:"included_item_count"`
	KnownCount        int `json:"known_count"`
	AssumedCount      int `json:"assumed_count"`
	StaleCount        int `json:"stale_count"`
	ConflictingCount  int `json:"conflicting_count,omitempty"`
}

type KnowledgeCoverageData struct {
	InvariantCoverage InvariantCoverageData `json:"invariant_coverage"`
	OutcomeCoverage   OutcomeCoverageData   `json:"outcome_coverage"`
}

type EvidenceData struct {
	RunID        string                    `json:"run_id"`
	Manifest     EvidenceManifestData      `json:"manifest"`
	Requirements []EvidenceRequirementData `json:"requirements"`
	ArtifactRefs []ArtifactMetadata        `json:"artifact_refs"`
	Verification EvidenceVerificationData  `json:"verification"`
	Acceptance   string                    `json:"acceptance"`
	Findings     []EvidenceFindingData     `json:"findings"`
}

// InspectEvidence projects the safe metadata returned by auditverify. It does
// not duplicate manifest, artifact, acceptance, or completion verification.
func InspectEvidence(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindEvidence); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	explanation, err := auditverify.ExplainLineageRun(ctx, query.Workspace, query.RunID, selected.raw)
	if err != nil {
		return nil, classifyProjectionError(query.RunID, err)
	}

	data := EvidenceData{
		RunID: query.RunID, Manifest: EvidenceManifestData{Status: "unavailable"},
		Requirements: []EvidenceRequirementData{}, ArtifactRefs: []ArtifactMetadata{},
		Verification: EvidenceVerificationData{Verdict: "unavailable", Integrity: "unavailable", Provenance: "unavailable", Evidence: "unavailable", Acceptance: "unavailable", SemanticRegression: "unavailable", Completion: "unavailable"},
		Acceptance:   "unavailable", Findings: []EvidenceFindingData{},
	}
	if explanation != nil && explanation.Verification != nil {
		verification := explanation.Verification
		data.Verification = EvidenceVerificationData{
			Verdict: string(verification.Verdict), Integrity: string(verification.Integrity.Status),
			Provenance: string(verification.Provenance.Status), Evidence: string(verification.Evidence.Status),
			Acceptance: string(verification.Acceptance.Status), SemanticRegression: string(verification.SemanticRegression.Status), Completion: string(verification.Completion.Status),
		}
		for _, finding := range verification.Findings {
			data.Findings = append(data.Findings, EvidenceFindingData{Code: finding.Code, Severity: finding.Severity, TaskID: finding.TaskID, Attempt: finding.Attempt, Ref: safeOpaqueRef(finding.Ref)})
		}
	}
	result, err := selectedRunResult(selected, query.RunID)
	if err != nil {
		return nil, err
	}
	if result != nil {
		if result.Acceptance == nil {
			data.Acceptance = string(team.AcceptanceNotConfigured)
		} else {
			data.Acceptance = string(result.Acceptance.EffectiveState())
		}
		if result.EvidenceManifest != nil {
			projectEvidenceManifest(&data, result.EvidenceManifest)
		}
	}
	return envelope(KindEvidence, query, lineage.BranchID, data), nil
}

func selectedRunResult(selected selectedRun, runID string) (*team.RunResult, error) {
	if selected.terminal == nil {
		return nil, nil
	}
	projected := team.ReduceToSessionData(selected.raw[:selected.terminalIndex+1])
	if projected == nil || projected.RunResult == nil || projected.RunResult.RunID != runID {
		return nil, fmt.Errorf("%w: terminal event %q did not reduce to run %q", ErrIntegrity, selected.terminal.ID, runID)
	}
	return projected.RunResult, nil
}

func projectEvidenceManifest(data *EvidenceData, manifest *team.EvidenceManifest) {
	data.Manifest = EvidenceManifestData{Hash: manifest.ManifestHash, Status: manifest.Status}
	for _, ref := range manifest.ArtifactRefs {
		data.ArtifactRefs = append(data.ArtifactRefs, safeArtifactMetadata(ref))
	}
	for _, result := range manifest.EvidenceResults {
		requirement := EvidenceRequirementData{RequirementID: result.RequirementID, Status: result.Status, Validator: result.Validator, ArtifactRefs: []ArtifactMetadata{}}
		for _, ref := range result.ArtifactRefs {
			requirement.ArtifactRefs = append(requirement.ArtifactRefs, safeArtifactMetadata(ref))
		}
		if result.Binding != nil {
			requirement.Binding = &EvidenceBindingData{
				RunID: result.Binding.RunID, TaskID: result.Binding.TaskID, Attempt: result.Binding.Attempt,
				ModelExecutionID: result.Binding.ModelExecutionID, ProducerID: result.Binding.ProducerID,
				TranscriptRef: safeOpaqueRef(result.Binding.TranscriptRef), ArtifactIDs: normalizeOpaqueRefs(result.Binding.ArtifactIDs),
			}
		}
		data.Requirements = append(data.Requirements, requirement)
	}
}

func safeArtifactMetadata(ref team.ArtifactRef) ArtifactMetadata {
	bytes := ref.Bytes
	if bytes == 0 {
		bytes = ref.ByteSize
	}
	return ArtifactMetadata{ID: safeOpaqueRef(ref.ID), Digest: safeOpaqueRef(ref.SHA256), Kind: ref.Kind, MediaType: ref.MediaType, Bytes: bytes}
}

func normalizeOpaqueRefs(refs []string) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = appendOpaqueRef(out, ref)
	}
	return normalizeRefs(out)
}

func projectKnowledgeCoverage(coverage *team.TaskKnowledgeCoverage) *KnowledgeCoverageData {
	if coverage == nil {
		return nil
	}
	return &KnowledgeCoverageData{
		InvariantCoverage: InvariantCoverageData{
			TouchedPathCount:         coverage.InvariantCoverage.TouchedPathCount,
			ApplicableInvariantCount: coverage.InvariantCoverage.ApplicableInvariantCount,
			UncoveredPathCount:       coverage.InvariantCoverage.UncoveredPathCount,
		},
		OutcomeCoverage: OutcomeCoverageData{
			IncludedItemCount: coverage.OutcomeCoverage.IncludedItemCount,
			KnownCount:        coverage.OutcomeCoverage.KnownCount,
			AssumedCount:      coverage.OutcomeCoverage.AssumedCount,
			StaleCount:        coverage.OutcomeCoverage.StaleCount,
			ConflictingCount:  coverage.OutcomeCoverage.ConflictingCount,
		},
	}
}
