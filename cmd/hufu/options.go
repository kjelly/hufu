package main

// runOptions holds every CLI flag for the root `hufu` command, replacing the
// former sprawl of package-level globals. Flags are bound to fields of the
// single package-level instance `opts` in newRootCommand; tests that need a
// clean slate can save/restore the whole struct in one assignment.
type runOptions struct {
	// Provider / model selection
	providerURL               string
	providerAPIKey            string
	modelOverride             string
	temperatureOverride       string
	maxTokensOverride         string
	topPOverride              string
	topKOverride              string
	reasoningEffortOverride   string
	sidecarModelOverride      string
	guardModelOverride        string
	judgeModelOverride        string
	planReviewerModelOverride string

	// Team selection / discovery
	agentTeamName       string
	agentTeamSearchPath string
	defaultTeam         bool
	helperTools         string
	autoTeam            bool
	routeMode           string

	// Workspace / session lifecycle
	workspace     string
	newSession    bool
	tempWorkspace bool
	showHistory   bool

	// Memory
	memoryEnabled bool
	memoryModel   string
	archiveMemory bool

	// Execution behavior
	executionProfile      string
	goalMode              string
	stepsMode             bool
	dryRun                bool
	planMode              bool
	autoSkills            bool
	forcedSkills          []string
	fixQuestion           string
	reportMode            bool
	think                 bool
	direnv                bool
	timeoutOverride       int64
	verifyTimeoutOverride int64
	maxRoundsOverride     int
	maxConcurrentOverride int
	maxStepsOverride      int

	// Security / sandboxing
	rbashMode   bool
	noNet       bool
	noJournal   bool
	forceMCP    bool
	allowPaths  []string
	unattended  bool
	autoApprove bool

	// Budgets
	maxDuration    int64
	maxTotalTokens int64

	// Prompt sources / template variables
	varFlags         []string
	varFiles         []string
	templateName     string
	initTemplateName string
	profileName      string
	projectContext   bool

	// Output / display
	verbose           bool
	quietMode         bool
	outputFormat      string
	displayMode       string
	noColorMode       bool
	noSummary         bool
	noSpinner         bool
	tuiMode           bool
	enablePTYTerminal bool
	tuiCompact        bool
	eventFormat       string

	// Runtime state (not a flag): set by the chat command so the injection
	// loop knows it drives a TUI conversation instead of a one-shot run.
	isChatTUI bool
}

// opts is the process-wide flag state for the root command and its helpers.
var opts runOptions
