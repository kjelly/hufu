package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
)

var (
	teamLintName   string
	teamLintFormat string
	teamLintFailOn string
)

type teamLintExitError struct {
	code int
	msg  string
}

func (e *teamLintExitError) Error() string        { return e.msg }
func (e *teamLintExitError) ProcessExitCode() int { return e.code }

type teamLintSummary struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
	Ignored int `json:"ignored"`
}

type teamLintJSONError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type teamLintJSONDocument struct {
	SchemaVersion int                             `json:"schema_version"`
	Team          string                          `json:"team"`
	Complete      bool                            `json:"complete"`
	Findings      *[]internalteam.TeamLintFinding `json:"findings,omitempty"`
	Summary       *teamLintSummary                `json:"summary,omitempty"`
	Error         *teamLintJSONError              `json:"error,omitempty"`
}

var teamLintCmd = &cobra.Command{
	Use:   "lint [team-directory]",
	Short: "Report deterministic team authoring drift",
	Long: `Inspect a team definition without calling a model, opening a network
connection, creating a workspace, starting MCP, or executing verifier commands.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) <= 1 {
			return nil
		}
		cmd.Root().SilenceErrors = true
		return &teamLintExitError{code: 2, msg: "accepts at most one team-directory argument"}
	},
	RunE: runTeamLint,
}

func init() {
	teamCmd.AddCommand(teamLintCmd)
	teamLintCmd.Flags().StringVar(&teamLintName, "team", "", "Discoverable team name to lint")
	teamLintCmd.Flags().StringVar(&teamLintFormat, "format", "text", "Output format: text or json")
	teamLintCmd.Flags().StringVar(&teamLintFailOn, "fail-on", internalteam.FindingSeverityError, "Lowest finding severity that fails: error, warning, info, or none")
	teamLintCmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		cmd.Root().SilenceErrors = true
		return &teamLintExitError{code: 2, msg: err.Error()}
	})
}

func runTeamLint(cmd *cobra.Command, args []string) error {
	format := strings.ToLower(strings.TrimSpace(teamLintFormat))
	if format != "text" && format != "json" {
		return lintCLIError(cmd, format, "invalid --format: use text or json")
	}
	failOn := strings.ToLower(strings.TrimSpace(teamLintFailOn))
	if failOn != internalteam.FindingSeverityError && failOn != internalteam.FindingSeverityWarning && failOn != internalteam.FindingSeverityInfo && failOn != "none" {
		return lintCLIError(cmd, format, "invalid --fail-on: use error, warning, info, or none")
	}
	teamDir, err := resolveTeamDirArg(args, teamLintName)
	if err != nil {
		return lintCLIError(cmd, format, err.Error())
	}
	result, err := internalteam.LintTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		if format == "json" {
			_ = writeTeamLintJSON(cmd.OutOrStdout(), teamLintJSONDocument{
				SchemaVersion: 1, Team: "", Complete: false,
				Error: &teamLintJSONError{Kind: "load_error", Message: err.Error()},
			})
		}
		cmd.Root().SilenceErrors = true
		return &teamLintExitError{code: 2, msg: err.Error()}
	}
	if format == "json" {
		doc := teamLintJSONDocument{
			SchemaVersion: 1, Team: result.Team, Complete: result.Complete,
			Findings: &result.Findings, Summary: summarizeTeamLint(result.Findings),
		}
		if err := writeTeamLintJSON(cmd.OutOrStdout(), doc); err != nil {
			cmd.Root().SilenceErrors = true
			return &teamLintExitError{code: 2, msg: fmt.Sprintf("encode team lint JSON: %v", err)}
		}
	} else if err := writeTeamLintText(cmd.OutOrStdout(), result); err != nil {
		cmd.Root().SilenceErrors = true
		return &teamLintExitError{code: 2, msg: fmt.Sprintf("write team lint output: %v", err)}
	}
	if internalteam.TeamLintReachesThreshold(result.Findings, failOn) {
		cmd.Root().SilenceErrors = true
		return &teamLintExitError{code: 1, msg: "team lint findings reached the configured threshold"}
	}
	return nil
}

func lintCLIError(cmd *cobra.Command, format, message string) error {
	if format == "json" {
		_ = writeTeamLintJSON(cmd.OutOrStdout(), teamLintJSONDocument{
			SchemaVersion: 1, Complete: false,
			Error: &teamLintJSONError{Kind: "cli_error", Message: message},
		})
	}
	cmd.Root().SilenceErrors = true
	return &teamLintExitError{code: 2, msg: message}
}

func writeTeamLintJSON(w io.Writer, document teamLintJSONDocument) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

func writeTeamLintText(w io.Writer, result internalteam.TeamLintResult) error {
	if len(result.Findings) == 0 {
		_, err := fmt.Fprintf(w, "team %s: no findings\n", result.Team)
		return err
	}
	for _, finding := range result.Findings {
		location := finding.File
		if location == "" {
			location = finding.FieldPath
		}
		if finding.Line > 0 {
			location += fmt.Sprintf(":%d", finding.Line)
			if finding.Column > 0 {
				location += fmt.Sprintf(":%d", finding.Column)
			}
		}
		if _, err := fmt.Fprintf(w, "%s: %s %s: %s\n", location, finding.Severity, finding.Code, finding.Message); err != nil {
			return err
		}
	}
	return nil
}

func summarizeTeamLint(findings []internalteam.TeamLintFinding) *teamLintSummary {
	summary := new(teamLintSummary)
	for _, finding := range findings {
		switch finding.Severity {
		case internalteam.FindingSeverityError:
			summary.Error++
		case internalteam.FindingSeverityWarning:
			summary.Warning++
		case internalteam.FindingSeverityInfo:
			summary.Info++
		}
		if finding.Ignored {
			summary.Ignored++
		}
	}
	return summary
}
