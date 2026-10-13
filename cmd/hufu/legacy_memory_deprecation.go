package main

// Deprecation notices for the legacy chromem long-term memory store. The
// removal gate is in docs/deprecations/legacy-memory-store.md.
const (
	memoryFlagDeprecation      = "it has no effect; runs always use canonical context memory"
	memoryModelFlagDeprecation = "it has no effect; semantic memory retrieval uses the semantic-retrieval settings in hufu.yaml"
	migrateMemoryDeprecation   = "import any remaining legacy memory store now; this command and chromem support will be removed in a later release (see docs/deprecations/legacy-memory-store.md)"
)
