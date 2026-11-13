package team

import (
	"github.com/kjelly/hufu/internal/decisionrt/control"
	"github.com/kjelly/hufu/internal/decisionrt/settings"
)

// ResolveDecisionRuntime shares one resolver across startup, execution and recovery.
// Direct API sessions receive the same alias validation as file-loaded sessions.
func (s *TeamSession) ResolveDecisionRuntime() (settings.Config, error) {
	global, err := control.NormalizeTransport(s.GlobalDecisionRuntime, s.GlobalControlDecisions, "global")
	if err != nil {
		return settings.Config{}, err
	}
	local, err := control.NormalizeTransport(s.Config.DecisionRuntime, s.Config.ControlDecisions, "team")
	if err != nil {
		return settings.Config{}, err
	}
	return settings.Merge(global, local), nil
}

func (s *TeamSession) decisionRuntimeFingerprint() (string, error) {
	shared, err := s.ResolveDecisionRuntime()
	if err != nil {
		return "", err
	}
	return shared.Fingerprint("sidecar")
}
