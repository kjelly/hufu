package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
	"github.com/kjelly/hufu/internal/resultquery"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

type resultsCLIOptions struct {
	workspace, team, agent, invocation, run, branch, since, until, output, cursor, model, timezone string
	statuses                                                                                       []string
	limit                                                                                          int
	allBranches, preview                                                                           bool
	render                                                                                         string
	payload                                                                                        bool
	theme, displayMode, displayPreset                                                              string
	quiet, noColor                                                                                 bool
	pickTasks                                                                                      bool
	pickView, exportPath                                                                           string
}

type resultsDeps struct {
	report    func(context.Context, string, string) (*team.RunReport, error)
	workspace workspaceCommandDeps
	load      func(context.Context, inspectpkg.HistoricalResultScope) (inspectpkg.HistoricalSnapshot, error)
	generate  func(context.Context, *operatorpkg.OperatorSnapshot, string, string, string, sidecar.Profile) (explainAIResult, error)
	now       func() time.Time
	pick      func(context.Context, io.Reader, io.Writer, []inspectpkg.HistoricalResult, string) (int, error)
}

type resultsEnvelope struct {
	Report         *team.RunReport                 `json:"report,omitempty"`
	ReportMarkdown string                          `json:"report_markdown,omitempty"`
	SchemaVersion  int                             `json:"schema_version"`
	Kind           string                          `json:"kind"`
	Team           string                          `json:"team_scope,omitempty"`
	Operation      string                          `json:"operation"`
	Coverage       []inspectpkg.HistoricalCoverage `json:"coverage"`
	Matched        int                             `json:"matched_count"`
	Results        []inspectpkg.HistoricalResult   `json:"results"`
	NextCursor     string                          `json:"next_cursor,omitempty"`
	Query          *resultquery.Result             `json:"query,omitempty"`
	Model          string                          `json:"model,omitempty"`
	Error          string                          `json:"error,omitempty"`
	Usage          resultsAnalysisUsage            `json:"usage"`
}

