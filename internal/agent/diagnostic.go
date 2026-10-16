package agent

// DiagnosticLoopConfig prepares immutable inputs only. Stage 1 supports off;
// shadow and bounded_read_only remain unavailable until their runtime exists.
type DiagnosticLoopConfig struct {
	Mode        string `yaml:"mode"`
	ModelFile   string `yaml:"model-file"`
	PolicyFile  string `yaml:"policy-file"`
	AdapterFile string `yaml:"adapter-file"`
	Actor       string `yaml:"actor"`
}
