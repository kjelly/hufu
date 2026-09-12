package executioncompat

import "github.com/kjelly/hufu/internal/execution"

// IsLegacyLocalAlias reports whether backend uses the historical spelling that
// was accepted before ollama became the sole durable built-in backend name.
func IsLegacyLocalAlias(backend string) bool {
	return execution.CanonicalBackendName(backend) == execution.LegacyLocalBackendName
}

// CanonicalBackend returns the durable backend spelling and whether the input
// used the historical local alias. It is pure so scanner and materializer code
// can share it without consulting current team configuration.
func CanonicalBackend(backend string) (canonical string, wasLegacyAlias bool) {
	return execution.CanonicalTargetBackendName(backend), IsLegacyLocalAlias(backend)
}
