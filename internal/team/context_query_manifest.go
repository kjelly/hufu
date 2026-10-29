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
	if manifest.SchemaVersion != ContextManifestSchemaVersion && manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion {
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
	if manifest.SchemaVersion == DependencyResultViewManifestSchemaVersion {
		manifest.Fingerprint = dependencyResultViewManifestFingerprint(manifest)
	} else {
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
	if manifest.SchemaVersion != DependencyResultViewManifestSchemaVersion {
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
	default:
		return fmt.Errorf("unsupported context manifest schema %d", manifest.SchemaVersion)
	}
	return nil
}

func supportedDeliveryContextManifest(manifest ContextInjectionManifest) bool {
	return (manifest.SchemaVersion == ContextManifestSchemaVersion || manifest.SchemaVersion == ContextQueryManifestSchemaVersion || manifest.SchemaVersion == DependencyResultViewManifestSchemaVersion) && ValidateContextQueryManifest(manifest) == nil
}
