package agent

// ExecutionReportConfig declares ordered sections for a team's saved run report.
type ExecutionReportConfig struct {
	Sections []ReportSectionConfig `yaml:"sections,omitempty" json:"sections,omitempty"`
}

type ReportSectionConfig struct {
	ID       string               `yaml:"id" json:"id"`
	Title    string               `yaml:"title" json:"title"`
	Source   ReportSectionSource  `yaml:"source" json:"source"`
	Renderer ReportRendererConfig `yaml:"renderer" json:"renderer"`
}

type ReportSectionSource struct {
	Task    string `yaml:"task" json:"task"`
	Output  string `yaml:"output" json:"output"`
	Require string `yaml:"require" json:"require"`
}

type ReportRendererConfig struct {
	Runtime  string `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Source   string `yaml:"source,omitempty" json:"source,omitempty"`
	Mode     string `yaml:"mode,omitempty" json:"mode,omitempty"`
	Timeout  int    `yaml:"timeout,omitempty" json:"timeout,omitzero"`
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
}
