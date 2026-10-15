package agent

// BeliefConfig enables a team-owned finite model. Experiment selects the one
// pass/fail experiment fed by canonical verification receipts. It grants no
// tools, changes no acceptance rule, and never enables automatic dispatch.
type BeliefConfig struct {
	ModelFile  string `yaml:"model-file"`
	Experiment string `yaml:"experiment"`
}