type resultsAnalysisUsage struct {
	Available    bool  `json:"available"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

func newResultsCommand() *cobra.Command {
	return newResultsCommandWithDeps(resultsDeps{
		workspace: defaultWorkspaceCommandDeps(), load: inspectpkg.LoadHistoricalResults,
		generate: analyzeOperatorAI, now: time.Now,
	})
}

func newResultsCommandWithDeps(deps resultsDeps) *cobra.Command {
	options := &resultsCLIOptions{limit: 20, output: "text", timezone: "Local"}
	command := &cobra.Command{Use: "results", Aliases: []string{"result"}, Short: "Show the latest final response, browse saved runs, or query history", Args: cobra.NoArgs, SilenceUsage: true}
	flags := command.PersistentFlags()
	flags.StringVarP(&options.workspace, "workspace", "w", "", "Exact workspace override (default: current project's registered team workspaces)")
	flags.StringVar(&options.team, "team", "", "Restrict every result read and model input to this team")
	flags.StringVar(&options.agent, "agent", "", "Agent identity filter (case-insensitive)")
	flags.StringVar(&options.invocation, "invocation", "", "Exact invocation ID filter")
	flags.StringVar(&options.run, "run", "", "Optional exact execution run ID filter")
	flags.StringVar(&options.branch, "branch", "", "Exact branch ID, name, or label (default: active lineage)")
	flags.BoolVar(&options.allBranches, "all-branches", false, "Explicitly include historical session roots and sibling branches")
	flags.StringSliceVar(&options.statuses, "status", nil, "Terminal task statuses (list/search/latest default: done)")
	flags.StringVar(&options.since, "since", "", "Inclusive terminal-event timestamp (RFC3339)")
	flags.StringVar(&options.until, "until", "", "Exclusive terminal-event timestamp (RFC3339)")
	flags.IntVar(&options.limit, "limit", 20, "Result limit, 1 to 50")
	flags.StringVar(&options.output, "output", "text", "Output format: text or json")
	flags.StringVar(&options.render, "render", "plain", "Text presentation: plain or markdown (Markdown only on a color-enabled terminal)")
	registerStaticFlagCompletion(command, "render", []string{"plain", "markdown"})
	flags.StringVar(&options.theme, "theme", "", "Display theme: auto, light, dark, or mono (default: config or auto)")
	flags.StringVar(&options.displayMode, "display-mode", "auto", "Display mode: auto, terminal, or plain")
	flags.StringVar(&options.displayPreset, "display-preset", "", "Display preset: default or epaper (default: config or default)")
	flags.BoolVarP(&options.quiet, "quiet", "q", false, "Suppress source metadata on stderr")
	flags.BoolVar(&options.noColor, "no-color", false, "Disable ANSI color output (also honors NO_COLOR)")
	registerStaticFlagCompletion(command, "theme", []string{"auto", "light", "dark", "mono"})
	registerStaticFlagCompletion(command, "display-mode", []string{"auto", "terminal", "plain"})
	registerStaticFlagCompletion(command, "display-preset", []string{"default", "epaper"})
	flags.StringVar(&options.cursor, "cursor", "", "Continue an offline list/search at the same event snapshot")
	registerStaticFlagCompletion(command, "output", []string{"text", "json"})
	registerStaticFlagCompletion(command, "status", []string{"done", "error", "blocked", "skipped", "protocol_incomplete"})
	command.RunE = func(cmd *cobra.Command, _ []string) error { return runResults(cmd, options, deps, "read", "") }
	for _, operation := range []string{"runs", "list", "search", "latest", "show", "final", "invocation", "query", "pick", "report"} {
		child := &cobra.Command{Use: operation, ValidArgsFunction: cobra.NoFileCompletions, Args: cobra.NoArgs}
		switch operation {
		case "report":
			child.Use += " [result-id]"
			child.Short = "Read an immutable execution report for a saved result (default: latest run)"
			child.Args = cobra.MaximumNArgs(1)
			child.Flags().StringVar(&options.exportPath, "export", "", "Export the saved report as Markdown to this path")
		case "pick":
			child.Flags().StringVar(&options.pickView, "view", "final", "Selected content: final or report (report requires final-response mode)")
			registerStaticFlagCompletion(child, "view", []string{"final", "report"})
			child.Use += " [text]"
			child.Short = "Fuzzy-search saved final responses and read the selection (--tasks for agent tasks)"
			child.Args = cobra.MaximumNArgs(1)
			child.Flags().BoolVar(&options.pickTasks, "tasks", false, "Search individual agent task results instead of final responses")
		case "runs":
			child.Short = "Browse saved final responses with short previews and reading commands"
		case "list":
			child.Short = "List saved terminal task results without requiring run IDs"
		case "latest":
			child.Short = "Read the most recent completed task result"
		case "search":
			child.Use += " <text>"
			child.Short = "Search saved result text offline, including Unicode"
			child.Args = cobra.ExactArgs(1)
		case "show":
			child.Use += " <result-id>"
			child.Short = "Read one exact saved result or unique ID prefix"
			child.Args = cobra.ExactArgs(1)
			child.Flags().BoolVar(&options.payload, "payload", false, "Print only the complete structured payload JSON; fail if unavailable")
		case "final":
			child.Use += " [result-id]"
			child.Short = "Read the latest saved final run response, or the response for a task result"
			child.Args = cobra.MaximumNArgs(1)
		case "invocation":
			child.Use += " <invocation-id>"
			child.Short = "List every saved task and run response for an invocation"
			child.Args = cobra.ExactArgs(1)
		case "query":
			child.Use += " <question>"
			child.Short = "Ask an LLM about saved history with source-linked answers"
			child.Args = cobra.ExactArgs(1)
			child.Flags().StringVarP(&options.model, "model", "m", "", "Analysis model (default: selected team's or global sidecar/model)")
			child.Flags().StringVar(&options.timezone, "timezone", "Local", "Timezone for relative dates, e.g. UTC or Asia/Taipei")
			child.Flags().BoolVar(&options.preview, "preview", false, "Show explicit scope and model configuration without resolving the question or calling an LLM")
		}
		child.RunE = func(cmd *cobra.Command, args []string) error {
			value := ""
			if len(args) > 0 {
				value = args[0]
			}
			if operation == "runs" {
				return runResults(cmd, options, deps, "overview", value)
			}
			return runResults(cmd, options, deps, operation, value)
		}
		command.AddCommand(child)
	}
	return command
}

func runResults(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, operation, value string) error {
	if err := validateResultsPresentation(options, operation); err != nil {
		return resultsExit(2, err)
	}
	if operation == "query" {
		ctx, cancel := context.WithTimeout(command.Context(), 60*time.Second)
		defer cancel()
		command.SetContext(ctx)
	}
	request, err := validateResultsOptions(options, deps.now())
	if err != nil {
		return resultsExit(2, err)
	}
	if options.cursor != "" && !slices.Contains([]string{"overview", "list", "search", "invocation"}, operation) {
		return resultsExit(2, errors.New("--cursor applies only to the final-response overview or list/search/invocation"))
	}
	if isFinalResultsOperation(operation) && (options.agent != "" || len(options.statuses) > 0) {
		return resultsExit(2, errors.New("--agent and --status filter worker tasks; use results list with these filters, then final <result-id> to read their run response"))
	}
	if operation == "query" && (len([]rune(strings.TrimSpace(value))) == 0 || len([]rune(value)) > 2000) {
		return resultsExit(2, errors.New("question must contain 1 to 2000 characters"))
	}
	if operation == "invocation" {
		if options.invocation != "" && options.invocation != value {
			return resultsExit(2, errors.New("positional invocation conflicts with --invocation"))
		}
		request.InvocationID = value
	}
	snapshots, err := loadResultsScopes(command.Context(), options, deps)
	if err != nil {
		return resultsExit(inspectpkg.ExitCodeFor(err), err)
	}
	response := resultsEnvelope{SchemaVersion: 1, Kind: "historical_results", Team: options.team, Operation: "succeeded", Results: []inspectpkg.HistoricalResult{}, Coverage: []inspectpkg.HistoricalCoverage{}}
	all := []inspectpkg.HistoricalResult{}
	for _, snapshot := range snapshots {
		all = append(all, snapshot.Results...)
		response.Coverage = append(response.Coverage, snapshot.Coverage)
	}
	if operation == "query" {
		request.Question = value
		return runResultsQuery(command, options, deps, request, all, response)
	}
	if operation == "pick" {
		return runResultsPick(command, options, deps, request, all, response, value)
	}
	plan := resultquery.Plan{Selectors: resultquery.Selectors{}, StatusBasis: "at_terminal", Order: "newest", Limit: options.limit}
	if operation == "list" || operation == "search" || operation == "latest" {
		plan.Statuses = options.statuses
		if len(plan.Statuses) == 0 {
			plan.Statuses = []string{"done"}
		}
	}
	if operation == "search" {
		plan.SearchTerms = []string{value}
	}
	rows := resultquery.Retrieve(all, plan, request)
	if operation == "show" {
		// A result locator may also address a run_response; all explicit scope
		// and time/status filters still apply.
		rows = findHistoricalResult(all, request, value)
		if len(rows) == 0 {
			return resultsExit(2, fmt.Errorf("result %q was not found in the selected team/branch scope", value))
		}
		if len(rows) != 1 {
			return resultsExit(2, errors.New("result ID prefix is ambiguous; use the full result-id"))
		}
	}
	if isFinalResultsOperation(operation) {
		rows, err = selectFinalResults(all, request, value)
		if err != nil {
			return resultsExit(2, err)
		}
	}
	response.Matched = len(rows)
	limit := options.limit
	full := resultsReadFull(operation)
	if full {
		limit = 1
	}
	fingerprint, err := resultsFingerprint(options, operation, value, response.Coverage)
	if err != nil {
		return resultsExit(2, err)
	}
	start, err := decodeResultsCursor(options.cursor, fingerprint, len(rows))
	if err != nil {
		return resultsExit(2, err)
	}
	end := min(start+limit, len(rows))
	response.Results = rows[start:end]
	if end < len(rows) && !full {
		response.NextCursor, err = encodeResultsCursor(end, fingerprint)
		if err != nil {
			return err
		}
	}
	if operation == "report" {
		return presentRunReport(command, options, deps, response)
	}
	return presentResults(command, options, operation, response, full)
}

func resultsReadFull(operation string) bool {
	return slices.Contains([]string{"show", "latest", "final", "read", "report"}, operation)
}

func isFinalResultsOperation(operation string) bool {
	return slices.Contains([]string{"read", "final", "overview", "report"}, operation)
}

func validateResultsOptions(options *resultsCLIOptions, now time.Time) (resultquery.Request, error) {
	request := resultquery.Request{Team: options.team, Agent: options.agent, RunID: options.run, InvocationID: options.invocation, Statuses: options.statuses, Limit: options.limit, Now: now}
	options.agent = strings.ToLower(strings.TrimSpace(options.agent))
	request.Agent = options.agent
	if options.output != "text" && options.output != "json" {
		return request, errors.New("--output must be text or json")
	}
	if options.limit < 1 || options.limit > 50 {
		return request, errors.New("--limit must be between 1 and 50")
	}
	if options.allBranches && options.branch != "" {
		return request, errors.New("--branch and --all-branches are mutually exclusive")
	}
	if options.team != "" {
		canonical, err := workspacepkg.NormalizeTeamName(options.team)
		if err != nil {
			return request, err
		}
		options.team, request.Team = canonical, canonical
	}
	for _, status := range options.statuses {
		if !slices.Contains([]string{"done", "error", "blocked", "skipped", "protocol_incomplete"}, status) {
			return request, fmt.Errorf("unsupported terminal --status %q", status)
		}
	}
	var err error
	if options.since != "" {
		request.Since, err = time.Parse(time.RFC3339, options.since)
		if err != nil {
			return request, fmt.Errorf("invalid --since: %w", err)
		}
	}
	if options.until != "" {
		request.Until, err = time.Parse(time.RFC3339, options.until)
		if err != nil {
			return request, fmt.Errorf("invalid --until: %w", err)
		}
	}
	if !request.Since.IsZero() && !request.Until.IsZero() && !request.Since.Before(request.Until) {
		return request, errors.New("--since must precede --until")
	}
	location, err := time.LoadLocation(options.timezone)
	if err != nil {
		return request, fmt.Errorf("invalid --timezone: %w", err)
	}
	request.Now, request.Timezone = now.In(location), options.timezone
	return request, nil
}

func loadResultsScopes(ctx context.Context, options *resultsCLIOptions, deps resultsDeps) ([]inspectpkg.HistoricalSnapshot, error) {
	if options.workspace != "" {
		workspace, err := requireResolvedWorkspace(options.workspace)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", inspectpkg.ErrInvalidQuery, err)
		}
		snapshot, err := deps.load(ctx, inspectpkg.HistoricalResultScope{Workspace: workspace, Team: options.team, Branch: options.branch, AllBranches: options.allBranches})
		if err != nil {
			return nil, err
		}
		return []inspectpkg.HistoricalSnapshot{snapshot}, nil
	}
	registry, project, err := openAndResolveWorkspaceProject(ctx, deps.workspace, "")
	if err != nil {
		return nil, fmt.Errorf("%w: resolve registered project (or pass --workspace): %v", inspectpkg.ErrInvalidQuery, err)
	}
	defer func() { _ = registry.Close() }()
	workspaces, err := registry.ListWorkspaces(ctx, project.ID)
	if err != nil {
		return nil, err
	}
	result := []inspectpkg.HistoricalSnapshot{}
	for _, workspace := range workspaces {
		if workspace.State != "active" || options.team != "" && options.team != workspace.TeamName {
			continue
		}
		snapshot, err := deps.load(ctx, inspectpkg.HistoricalResultScope{Workspace: workspace.ControlRoot, Team: workspace.TeamName, Branch: options.branch, AllBranches: options.allBranches})
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("%w: no active registered workspace for the requested team", inspectpkg.ErrNotFound)
	}
	return result, nil
}

func findHistoricalResult(rows []inspectpkg.HistoricalResult, request resultquery.Request, prefix string) []inspectpkg.HistoricalResult {
	result := []inspectpkg.HistoricalResult{}
	if !strings.HasPrefix(prefix, "res-") || len(prefix) < 12 {
		return result
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.ResultID, prefix) {
			continue
		}
		// Allow both task and run response kinds without using an invocation
		// selector that could broaden the caller's authorization.
		request.IncludeRunResponses = true
		if len(resultquery.Retrieve([]inspectpkg.HistoricalResult{row}, resultquery.Plan{StatusBasis: "at_terminal"}, request)) == 1 {
			result = append(result, row)
		}
	}
	return result
}

type resultsCursor struct {
	Offset      int    `json:"offset"`
	Fingerprint string `json:"fingerprint"`
}

func resultsFingerprint(options *resultsCLIOptions, operation, value string, coverage []inspectpkg.HistoricalCoverage) (string, error) {
	copyOptions := *options
	copyOptions.cursor = ""
	// Explicit fields avoid hashing unexported CLI storage or the cursor itself.
	valueBytes, err := json.Marshal([]any{operation, value, copyOptions.team, copyOptions.agent, copyOptions.invocation, copyOptions.run, copyOptions.since, copyOptions.until, copyOptions.statuses, coverage})
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(valueBytes)
	return hex.EncodeToString(hash[:]), nil
}

func encodeResultsCursor(offset int, fingerprint string) (string, error) {
	encoded, err := json.Marshal(resultsCursor{Offset: offset, Fingerprint: fingerprint})
	return base64.RawURLEncoding.EncodeToString(encoded), err
}

func decodeResultsCursor(raw, fingerprint string, count int) (int, error) {
	if raw == "" {
		return 0, nil
	}
	if len(raw) > 1024 {
		return 0, errors.New("invalid --cursor")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return 0, errors.New("invalid --cursor encoding")
	}
	var cursor resultsCursor
	if err := json.Unmarshal(encoded, &cursor, json.RejectUnknownMembers(true)); err != nil {
		return 0, errors.New("invalid --cursor value")
	}
	if cursor.Fingerprint != fingerprint {
		return 0, errors.New("cursor scope or event snapshot changed; start a new list/search")
	}
	if cursor.Offset < 0 || cursor.Offset > count {
		return 0, errors.New("cursor offset is outside the results")
	}
	return cursor.Offset, nil
}

func writeResults(writer io.Writer, format string, response resultsEnvelope, full bool) error {
	if format == "json" {
		encoded, err := json.Marshal(response, jsonv1.FormatDurationAsNano(true))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(writer, string(encoded))
		return err
	}
	if _, err := fmt.Fprintf(writer, "Historical results: %d matched (team: %s)\n", response.Matched, cmpTeamLabel(response.Team)); err != nil {
		return err
	}
	for _, coverage := range response.Coverage {
		if _, err := fmt.Fprintf(writer, "Scope: %s · team=%s · branch=%s · all-branches=%t · index=%s\n", safeResultsText(coverage.Scope.Workspace), cmpTeamLabel(coverage.Scope.Team), safeResultsText(coverage.Scope.Branch), coverage.Scope.AllBranches, coverage.IndexState); err != nil {
			return err
		}
	}
	for _, row := range response.Results {
		if _, err := fmt.Fprintf(writer, "\n%s  %s  %s/%s  %s (%s)\n", row.CompletedAt.Format(time.RFC3339), safeResultsText(row.ResultID), safeResultsText(row.Team), safeResultsText(row.Agent), safeResultsText(row.Status), safeResultsText(row.Validity)); err != nil {
			return err
		}
		if err := writeResultsRequest(writer, row, full); err != nil {
			return err
		}
		if row.Kind == "task" {
			description := operatorpkg.SafeDisplayText(row.Description, 300)
			if full {
				description = safeResultsText(row.Description)
			}
			if _, err := fmt.Fprintln(writer, "Agent task: "+description); err != nil {
				return err
			}
		}
		if !full {
			if _, err := fmt.Fprintln(writer, "Result (excerpt): "+operatorpkg.SafeDisplayText(firstNonEmpty(row.Summary, row.Output, "(No result text saved.)"), 300)); err != nil {
				return err
			}
		}
		if err := writeResultKind(writer, row, response.Coverage); err != nil {
			return err
		}
		if full {
			if _, err := fmt.Fprintln(writer, formatHistoricalDeliverable(row)); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(writer, "View: %s\n", safeResultsText(resultsScopedCommand("show", row, response.Coverage))); err != nil {
			return err
		}
	}
	if response.NextCursor != "" {
		if _, err := fmt.Fprintf(writer, "\nNext page: --cursor %s\n", response.NextCursor); err != nil {
			return err
		}
	}
	return nil
}

func cmpTeamLabel(team string) string {
	if team == "" {
		return "selected project"
	}
	return safeResultsText(team)
}

func resultsTeamArgument(team string) string {
	if team == "" {
		return ""
	}
	return " --team '" + strings.ReplaceAll(team, "'", "'\\''") + "'"
}

func safeResultsText(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for index, line := range lines {
		lines[index] = operatorpkg.SafeDisplayText(line, len([]rune(line))+1)
	}
	return strings.Join(lines, "\n")
}

func resultsExit(code int, err error) error {
	return &inspectExitError{code: code, err: fmt.Errorf("hufu results: %w", err)}
}
