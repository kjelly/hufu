package team

import (
	json "encoding/json/v2"
	"fmt"
	"slices"
	"time"
)

const ContextQueryManifestSchemaVersion = 3

func cloneContextQueryAttribution(query *ContextQueryAttribution) *ContextQueryAttribution {
	if query == nil {
		return nil
	}
	copy := *query
	copy.Sources = slices.Clone(query.Sources)
	return &copy
}

// WithContextQueryAttribution creates schema3 without recomputing the v1
// request identity or applying a newer fingerprint to a legacy manifest.
func WithContextQueryAttribution(manifest ContextInjectionManifest, attribution ContextQueryAttribution) (ContextInjectionManifest, error) {
	if manifest.SchemaVersion != ContextManifestSchemaVersion && manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion && manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		return manifest, fmt.Errorf("query attribution requires a schema2 compiler manifest")
	}
	if manifest.Query != nil {
		return manifest, fmt.Errorf("context manifest already carries query attribution")
	}
	if err := ValidateContextQueryManifest(manifest); err != nil {
		return manifest, err
	}
	if err := ValidateContextQueryAttribution(attribution); err != nil {
		return manifest, err
	}
	manifest = *cloneContextInjectionManifest(&manifest)
	attribution.Sources = slices.Clone(attribution.Sources)
	manifest.Query = &attribution
	switch manifest.SchemaVersion {
	case HistoricalResultManifestSchemaVersion:
		manifest.Fingerprint = historicalResultManifestFingerprint(manifest)
	case DependencyResultViewManifestSchemaVersion:
		manifest.Fingerprint = dependencyResultViewManifestFingerprint(manifest)
	default:
		manifest.SchemaVersion = ContextQueryManifestSchemaVersion
		manifest.Fingerprint = queryContextManifestFingerprint(manifest)
	}
	return manifest, nil
}

func queryContextManifestFingerprint(manifest ContextInjectionManifest) string {
	manifest.Fingerprint = ""
	manifest.CreatedAt = time.Time{}
	data, err := json.Marshal(manifest, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	return contextQueryDigest(string(data))
}

func ValidateContextQueryManifest(manifest ContextInjectionManifest) error {
	if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion && hasHistoricalResultManifestItems(manifest) {
		return fmt.Errorf("legacy context manifest cannot carry historical result attribution")
	}
	if manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion && manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		if err := rejectLegacyDependencyResultView(manifest); err != nil {
			return err
		}
	}
	switch manifest.SchemaVersion {
	case 1, ContextManifestSchemaVersion:
		if manifest.Query != nil {
			return fmt.Errorf("legacy context manifest cannot carry query attribution")
		}
		if manifest.Fingerprint != contextManifestFingerprint(manifest) {
			return fmt.Errorf("legacy context manifest fingerprint mismatch")
		}
	case ContextQueryManifestSchemaVersion:
		if manifest.Query == nil {
			return fmt.Errorf("schema3 context manifest requires query attribution")
		}
		if err := ValidateContextQueryAttribution(*manifest.Query); err != nil {
			return err
		}
		if manifest.Fingerprint != queryContextManifestFingerprint(manifest) {
			return fmt.Errorf("query context manifest fingerprint mismatch")
		}
	case DependencyResultViewManifestSchemaVersion:
		return validateDependencyResultViewManifest(manifest)
	case HistoricalResultManifestSchemaVersion:
		return validateHistoricalResultManifest(manifest)
	default:
		return fmt.Errorf("unsupported context manifest schema %d", manifest.SchemaVersion)
	}
	return nil
}

func supportedDeliveryContextManifest(manifest ContextInjectionManifest) bool {
	return (manifest.SchemaVersion == ContextManifestSchemaVersion || manifest.SchemaVersion == ContextQueryManifestSchemaVersion || manifest.SchemaVersion == DependencyResultViewManifestSchemaVersion || manifest.SchemaVersion == HistoricalResultManifestSchemaVersion) && ValidateContextQueryManifest(manifest) == nil
}
