package team

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/utils"
)

// WorkerResultProposal is the untrusted, schema-limited final response an
// external SubagentProvider (Codex) returns for one attempt
// (docs/hufu-external-coding-agent-runtime-spec.md §9.1). It has no field for
// TaskID/RunID/Attempt/Agent/Provider/hashes/receipt IDs/verification: a
// provider cannot populate trusted TaskResult state, only propose one.
//
// Finding and Risk are the same types canonical TaskResult already uses
// (task_result.go) — reused deliberately, not redeclared, per this
// specification's own §9.1 note.
type WorkerResultProposal struct {
	Status        string         `json:"status"`
	Summary       string         `json:"summary"`
	Details       string         `json:"details,omitempty"`
	ProposedFiles []ProposedFile `json:"proposed_files,omitempty"`
	Findings      []Finding      `json:"findings,omitempty"`
	Risks         []Risk         `json:"risks,omitempty"`
	OpenQuestions []string       `json:"open_questions,omitempty"`
	Facts         map[string]any `json:"facts,omitempty"`
	Confidence    float64        `json:"confidence,omitempty"`
}

// ProposedFile names one file the provider believes it produced or relied on.
// Path is resolved and verified against the actual observed workspace delta
// before it becomes a trusted ArtifactRef; it is never trusted as-is.
type ProposedFile struct {
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
	Role        string `json:"role,omitempty"`
}

// Bounds applied when a proposal is canonicalized (§9.3 rule 9: "attach
// proposal findings/risks/facts only after size/schema limits"). An
// untrusted provider process must never be able to inflate canonical
// TaskResult storage unboundedly.
const (
	workerResultProposalMaxFindings      = 50
	workerResultProposalMaxRisks         = 50
	workerResultProposalMaxOpenQuestions = 50
	workerResultProposalMaxTextRunes     = 4000
	workerResultProposalMaxFactsBytes    = 16 * 1024
)

func validWorkerResultProposalStatus(status string) bool {
	switch status {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		return true
	default:
		return false
	}
}

// DecodeWorkerResultProposal strictly decodes an external provider's raw
// final JSON response. DisallowUnknownFields means any field this schema
// does not declare — including every trusted field the proposal type
// deliberately omits — fails the decode instead of being silently dropped
// (§9.1: "Any such fields supplied by the provider MUST cause strict schema
// decode failure").
func DecodeWorkerResultProposal(data []byte) (*WorkerResultProposal, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var proposal WorkerResultProposal
	if err := dec.Decode(&proposal); err != nil {
		return nil, fmt.Errorf("decode worker result proposal: %w", err)
	}
	if strings.TrimSpace(proposal.Status) == "" {
		return nil, fmt.Errorf("decode worker result proposal: status is required")
	}
	if strings.TrimSpace(proposal.Summary) == "" {
		return nil, fmt.Errorf("decode worker result proposal: summary is required")
	}
	if !validWorkerResultProposalStatus(proposal.Status) {
		return nil, fmt.Errorf("decode worker result proposal: unknown status %q", proposal.Status)
	}
	return &proposal, nil
}

// ExternalProviderProposalSource marks a canonical TaskResult built from an
// untrusted external provider proposal (§9.3 rule 10, §19).
const ExternalProviderProposalSource = "external_provider_proposal"

// ExternalResultCanonicalizer turns an external provider's untrusted
// AttemptResult into a Hufu-owned canonical TaskResult
// (docs/hufu-external-coding-agent-runtime-spec.md §9.3, §19).
//
// The workspaceRoot parameter is not in the specification's original
// signature. It exists because distinguishing "proposed file was not part of
// this attempt's delta but still exists unchanged" from "does not exist at
// all" (§9.4's provider_claimed_unchanged_file vs. provider_claimed_missing_file)
// requires checking the live workspace, and AttemptRequest carries no root
// yet — that arrives with *PreparedExecutionWorld in the ExecutionWorld phase
// (§10, PR-06). When it lands, pass request.ExecutionWorld.Root here instead
// and drop this parameter; it is not a second, permanent design.
type ExternalResultCanonicalizer interface {
	Canonicalize(ctx context.Context, request AttemptRequest, result AttemptResult, delta WorkspaceDelta, workspaceRoot string) (*TaskResult, error)
}

