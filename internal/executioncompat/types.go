// Package executioncompat owns pure, dependency-light compatibility types for
// inspecting and materializing historical durable execution identity.
package executioncompat

// Feature identifies one historical execution-identity surface. A subject can
// report more than one feature while retaining exactly one classification.
type Feature string

const (
	FeatureLocalAlias            Feature = "local_alias"
	FeatureProviderShadowFields  Feature = "provider_shadow_fields"
	FeatureLegacyProviderBinding Feature = "legacy_provider_binding"
	FeatureProviderSessionEvent  Feature = "provider_session_event"
	FeatureLegacyReceiptProvider Feature = "legacy_receipt_provider"
	FeatureLegacyPolicyRoute     Feature = "legacy_policy_route"
	FeatureLegacyResumeMigration Feature = "legacy_resume_migration"
)

// Classification is the deterministic compatibility state of one task or
// policy subject. Feature counts may overlap; classifications may not.
type Classification string

const (
	ClassificationCanonical     Classification = "canonical"
	ClassificationMigrated      Classification = "migrated"
	ClassificationMigratable    Classification = "migratable"
	ClassificationAmbiguous     Classification = "ambiguous"
	ClassificationUnmigratable  Classification = "unmigratable"
	ClassificationNotApplicable Classification = "not_applicable"
)
