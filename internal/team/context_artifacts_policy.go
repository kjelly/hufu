package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

// rawContextArtifactsPolicy is the team.yaml context-artifacts block. Pointer
// fields distinguish an omitted limit, which takes its default, from an
// explicit value, which must be positive.
type rawContextArtifactsPolicy struct {
	Enabled                bool `yaml:"enabled,omitempty"`
	MinBytes               *int `yaml:"min-bytes,omitempty"`
	PreviewBytes           *int `yaml:"preview-bytes,omitempty"`
	MaxArtifactBytes       *int `yaml:"max-artifact-bytes,omitempty"`
	MaxReadBytes           *int `yaml:"max-read-bytes,omitempty"`
	MaxArtifactsPerAttempt *int `yaml:"max-artifacts-per-attempt,omitempty"`
}

// resolve returns the effective policy. A missing block is disabled; a
// declared block is validated even when it is disabled.
func (r *rawContextArtifactsPolicy) resolve() (agent.ContextArtifactsPolicy, error) {
	if r == nil {
		return agent.ContextArtifactsPolicy{}, nil
	}
	p := agent.DefaultContextArtifactsPolicy()
	p.Enabled = r.Enabled
	for _, field := range []struct {
		name  string
		value *int
		dst   *int
	}{
		{"min-bytes", r.MinBytes, &p.MinBytes},
		{"preview-bytes", r.PreviewBytes, &p.PreviewBytes},
		{"max-artifact-bytes", r.MaxArtifactBytes, &p.MaxArtifactBytes},
		{"max-read-bytes", r.MaxReadBytes, &p.MaxReadBytes},
		{"max-artifacts-per-attempt", r.MaxArtifactsPerAttempt, &p.MaxArtifactsPerAttempt},
	} {
		if field.value == nil {
			continue
		}
		if *field.value <= 0 {
			return agent.ContextArtifactsPolicy{}, fmt.Errorf("invalid context-artifacts config: %s must be positive, got %d", field.name, *field.value)
		}
		*field.dst = *field.value
	}
	if err := p.Validate(); err != nil {
		return agent.ContextArtifactsPolicy{}, fmt.Errorf("invalid context-artifacts config: %w", err)
	}
	return p, nil
}

// ExecutionContextArtifactPolicySnapshot pins the effective context-artifact
// limits for a run. It is present only when offload is enabled, so a team
// without the feature keeps its existing configuration hash.
type ExecutionContextArtifactPolicySnapshot struct {
	MinBytes               int `json:"min_bytes"`
	PreviewBytes           int `json:"preview_bytes"`
	MaxArtifactBytes       int `json:"max_artifact_bytes"`
	MaxReadBytes           int `json:"max_read_bytes"`
	MaxArtifactsPerAttempt int `json:"max_artifacts_per_attempt"`
}

func executionPolicyContextArtifacts(session *TeamSession) *ExecutionContextArtifactPolicySnapshot {
	if session == nil || !session.Config.ContextArtifacts.Enabled {
		return nil
	}
	p := session.Config.ContextArtifacts
	return &ExecutionContextArtifactPolicySnapshot{
		MinBytes: p.MinBytes, PreviewBytes: p.PreviewBytes, MaxArtifactBytes: p.MaxArtifactBytes,
		MaxReadBytes: p.MaxReadBytes, MaxArtifactsPerAttempt: p.MaxArtifactsPerAttempt,
	}
}

func cloneExecutionContextArtifactPolicy(src *ExecutionContextArtifactPolicySnapshot) *ExecutionContextArtifactPolicySnapshot {
	if src == nil {
		return nil
	}
	clone := *src
	return &clone
}

func validateExecutionContextArtifactPolicy(version int, snapshot *ExecutionContextArtifactPolicySnapshot) error {
	if snapshot == nil {
		return nil
	}
	if version < executionPolicyPreviousSnapshotVersion || version > executionPolicySnapshotVersion {
		return fmt.Errorf("execution policy snapshot v%d cannot pin context artifacts", version)
	}
	policy := agent.ContextArtifactsPolicy{
		Enabled: true, MinBytes: snapshot.MinBytes, PreviewBytes: snapshot.PreviewBytes,
		MaxArtifactBytes: snapshot.MaxArtifactBytes, MaxReadBytes: snapshot.MaxReadBytes,
		MaxArtifactsPerAttempt: snapshot.MaxArtifactsPerAttempt,
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("execution policy snapshot context artifacts: %w", err)
	}
	return nil
}

// admittedContextArtifactPolicy returns the offload limits from the admitted
// execution-policy snapshot. Live configuration is never consulted: a changed
// setting is detected as snapshot drift before any attempt runs.
func (c *Coordinator) admittedContextArtifactPolicy() (ExecutionContextArtifactPolicySnapshot, bool) {
	if c == nil || c.executionPolicy == nil || c.executionPolicy.snapshot == nil || c.executionPolicy.snapshot.ContextArtifacts == nil {
		return ExecutionContextArtifactPolicySnapshot{}, false
	}
	return *c.executionPolicy.snapshot.ContextArtifacts, true
}
