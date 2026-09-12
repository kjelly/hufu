package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/evalharness"
)

// evalExitError signals a non-zero process exit for a completed, correctly
// reported eval run (one or more cases failed), distinct from a cobra RunE
// error, which means the harness itself could not produce a report.
type evalExitError struct{ code int }

func (e *evalExitError) Error() string        { return "one or more eval cases failed" }
func (e *evalExitError) ProcessExitCode() int { return e.code }

var (
	evalCaseID string
	evalFormat string
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Run the offline deterministic workflow regression harness",
	Long: `hufu eval runs deterministic, offline regression suites against a real
team.Coordinator driven by a scripted model provider -- see
docs/archive/implementation-plans/workflow-regression-eval-harness.md. It asserts on runtime
semantics (routing, task lifecycle, retry, recovery, acceptance, events,
artifacts, outcome), never on model output quality, and never calls a live
model or the network.`,
	Args: cobra.NoArgs,
}

var evalListCmd = &cobra.Command{
	Use:   "list [suite-dir]",
	Short: "List the cases declared by every suite fixture under a directory",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runEvalList,
}

var evalRunCmd = &cobra.Command{
	Use:   "run <suite-dir>",
	Short: "Run every case in every suite fixture under a directory",
	Args:  cobra.ExactArgs(1),
	RunE:  runEvalRun,
}

func init() {
	evalRunCmd.Flags().StringVar(&evalCaseID, "case", "", "Run only the case with this id (across every fixture found)")
	evalRunCmd.Flags().StringVar(&evalFormat, "format", "text", "Report format: text or json")
	evalCmd.AddCommand(evalListCmd)
	evalCmd.AddCommand(evalRunCmd)
}

func loadEvalSuites(dir string) ([]*evalharness.SuiteFixture, error) {
	paths, err := evalharness.DiscoverSuiteFixtures(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no suite fixtures (*.yaml/*.yml) found in %s", dir)
	}
	fixtures := make([]*evalharness.SuiteFixture, 0, len(paths))
	for _, path := range paths {
		fixture, err := evalharness.LoadSuiteFixture(path)
		if err != nil {
			return nil, err
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures, nil
}

func runEvalList(cmd *cobra.Command, args []string) error {
	dir := "./evals"
	if len(args) == 1 {
		dir = args[0]
	}
	fixtures, err := loadEvalSuites(dir)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	for _, fixture := range fixtures {
		for _, c := range fixture.Cases {
			_, _ = fmt.Fprintf(out, "%s/%s\n", fixture.Name, c.ID)
		}
	}
	return nil
}

func runEvalRun(cmd *cobra.Command, args []string) error {
	format := strings.ToLower(strings.TrimSpace(evalFormat))
	if format != "text" && format != "json" {
		return fmt.Errorf("unsupported --format %q (want text or json)", evalFormat)
	}
	fixtures, err := loadEvalSuites(args[0])
	if err != nil {
		return err
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	suiteResults := make([]evalharness.EvalSuiteResult, 0, len(fixtures))
	foundCase := evalCaseID == ""
	for _, fixture := range fixtures {
		result, err := evalharness.RunSuite(ctx, fixture, evalCaseID)
		if err != nil {
			if evalCaseID != "" && errors.Is(err, evalharness.ErrCaseNotFound) {
				continue
			}
			return fmt.Errorf("run suite %s: %w", fixture.Name, err)
		}
		foundCase = true
		if len(result.Cases) > 0 {
			suiteResults = append(suiteResults, result)
		}
	}
	if !foundCase {
		return fmt.Errorf("case %q not found in any suite fixture under %s", evalCaseID, args[0])
	}

	out := cmd.OutOrStdout()
	if format == "json" {
		if err := evalharness.WriteJSONReport(out, suiteResults); err != nil {
			return fmt.Errorf("write json report: %w", err)
		}
	} else {
		evalharness.WriteTextReport(out, suiteResults)
	}

	if !evalharness.AllPassed(suiteResults) {
		return &evalExitError{code: 1}
	}
	return nil
}