type defaultExternalResultCanonicalizer struct{}

// NewExternalResultCanonicalizer returns the default ExternalResultCanonicalizer.
func NewExternalResultCanonicalizer() ExternalResultCanonicalizer {
	return defaultExternalResultCanonicalizer{}
}

func (defaultExternalResultCanonicalizer) Canonicalize(_ context.Context, request AttemptRequest, result AttemptResult, delta WorkspaceDelta, workspaceRoot string) (*TaskResult, error) {
	if strings.TrimSpace(request.Provider) == "" {
		return nil, fmt.Errorf("canonicalize external result: attempt request has no provider binding")
	}
	proposal := result.ResultProposal
	if proposal == nil {
		return nil, fmt.Errorf("canonicalize external result: provider result proposal is missing")
	}
	if !validWorkerResultProposalStatus(proposal.Status) {
		return nil, fmt.Errorf("canonicalize external result: unknown proposal status %q", proposal.Status)
	}

	requiresGrounded := request.Task.Execution.RequiresGroundedResult

	addedOrModified := make(map[string]WorkspaceFileState, len(delta.Added)+len(delta.Modified))
	for _, f := range delta.Added {
		addedOrModified[f.Path] = f
	}
	for _, f := range delta.Modified {
		addedOrModified[f.Path] = f
	}
	deleted := make(map[string]bool, len(delta.Deleted))
	for _, p := range delta.Deleted {
		deleted[p] = true
	}
	mentioned := make(map[string]bool, len(proposal.ProposedFiles))

	var artifacts []ArtifactRef
	for _, proposed := range proposal.ProposedFiles {
		cleanPath, pathErr := sanitizeWorkspaceRelativePath(proposed.Path)
		if pathErr != nil {
			// provider_claimed_outside_workspace (§9.4).
			if requiresGrounded {
				return nil, fmt.Errorf("canonicalize external result: proposed artifact %q resolves outside the authorized workspace: %w", proposed.Path, pathErr)
			}
			continue
		}
		mentioned[cleanPath] = true

		if state, ok := addedOrModified[cleanPath]; ok {
			artifacts = append(artifacts, canonicalArtifactRef(request, proposed, cleanPath, state.SHA256, state.Bytes))
			continue
		}
		if deleted[cleanPath] {
			// provider_claimed_missing_file: the file existed before this
			// attempt but does not exist after it (§9.4).
			if requiresGrounded {
				return nil, fmt.Errorf("canonicalize external result: proposed artifact %q was deleted during this attempt", cleanPath)
			}
			continue
		}
		// Not part of the observed delta at all. It MAY be a legitimate
		// reference to a pre-existing, untouched artifact
		// (provider_claimed_unchanged_file) rather than a fabrication —
		// check the live workspace before deciding which.
		hash, size, statErr := hashWorkspaceFile(workspaceRoot, cleanPath)
		if statErr != nil {
			// provider_claimed_missing_file: never existed at all.
			if requiresGrounded {
				return nil, fmt.Errorf("canonicalize external result: proposed artifact %q does not exist: %w", cleanPath, statErr)
			}
			continue
		}
		artifacts = append(artifacts, canonicalArtifactRef(request, proposed, cleanPath, hash, size))
	}

	// FilesModified always comes from actual delta (§19), independent of
	// whether the provider mentioned each path (provider_omitted_modified_file
	// is recorded from Hufu truth regardless, §9.4).
	filesModified := make([]FileRef, 0, len(addedOrModified))
	for path := range addedOrModified {
		filesModified = append(filesModified, FileRef{Path: path})
	}
	sort.Slice(filesModified, func(i, j int) bool { return filesModified[i].Path < filesModified[j].Path })
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })

	return &TaskResult{
		TaskID:  request.TaskID,
		Attempt: request.Attempt,
		Agent:   canonicalAttemptAgentName(request),
		Status:  proposal.Status,
		Summary: utils.TruncateRunes(proposal.Summary, workerResultProposalMaxTextRunes),
		Details: utils.TruncateRunes(proposal.Details, workerResultProposalMaxTextRunes),

		Artifacts:     artifacts,
		FilesModified: filesModified,

		Findings:      boundedFindings(proposal.Findings),
		Risks:         boundedRisks(proposal.Risks),
		OpenQuestions: boundedOpenQuestions(proposal.OpenQuestions),
		Facts:         boundedFacts(proposal.Facts),
		Confidence:    proposal.Confidence,

		// Evidence, ReceiptIDs and Verification remain unset here: they are
		// populated only later by Hufu-owned receipt/verification logic (§19).
		Source: ExternalProviderProposalSource,
	}, nil
}

