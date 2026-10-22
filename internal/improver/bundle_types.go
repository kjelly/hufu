package improver

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
)

const (
	bundleSchemaVersion = 1
	maxBundleObjects    = 100000
	maxBundleBytes      = 512 << 20
)

type BundleFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

type BundleLesson struct {
	Status      string                 `json:"status"`
	Reason      string                 `json:"reason_code"`
	Synthetic   bool                   `json:"synthetic"`
	Lesson      *paired.ContrastLesson `json:"lesson,omitzero"`
	EvidenceRef *paired.ArtifactRef    `json:"evidence_ref,omitzero"`
}

type BundleTiming struct {
	StageID        string    `json:"stage_id"`
	Generation     int64     `json:"generation"`
	AdmittedAt     time.Time `json:"admitted_at"`
	SettledAt      time.Time `json:"settled_at"`
	ElapsedSeconds float64   `json:"elapsed_seconds"`
}

type ReviewBundle struct {
	Kind               string               `json:"kind"`
	SchemaVersion      int                  `json:"schema_version"`
	ExperimentID       string               `json:"experiment_id"`
	Scope              LedgerScope          `json:"scope"`
	ExecutionMode      string               `json:"execution_mode"`
	Decision           string               `json:"decision"`
	SelectedVariant    string               `json:"selected_variant"`
	Synthetic          bool                 `json:"synthetic"`
	AdoptionAuthorized bool                 `json:"adoption_authorized"`
	Build              BuildIdentity        `json:"build"`
	Result             ExperimentReport     `json:"result"`
	Timings            []BundleTiming       `json:"timings"`
	Plan               paired.ArtifactRef   `json:"plan_ref"`
	Report             paired.ArtifactRef   `json:"report_ref"`
	ParentJournal      paired.ArtifactRef   `json:"parent_journal_ref"`
	Baseline           VariantBinding       `json:"baseline"`
	Candidate          VariantBinding       `json:"candidate"`
	BaselineInventory  []File               `json:"baseline_inventory"`
	CandidateInventory []File               `json:"candidate_inventory"`
	Changes            []paired.Change      `json:"changes"`
	Patch              BundleFile           `json:"patch"`
	Markdown           BundleFile           `json:"markdown"`
	Lesson             BundleLesson         `json:"lesson"`
	Objects            []paired.ArtifactRef `json:"objects"`
}

// overlayObjects adds export-owned immutable bytes without writing the source
// store. In particular, the parent log snapshot is created only in the bundle.
type overlayObjects struct {
	source Objects
	inline map[paired.ArtifactRef][]byte
}

func (s overlayObjects) Read(ref paired.ArtifactRef) ([]byte, error) {
	if raw, exists := s.inline[ref]; exists {
		return raw, nil
	}
	return s.source.Read(ref)
}

func (overlayObjects) PutJSON(string, any) (paired.ArtifactRef, error) {
	return paired.ArtifactRef{}, errors.New("bundle source is read-only")
}

// collectBundleClosure follows only typed artifact references. Model-authored
// strings and arbitrary JSON keys cannot cause filesystem reads.
func collectBundleClosure(ctx context.Context, objects Objects, roots ...paired.ArtifactRef) ([]paired.ArtifactRef, error) {
	queue := slices.Clone(roots)
	seen := map[paired.ArtifactRef]bool{}
	uniqueBytes := map[string]bool{}
	var total int64
	var result []paired.ArtifactRef
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ref := queue[0]
		queue = queue[1:]
		if seen[ref] {
			continue
		}
		if len(result) >= maxBundleObjects || ref.Validate(ref.Kind) != nil {
			return nil, errors.New("bundle reference limit or identity is invalid")
		}
		raw, err := objects.Read(ref)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != ref.SizeBytes || paired.BytesDigest(raw) != ref.Digest {
			return nil, errors.New("bundle source digest or size mismatch")
		}
		if !uniqueBytes[ref.Digest] {
			if ref.SizeBytes > maxBundleBytes-total {
				return nil, errors.New("bundle exceeds 512 MiB object limit")
			}
			total += ref.SizeBytes
			uniqueBytes[ref.Digest] = true
		}
		children, err := bundleReferences(objects, ref)
		if err != nil {
			return nil, err
		}
		seen[ref] = true
		result = append(result, ref)
		queue = append(queue, children...)
	}
	slices.SortFunc(result, func(a, b paired.ArtifactRef) int {
		return strings.Compare(a.Kind+"/"+a.Digest, b.Kind+"/"+b.Digest)
	})
	return result, nil
}

func bundleReferences(objects Objects, ref paired.ArtifactRef) ([]paired.ArtifactRef, error) {
	switch ref.Kind {
	case "experiment_plan":
		var plan paired.ExperimentPlan
		if err := readObjectJSON(objects, ref, &plan); err != nil {
			return nil, err
		}
		return append([]paired.ArtifactRef{plan.Config, plan.Fixture, plan.BaselineSnapshot}, plan.Inputs...), nil
	case "experiment_report":
		var report ExperimentReport
		if err := readObjectJSON(objects, ref, &report); err != nil {
			return nil, err
		}
		refs := slices.Clone(report.ManifestRefs)
		if report.PlanRef != nil {
			refs = append(refs, *report.PlanRef)
		}
		for _, stage := range report.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.Settlement != nil {
					refs = append(refs, attempt.Settlement.Evidence)
				}
			}
		}
		return refs, nil
	case "frozen_variants":
		var bindings []VariantBinding
		if err := readObjectJSON(objects, ref, &bindings); err != nil {
			return nil, err
		}
		var refs []paired.ArtifactRef
		for _, binding := range bindings {
			refs = append(refs, binding.Snapshot)
		}
		return refs, nil
	case "stage_evidence":
		var evidence StageEvidence
		if err := readObjectJSON(objects, ref, &evidence); err != nil {
			return nil, err
		}
		refs := []paired.ArtifactRef{evidence.Child.EventLog}
		for _, artifact := range evidence.Child.Artifacts {
			refs = append(refs, artifact.Content)
		}
		if evidence.RoleInput != nil {
			refs = append(refs, *evidence.RoleInput)
		}
		return refs, nil
	case "role_input":
		var input roleInput
		if err := readObjectJSON(objects, ref, &input); err != nil {
			return nil, err
		}
		var refs []paired.ArtifactRef
		for _, source := range input.Sources {
			refs = append(refs, source.Attempts...)
		}
		for _, binding := range input.Variants {
			refs = append(refs, binding.Snapshot)
		}
		return refs, nil
	case "run_manifest":
		var manifest paired.RunManifest
		if err := readObjectJSON(objects, ref, &manifest); err != nil {
			return nil, err
		}
		refs := []paired.ArtifactRef{manifest.Snapshot, manifest.ExecutionPolicy, manifest.Usage}
		refs = append(refs, manifest.Receipts...)
		refs = append(refs, manifest.Verifications...)
		for _, previous := range []*paired.ArtifactRef{manifest.RetryOf, manifest.ReusedFrom} {
			if previous != nil {
				refs = append(refs, *previous)
			}
		}
		return refs, nil
	case "experiment_config", "fixture", "frozen_inputs", "team_snapshot", "child_event_log", "child_artifact", "parent_event_log":
		return nil, nil
	default:
		return nil, errors.New("unsupported bundle object kind")
	}
}
