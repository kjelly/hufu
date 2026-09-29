package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/teampkg"
)

func newTeamPackCommand() *cobra.Command {
	var version string
	var output string
	command := &cobra.Command{
		Use:   "pack <team-directory>",
		Short: "Build a deterministic portable team package",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			result, err := teampkg.Pack(teampkg.PackOptions{
				TeamDir: args[0], Version: version, Output: output,
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "packed team %s version %s to %s (%s, %d files)\n",
				result.Manifest.Name, result.Manifest.Version, result.Output, result.Lock.TeamDigest, result.FileCount)
			return err
		},
	}
	command.Flags().StringVar(&version, "version", "", "Package version label")
	command.Flags().StringVar(&output, "output", "", "Output .hufu archive path")
	_ = command.MarkFlagRequired("version")
	_ = command.MarkFlagRequired("output")
	return command
}

func newTeamPackageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "package",
		Short: "Inspect portable team packages",
	}
	command.AddCommand(newTeamPackageInspectCommand())
	return command
}

func newTeamPackageInspectCommand() *cobra.Command {
	var format string
	command := &cobra.Command{
		Use:   "inspect <file.hufu>",
		Short: "Validate and describe a portable team package",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if format != "text" && format != "json" {
				return fmt.Errorf("unsupported format %q (expected text or json)", format)
			}
			report, inspectErr := teampkg.InspectPackage(args[0])
			var renderErr error
			if format == "json" {
				renderErr = renderTeamPackageJSON(command.OutOrStdout(), report)
			} else {
				renderErr = renderTeamPackageText(command.OutOrStdout(), report)
			}
			if renderErr != nil {
				return renderErr
			}
			return inspectErr
		},
	}
	command.Flags().StringVar(&format, "format", "text", "Report format: text or json")
	return command
}

func renderTeamPackageJSON(writer io.Writer, report teampkg.InspectionReport) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func renderTeamPackageText(writer io.Writer, report teampkg.InspectionReport) error {
	lines := []string{
		"Package",
		fmt.Sprintf("  schema: %d", report.PackageSchema),
		"  name: " + report.Name,
		"  version: " + report.Version,
		"  authenticity: " + report.Authenticity,
		"  integrity: " + report.Integrity,
		"  archive_sha256: " + report.ArchiveSHA256,
		"  team_digest: " + report.TeamDigest,
		"  team_manifest: " + report.TeamManifest,
		"  manifest_schema: " + report.ManifestSchema,
		fmt.Sprintf("  files: %d", report.FileCount),
		fmt.Sprintf("  archive_bytes: %d", report.ArchiveBytes),
		fmt.Sprintf("  uncompressed_bytes: %d", report.TotalUncompressedBytes),
		"Compile",
		"  status: " + report.Compile.Status,
	}
	if report.Compile.ErrorCategory != "" {
		lines = append(lines, "  error_category: "+report.Compile.ErrorCategory)
	}
	if report.Compile.Message != "" {
		lines = append(lines, "  message: "+report.Compile.Message)
	}
	lines = append(lines, "Team", "  normalized_name: "+report.NormalizedTeamName, "  agents:")
	for _, item := range report.Agents {
		lines = append(lines, fmt.Sprintf("    - %s (%s)", item.Name, item.Role))
	}
	lines = append(lines,
		"Files",
	)
	for _, item := range report.Files {
		lines = append(lines, fmt.Sprintf("  - %s (%d bytes, %s)", item.Path, item.Size, item.SHA256))
	}
	lines = append(lines,
		"Included",
		fmt.Sprintf("  skills: %d", len(report.Included.Skills)),
	)
	for _, item := range report.Included.Skills {
		lines = append(lines, fmt.Sprintf("    - %s (%s)", item.Name, item.Path))
	}
	lines = append(lines, fmt.Sprintf("  result_schemas: %d", len(report.Included.ResultSchemas)))
	for _, item := range report.Included.ResultSchemas {
		lines = append(lines, "    - "+item)
	}
	lines = append(lines, fmt.Sprintf("  go_actions: %d", len(report.Included.GoActions)))
	for _, item := range report.Included.GoActions {
		lines = append(lines, fmt.Sprintf("    - %s (%s)", item.Capability, item.Source))
	}
	lines = append(lines, "External requirements", fmt.Sprintf("  executables: %d", len(report.External.Executables)))
	for _, item := range report.External.Executables {
		lines = append(lines, fmt.Sprintf("    - %s: %s", item.Owner, item.Command))
	}
	lines = append(lines, fmt.Sprintf("  mcp: %d", len(report.External.MCP)))
	for _, item := range report.External.MCP {
		detail := item.Command
		if detail == "" {
			detail = item.URL
		}
		lines = append(lines, fmt.Sprintf("    - %s (%s): %s", item.Name, item.Type, detail))
	}
	lines = append(lines, fmt.Sprintf("  project_resources: %d", len(report.External.ProjectResources)))
	for _, item := range report.External.ProjectResources {
		lines = append(lines, fmt.Sprintf("    - %s (%s): %s", item.Name, item.Kind, item.Path))
	}
	lines = append(lines, fmt.Sprintf("  skills: %d", len(report.External.Skills)))
	for _, item := range report.External.Skills {
		name := item.Name
		if name == "" {
			name = item.Reference
		}
		lines = append(lines, fmt.Sprintf("    - %s (available=%t)", name, item.Available))
	}
	lines = append(lines, fmt.Sprintf("  paths: %d", len(report.External.Paths)))
	for _, item := range report.External.Paths {
		lines = append(lines, fmt.Sprintf("    - %s: %s", item.Owner, item.Path))
	}
	lines = append(lines, fmt.Sprintf("Findings: %d", len(report.Findings)))
	for _, finding := range report.Findings {
		lines = append(lines, fmt.Sprintf("  - category=%s path=%s field=%s", finding.Category, finding.Path, finding.Field))
	}
	_, err := fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}

func init() {
	teamCmd.AddCommand(newTeamPackCommand(), newTeamPackageCommand())
}
