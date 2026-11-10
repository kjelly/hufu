package team

import (
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// HistoricalResultManifestSchemaVersion composes historical grants with the
// existing query and dependency-view attribution. Schemas2/3/4 keep their bytes.
const HistoricalResultManifestSchemaVersion = 5

func historicalResultManifestFingerprint(manifest ContextInjectionManifest) string {
	manifest.Fingerprint, manifest.CreatedAt = "", time.Time{}
	data, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return historicalResultDigest(data)
}

func hasHistoricalResultManifestItems(manifest ContextInjectionManifest) bool {
	return slices.ContainsFunc(manifest.Items, func(item ContextManifestItem) bool {
		return item.Kind == historicalResultContextKind || item.HistoricalResult != nil
	})
}

func validateHistoricalResultAttribution(a *HistoricalResultContextAttribution) error {
	if a == nil || a.Binding == nil || !historicalGrantSHA256(a.PolicyFingerprint) {
		return fmt.Errorf("historical result attribution is missing its binding or policy")
	}
	if err := validateHistoricalResultTaskBindingShape(&TodoItem{ID: a.Binding.TaskID, HistoricalResultBinding: a.Binding}); err != nil {
		return err
	}
	if err := validateHistoricalResultGrant(a.Source); err != nil {
		return err
	}
	if a.Source.Recipient != a.Binding.Recipient || !slices.Contains(a.Binding.ResultIDs, a.Source.ResultID) ||
		a.Source.PresentationBytes <= 0 || a.Source.PresentationBytes > maxHistoricalResultBytes {
		return fmt.Errorf("historical result attribution differs from its recipient binding")
	}
	return nil
}

func attachHistoricalResultManifestItem(target *ContextManifestItem, item ContextItem, included bool) error {
	if item.Kind != historicalResultContextKind && item.HistoricalResult == nil {
		return nil
	}
	if err := validateHistoricalResultAttribution(item.HistoricalResult); err != nil {
		return err
	}
	source := item.HistoricalResult.Source
	if item.ID != historicalResultContextID(source.ResultID) || item.Kind != historicalResultContextKind ||
		!included || !item.Required || item.Compressed || item.Compressible || item.Authority != ContextAuthorityHistorical ||
		len(item.Content) != source.PresentationBytes || historicalResultDigest([]byte(item.Content)) != source.PresentationSHA256 {
		return fmt.Errorf("historical result context must include the exact complete required presentation")
	}
	target.HistoricalResult = item.HistoricalResult.Clone()
	target.ContentHash = source.PresentationSHA256
	return nil
}

func finalizeHistoricalResultManifest(manifest *ContextInjectionManifest) error {
	manifest.SchemaVersion = HistoricalResultManifestSchemaVersion
	manifest.ModelCalled, manifest.Outcome = false, "prepared"
	manifest.Fingerprint = historicalResultManifestFingerprint(*manifest)
	return validateHistoricalResultManifest(*manifest)
}

func validateHistoricalResultManifest(manifest ContextInjectionManifest) error {
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion || !primaryDependencyViewRequest(manifest) ||
		manifest.AgentRole != "worker" || manifest.Attempt < 1 || manifest.RunID == "" || manifest.RequestID == "" || manifest.ModelExecutionID == "" ||
		(manifest.ModelCalled && manifest.Outcome != "model_call") || (!manifest.ModelCalled && manifest.Outcome != "prepared") {
		return fmt.Errorf("historical result manifest has an invalid request or dispatch identity")
	}
	var first *HistoricalResultContextAttribution
	var ids []string
	bytes := 0
	for _, item := range manifest.Items {
		if item.Kind != historicalResultContextKind && item.HistoricalResult == nil {
			continue
		}
		if err := validateHistoricalResultManifestItem(manifest, item); err != nil {
			return err
		}
		a := item.HistoricalResult
		if first == nil {
			first = a
		} else if !reflect.DeepEqual(first.Binding, a.Binding) || first.PolicyFingerprint != a.PolicyFingerprint {
			return fmt.Errorf("historical result manifest mixes task or policy bindings")
		}
		if slices.Contains(ids, a.Source.ResultID) || a.Source.PresentationBytes > maxHistoricalResultBytes-bytes {
			return fmt.Errorf("historical result manifest repeats or exceeds its authorized source set")
		}
		ids = append(ids, a.Source.ResultID)
		bytes += a.Source.PresentationBytes
	}
	slices.Sort(ids)
	if first == nil || !slices.Equal(ids, first.Binding.ResultIDs) {
		return fmt.Errorf("historical result manifest lacks the complete recipient source set")
	}
	if slices.ContainsFunc(manifest.Items, func(item ContextManifestItem) bool { return item.DependencyResultView != nil }) {
		if err := validateDependencyResultViewManifestItems(manifest); err != nil {
			return err
		}
	}
	if manifest.Query != nil {
		if err := ValidateContextQueryAttribution(*manifest.Query); err != nil {
			return err
		}
	}
	if manifest.Fingerprint != historicalResultManifestFingerprint(manifest) {
		return fmt.Errorf("historical result manifest fingerprint mismatch")
	}
	return nil
}

func validateHistoricalResultManifestItem(manifest ContextInjectionManifest, item ContextManifestItem) error {
	if err := validateHistoricalResultAttribution(item.HistoricalResult); err != nil {
		return err
	}
	a := item.HistoricalResult
	if a.Binding.TaskID != manifest.TaskID || a.Binding.Recipient != strings.ToLower(strings.TrimSpace(manifest.Agent)) ||
		item.Kind != historicalResultContextKind || item.ID != historicalResultContextID(a.Source.ResultID) || !item.Included ||
		item.Compressed || item.Reason != ContextIncludedRequired || item.Tokens <= 0 || item.ContentHash != a.Source.PresentationSHA256 {
		return fmt.Errorf("historical result manifest does not prove complete worker inclusion")
	}
	return nil
}

type HistoricalResultDeliveryReport struct {
	RequestID        string                              `json:"request_id"`
	TaskID           string                              `json:"task_id"`
	Agent            string                              `json:"agent"`
	Attempt          int                                 `json:"attempt"`
	ModelExecutionID string                              `json:"model_execution_id"`
	ModelCalled      bool                                `json:"model_called"`
	Included         bool                                `json:"included"`
	Reason           ContextDecisionReason               `json:"reason"`
	Tokens           int                                 `json:"tokens"`
	Attribution      *HistoricalResultContextAttribution `json:"attribution"`
}

func historicalResultDeliveryReport(manifest ContextInjectionManifest, item ContextManifestItem) *HistoricalResultDeliveryReport {
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion || item.HistoricalResult == nil {
		return nil
	}
	return &HistoricalResultDeliveryReport{RequestID: manifest.RequestID, TaskID: manifest.TaskID, Agent: manifest.Agent,
		Attempt: manifest.Attempt, ModelExecutionID: manifest.ModelExecutionID, ModelCalled: manifest.ModelCalled,
		Included: item.Included, Reason: item.Reason, Tokens: item.Tokens, Attribution: item.HistoricalResult.Clone()}
}
