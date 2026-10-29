package team

import (
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// DependencyResultViewSource identifies the canonical accepted source of one
// handoff. The runtime authenticates the event and its visible lineage before
// constructing a view; this attribution never grants access to that source.
type DependencyResultViewSource struct {
	BranchID           string `json:"branch_id"`
	RunID              string `json:"run_id"`
	TaskID             string `json:"task_id"`
	Attempt            int    `json:"attempt"`
	OccurrenceRevision int    `json:"occurrence_revision"`
	SourceEventID      string `json:"source_event_id"`
	SourceEventSHA256  string `json:"source_event_sha256"`
	FullViewSHA256     string `json:"full_view_sha256"`
}

// DependencyResultViewAttribution describes presentation without replacing
// canonical results, receipts, or artifact authorization.
type DependencyResultViewAttribution struct {
	Policy       DependencyResultViewSpec     `json:"policy"`
	PolicySHA256 string                       `json:"policy_sha256"`
	Sources      []DependencyResultViewSource `json:"sources"`
}

// Clone returns detached attribution for checkpoint, manifest, and inspector
// projections. None of its slices alias the original record.
func (a *DependencyResultViewAttribution) Clone() *DependencyResultViewAttribution {
	if a == nil {
		return nil
	}
	copy := *a
	copy.Policy.Fields = slices.Clone(a.Policy.Fields)
	copy.Sources = slices.Clone(a.Sources)
	return &copy
}

type dependencyResultView struct {
	content     string
	attribution DependencyResultViewAttribution
}

func dependencyViewDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func dependencyViewPolicyDigest(policy *DependencyResultViewSpec) (string, error) {
	data, err := json.Marshal(policy, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return dependencyViewDigest(data), nil
}

// buildDependencyResultView only renders already-authorized handoffs. The
// caller supplies authenticated source identities; it cannot use this pure
// formatter as an alternative source resolver or an authorization boundary.
// Full mode returns no override so callers retain FormatDependencyResults.
func buildDependencyResultView(spec *DependencyResultViewSpec, results []TaskResult, sources []DependencyResultViewSource) (*dependencyResultView, error) {
	policy, err := spec.normalized()
	if err != nil || policy == nil {
		return nil, err
	}
	if len(results) == 0 || len(results) != len(sources) {
		return nil, fmt.Errorf("selected dependency view requires every authorized typed source")
	}
	policyHash, err := dependencyViewPolicyDigest(policy)
	if err != nil {
		return nil, err
	}
	attribution := DependencyResultViewAttribution{Policy: *policy, PolicySHA256: policyHash, Sources: slices.Clone(sources)}
	var content strings.Builder
	content.WriteString("## Task Dependency Results\n\nSelected-field view; not complete upstream evidence.\n\n")
	seen := make(map[string]bool, len(results))
	for i, result := range results {
		source := &attribution.Sources[i]
		if err := validateDependencyViewSource(result, *source); err != nil {
			return nil, err
		}
		if seen[result.TaskID] {
			return nil, fmt.Errorf("selected dependency view repeats a source task")
		}
		seen[result.TaskID] = true
		fullHash := dependencyViewDigest([]byte(FormatDependencyResults([]TaskResult{result})))
		if source.FullViewSHA256 != "" && source.FullViewSHA256 != fullHash {
			return nil, fmt.Errorf("selected dependency view source handoff changed")
		}
		source.FullViewSHA256 = fullHash
		data, err := selectedDependencyFieldsJSON(result, policy.Fields)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			content.WriteString("\n\n")
		}
		fmt.Fprintf(&content, "### Task [%s] (Agent: %s, Status: %s)\n", result.TaskID, result.Agent, result.Status)
		content.WriteString(fenceUntrusted("json", string(data)))
	}
	return &dependencyResultView{content: content.String(), attribution: attribution}, nil
}

func validateDependencyViewSource(result TaskResult, source DependencyResultViewSource) error {
	if result.StructuredPayload != nil {
		return fmt.Errorf("selected dependency view cannot restrict a structured result payload")
	}
	if source.TaskID == "" || source.TaskID != result.TaskID || result.Agent == "" || !taskResultStatusIsSuccessful(result.Status) {
		return fmt.Errorf("selected dependency view requires an accepted typed task result")
	}
	if source.Attempt < 1 || source.Attempt != result.Attempt || source.OccurrenceRevision < 1 {
		return fmt.Errorf("selected dependency view source occurrence is unavailable")
	}
	if source.BranchID == "" || source.RunID == "" || source.SourceEventID == "" || !validFullContentHash(source.SourceEventSHA256) {
		return fmt.Errorf("selected dependency view source identity is unavailable")
	}
	return nil
}

type dependencyViewArtifact struct {
	ID          string `json:"id"`
	SHA256      string `json:"sha256,omitempty"`
	Bytes       int64  `json:"bytes"`
	Kind        string `json:"kind,omitempty"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
}

func selectedDependencyFieldsJSON(result TaskResult, fields []string) ([]byte, error) {
	var selected struct {
		Summary       string                   `json:"summary,omitempty"`
		Findings      []Finding                `json:"findings,omitempty"`
		Artifacts     []dependencyViewArtifact `json:"artifacts,omitempty"`
		FilesModified []FileRef                `json:"files_modified,omitempty"`
		Verification  []VerificationResult     `json:"verification,omitempty"`
	}
	for _, field := range fields {
		switch field {
		case "summary":
			selected.Summary = result.Summary
		case "findings":
			selected.Findings = result.Findings
		case "artifacts":
			for _, artifact := range result.Artifacts {
				selected.Artifacts = append(selected.Artifacts, dependencyViewArtifact{
					ID: opaqueArtifactID(artifact.ID), SHA256: artifact.SHA256, Bytes: artifact.Bytes,
					Kind: artifact.Kind, Type: artifact.Type, Description: artifact.Description,
				})
			}
		case "files_modified":
			selected.FilesModified = result.FilesModified
		case "verification":
			selected.Verification = result.Verification
		}
	}
	// Existing typed evidence uses v1 omission and duration semantics. Retain
	// those semantics rather than inventing zero/false optional diagnostics.
	return json.Marshal(selected, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
}
