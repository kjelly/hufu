package team

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"time"
)

// DependencyResultViewManifestSchemaVersion adds selected-view provenance;
// full manifests retain schema2 or the existing query-attribution schema3.
const DependencyResultViewManifestSchemaVersion = 4

func finalizeContextManifestAttribution(manifest ContextInjectionManifest, query *ContextQueryAttribution) (ContextInjectionManifest, error) {
	if hasHistoricalResultManifestItems(manifest) {
		if err := finalizeHistoricalResultManifest(&manifest); err != nil {
			return ContextInjectionManifest{}, err
		}
	} else if err := finalizeDependencyResultViewManifest(&manifest); err != nil {
		return ContextInjectionManifest{}, err
	}
	if query != nil {
		return WithContextQueryAttribution(manifest, *query)
	}
	return manifest, nil
}

// DependencyResultViewReport preserves content-free request and source
// attribution and distinguishes preparation from runtime dispatch.
type DependencyResultViewReport struct {
	RequestID        string                           `json:"request_id"`
	TaskID           string                           `json:"task_id"`
	Agent            string                           `json:"agent"`
	Attempt          int                              `json:"attempt"`
	ModelExecutionID string                           `json:"model_execution_id,omitempty"`
	ModelCalled      bool                             `json:"model_called"`
	ContentSHA256    string                           `json:"content_sha256"`
	Attribution      *DependencyResultViewAttribution `json:"attribution"`
}

func dependencyResultViewReport(manifest ContextInjectionManifest, item ContextManifestItem) *DependencyResultViewReport {
	if item.DependencyResultView == nil || !primaryDependencyViewRequest(manifest) {
		return nil
	}
	return &DependencyResultViewReport{RequestID: manifest.RequestID, TaskID: manifest.TaskID, Agent: manifest.Agent,
		Attempt: manifest.Attempt, ModelExecutionID: manifest.ModelExecutionID, ModelCalled: manifest.ModelCalled,
		ContentSHA256: item.ContentHash, Attribution: item.DependencyResultView.Clone()}
}

func validateDependencyResultViewAttribution(attribution *DependencyResultViewAttribution) error {
	if attribution == nil {
		return fmt.Errorf("selected dependency attribution is missing")
	}
	policy, err := attribution.Policy.normalized()
	if err != nil || policy == nil {
		return fmt.Errorf("dependency attribution requires a valid selected policy")
	}
	if !slices.Equal(policy.Fields, attribution.Policy.Fields) {
		return fmt.Errorf("dependency attribution policy is not canonical")
	}
	expected, err := dependencyViewPolicyDigest(policy)
	if err != nil || attribution.PolicySHA256 != expected {
		return fmt.Errorf("dependency attribution policy digest mismatch")
	}
	if len(attribution.Sources) == 0 {
		return fmt.Errorf("dependency attribution requires canonical sources")
	}
	seen := make(map[string]bool, len(attribution.Sources))
	for _, source := range attribution.Sources {
		if source.TaskID == "" || seen[source.TaskID] || source.BranchID == "" || source.RunID == "" || source.SourceEventID == "" {
			return fmt.Errorf("dependency attribution source identity is invalid")
		}
		if source.Attempt < 1 || source.OccurrenceRevision < 1 || !validFullContentHash(source.SourceEventSHA256) || !validFullContentHash(source.FullViewSHA256) {
			return fmt.Errorf("dependency attribution source binding is invalid")
		}
		seen[source.TaskID] = true
	}
	return nil
}

func attachDependencyResultViewManifestItem(target *ContextManifestItem, item ContextItem, included bool) error {
	if item.DependencyResultView == nil {
		return nil
	}
	if item.ID != "dependency_results" || item.Kind != "dependency_result" || !included || !item.Required || item.Compressed {
		return fmt.Errorf("selected dependency context must be fully included and required")
	}
	if err := validateDependencyResultViewAttribution(item.DependencyResultView); err != nil {
		return err
	}
	target.DependencyResultView = item.DependencyResultView.Clone()
	target.ContentHash = dependencyViewDigest([]byte(item.Content))
	return nil
}

func finalizeDependencyResultViewManifest(manifest *ContextInjectionManifest) error {
	for _, item := range manifest.Items {
		if item.DependencyResultView != nil {
			manifest.SchemaVersion = DependencyResultViewManifestSchemaVersion
			if primaryDependencyViewRequest(*manifest) {
				manifest.ModelCalled = false
				manifest.Outcome = "prepared"
			}
			manifest.Fingerprint = dependencyResultViewManifestFingerprint(*manifest)
			return validateDependencyResultViewManifest(*manifest)
		}
	}
	return nil
}

func dependencyResultViewManifestFingerprint(manifest ContextInjectionManifest) string {
	manifest.Fingerprint = ""
	manifest.CreatedAt = time.Time{}
	data, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return dependencyViewDigest(data)
}

func validateDependencyResultViewManifest(manifest ContextInjectionManifest) error {
	if manifest.SchemaVersion == HistoricalResultManifestSchemaVersion {
		return validateHistoricalResultManifest(manifest)
	}
	if manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion {
		return fmt.Errorf("selected dependency manifest uses an unsupported schema")
	}
	if err := validateDependencyResultViewManifestItems(manifest); err != nil {
		return err
	}
	if manifest.Query != nil {
		if err := ValidateContextQueryAttribution(*manifest.Query); err != nil {
			return err
		}
	}
	if manifest.Fingerprint != dependencyResultViewManifestFingerprint(manifest) {
		return fmt.Errorf("selected dependency manifest fingerprint mismatch")
	}
	return nil
}

func rejectLegacyDependencyResultView(manifest ContextInjectionManifest) error {
	for _, item := range manifest.Items {
		if item.DependencyResultView != nil {
			return fmt.Errorf("legacy context manifest cannot carry selected dependency attribution")
		}
	}
	return nil
}

func validateDependencyResultViewManifestItems(manifest ContextInjectionManifest) error {
	count := 0
	for _, item := range manifest.Items {
		if item.Kind != "dependency_result" && item.DependencyResultView == nil {
			continue
		}
		if item.ID != "dependency_results" || item.Kind != "dependency_result" || !item.Included || item.Compressed || item.Reason != ContextIncludedRequired || !validFullContentHash(item.ContentHash) {
			return fmt.Errorf("selected dependency manifest does not prove complete presentation")
		}
		if err := validateDependencyResultViewAttribution(item.DependencyResultView); err != nil {
			return err
		}
		count++
	}
	if count != 1 {
		return fmt.Errorf("selected dependency manifest requires one dependency context item")
	}
	return nil
}
