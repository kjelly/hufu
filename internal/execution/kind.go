// Package execution owns dependency-light execution identity values.
package execution

// BackendKind identifies the type of engine that executes an admitted worker
// attempt. It deliberately says nothing about Hufu authorization.
type BackendKind string

const (
	BackendKindLLM   BackendKind = "llm"
	BackendKindAgent BackendKind = "agent"
)

// BackendCapabilities describe an execution engine. They do not grant a task
// permission to use the corresponding capability.
type BackendCapabilities struct {
	DirectLanguageModel bool
	StructuredResult    bool
	WorkspaceRead       bool
	WorkspaceWrite      bool
	Shell               bool
	Resume              bool
}

// TargetDefaults supplies trusted defaults used only while admitting a bare
// selector as new work.
type TargetDefaults struct {
	DefaultLLMBackend string
}
