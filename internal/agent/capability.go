package agent

// CapabilityRequirement describes an environment check that can be probed
// before a task starts. Team config can declare these under `preflight`, and
// tasks can reference them by name via `requires`.
type CapabilityRequirement struct {
	Name    string `yaml:"name" json:"name"`
	Probe   string `yaml:"probe" json:"probe"`
	Timeout int64  `yaml:"timeout" json:"timeout"`
	Scope   string `yaml:"scope" json:"scope"`
}
