package team

// TaskExecutionPolicy encapsulates execution semantics for a TaskDef.
type TaskExecutionPolicy struct {
	CachePolicy       CachePolicy     `json:"cache_policy,omitempty"`
	RecoveryPolicy    RecoveryPolicy  `json:"recovery_policy,omitempty"`
	SideEffect        SideEffectClass `json:"side_effect,omitempty"`
	AllowedTools      []string        `json:"allowed_tools,omitempty"`
	AllowedCommands   []string        `json:"allowed_commands,omitempty"`
	DeniedCommands    []string        `json:"denied_commands,omitempty"`
	ReadOnlyPaths     []string        `json:"read_only_paths,omitempty"`
	WritablePaths     []string        `json:"writable_paths,omitempty"`
	FreshCapabilities bool            `json:"fresh_capabilities,omitempty"`
	RequiresHuman     bool            `json:"requires_human,omitempty"`
	StrictResult      bool            `json:"strict_result,omitempty"`
}
