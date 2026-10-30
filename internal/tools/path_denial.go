package tools

// pathDenied records the authorization outcome before callers format the
// diagnostic for a model. It is independent of the diagnostic's language.
func pathDenied(cfg ToolConfig, err error) error {
	reportPathDenial(cfg)
	return err
}

func reportPathDenial(cfg ToolConfig) {
	if cfg.reportPathDenial != nil {
		cfg.reportPathDenial()
	}
}

type toolExecutionIdentityKey struct{}
type toolExecutionIdentity struct{ name, callID string }