func canonicalArtifactRef(request AttemptRequest, proposed ProposedFile, cleanPath, sha256Hex string, bytes int64) ArtifactRef {
	return ArtifactRef{
		Path: cleanPath, Description: utils.TruncateRunes(proposed.Description, workerResultProposalMaxTextRunes), Role: proposed.Role,
		SHA256: sha256Hex, Bytes: bytes,
		RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt,
		Provider: request.Provider,
	}
}

func canonicalAttemptAgentName(request AttemptRequest) string {
	if request.Agent != nil && strings.TrimSpace(request.Agent.Name) != "" {
		return request.Agent.Name
	}
	return request.Task.Agent
}

// sanitizeWorkspaceRelativePath rejects an absolute path or one that escapes
// the workspace root via ".." segments, and normalizes the rest to a
// forward-slash relative path matching WorkspaceFileState.Path (§SEC-09:
// "Provider output paths MUST be canonicalized and root-checked").
func sanitizeWorkspaceRelativePath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("empty path")
	}
	if filepath.IsAbs(trimmed) {
		return "", fmt.Errorf("absolute path %q is not authorized", trimmed)
	}
	cleaned := filepath.Clean(filepath.FromSlash(trimmed))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the authorized workspace", trimmed)
	}
	return filepath.ToSlash(cleaned), nil
}

func boundedFindings(in []Finding) []Finding {
	if len(in) == 0 {
		return nil
	}
	if len(in) > workerResultProposalMaxFindings {
		in = in[:workerResultProposalMaxFindings]
	}
	out := make([]Finding, len(in))
	for i, f := range in {
		out[i] = Finding{
			Category: utils.TruncateRunes(f.Category, workerResultProposalMaxTextRunes),
			Summary:  utils.TruncateRunes(f.Summary, workerResultProposalMaxTextRunes),
			Detail:   utils.TruncateRunes(f.Detail, workerResultProposalMaxTextRunes),
		}
	}
	return out
}

func boundedRisks(in []Risk) []Risk {
	if len(in) == 0 {
		return nil
	}
	if len(in) > workerResultProposalMaxRisks {
		in = in[:workerResultProposalMaxRisks]
	}
	out := make([]Risk, len(in))
	for i, r := range in {
		out[i] = Risk{
			Description: utils.TruncateRunes(r.Description, workerResultProposalMaxTextRunes),
			Impact:      utils.TruncateRunes(r.Impact, workerResultProposalMaxTextRunes),
			Mitigation:  utils.TruncateRunes(r.Mitigation, workerResultProposalMaxTextRunes),
		}
	}
	return out
}

func boundedOpenQuestions(in []string) OpenQuestions {
	if len(in) == 0 {
		return nil
	}
	if len(in) > workerResultProposalMaxOpenQuestions {
		in = in[:workerResultProposalMaxOpenQuestions]
	}
	out := make(OpenQuestions, len(in))
	for i, q := range in {
		out[i] = utils.TruncateRunes(q, workerResultProposalMaxTextRunes)
	}
	return out
}

// boundedFacts drops the entire Facts map rather than partially truncating
// it when its encoded size exceeds the bound: a partially-truncated map is
// not obviously safer, just differently lossy, and a caller cannot tell
// which is which without the honest signal that the whole thing was refused.
func boundedFacts(facts map[string]any) map[string]any {
	if len(facts) == 0 {
		return nil
	}
	encoded, err := json.Marshal(facts)
	if err != nil || len(encoded) > workerResultProposalMaxFactsBytes {
		return nil
	}
	return facts
}
