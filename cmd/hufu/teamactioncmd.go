package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	internalteam "github.com/kjelly/hufu/internal/team"
)

type teamActionOptions struct {
	team   string
	output string
	json   bool
}

var teamActionOpts teamActionOptions

var teamActionCmd = &cobra.Command{Use: "action", Short: "Inspect the team's action catalog"}
var teamActionListCmd = &cobra.Command{
	Use: "list [team-directory]", Short: "List action catalog entries", Args: cobra.MaximumNArgs(1),
	RunE: func(command *cobra.Command, args []string) error { return runTeamActionList(command, args, os.Stdout) },
}
var teamActionShowCmd = &cobra.Command{
	Use: "show <action-id> [team-directory]", Short: "Show one action catalog entry", Args: cobra.RangeArgs(1, 2),
	RunE: func(command *cobra.Command, args []string) error { return runTeamActionShow(command, args, os.Stdout) },
}

func init() {
	teamCmd.AddCommand(teamActionCmd)
	teamActionCmd.AddCommand(teamActionListCmd, teamActionShowCmd)
	for _, command := range []*cobra.Command{teamActionListCmd, teamActionShowCmd} {
		command.Flags().StringVar(&teamActionOpts.team, "team", "", "Discoverable team name")
		command.Flags().StringVar(&teamActionOpts.output, "output", "text", "Output format: text or json")
		command.Flags().BoolVar(&teamActionOpts.json, "json", false, "Alias for --output json")
	}
}

// teamActionDTO is the operator view of a catalog entry. It shows the
// capability and type but never the provider's command, source, dir, or
// runtime.
type teamActionDTO struct {
	ID              string                       `json:"id"`
	Description     string                       `json:"description"`
	Capability      string                       `json:"capability"`
	Type            string                       `json:"type"`
	Agent           string                       `json:"agent"`
	SideEffect      string                       `json:"side_effect"`
	Recovery        string                       `json:"recovery"`
	InputSchema     *internalteam.RunInputSchema `json:"input_schema,omitempty"`
	OutputSchema    *internalteam.RunInputSchema `json:"output_schema,omitempty"`
	Discover        []string                     `json:"discover"`
	Propose         []string                     `json:"propose"`
	RequireProposal bool                         `json:"require_proposal"`
	AllowUnattended bool                         `json:"allow_unattended"`
	MaxInvocations  int                          `json:"max_invocations"`
	EntryHash       string                       `json:"entry_hash"`
}

type teamActionListDTO struct {
	CatalogHash string          `json:"catalog_hash,omitempty"`
	Actions     []teamActionDTO `json:"actions"`
}

func newTeamActionDTO(entry internalteam.ActionCatalogEntry, withSchemas bool) teamActionDTO {
	dto := teamActionDTO{
		ID: entry.ID, Description: entry.Description, Capability: entry.Capability, Type: entry.Type, Agent: entry.Agent,
		SideEffect: string(entry.SideEffect), Recovery: string(entry.Recovery),
		Discover: append([]string{}, entry.Discover...), Propose: append([]string{}, entry.Propose...),
		RequireProposal: entry.RequireProposal, AllowUnattended: entry.AllowUnattended,
		MaxInvocations: entry.MaxInvocations, EntryHash: entry.Hash,
	}
	if withSchemas {
		input := entry.InputSchema
		dto.InputSchema = &input
		dto.OutputSchema = entry.OutputSchema
	}
	return dto
}

func loadTeamActionCatalog(command *cobra.Command, args []string) (*internalteam.ActionCatalogSnapshot, string, error) {
	format, err := resolveOutputAlias("output", teamActionOpts.output, flagChanged(command, "output"), "json",
		outputForJSONAlias(teamActionOpts.json), flagChanged(command, "json"), "text", []string{"text", "json"})
	if err != nil {
		return nil, "", err
	}
	teamDir, err := resolveTeamDirArg(args, teamActionOpts.team)
	if err != nil {
		return nil, "", err
	}
	spec, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		return nil, "", err
	}
	return spec.RuntimeSession().ActionCatalog, format, nil
}

func runTeamActionList(command *cobra.Command, args []string, out io.Writer) error {
	catalog, format, err := loadTeamActionCatalog(command, args)
	if err != nil {
		return err
	}
	list := teamActionListDTO{Actions: []teamActionDTO{}}
	if catalog != nil {
		list.CatalogHash = catalog.Hash
		for _, entry := range catalog.Entries {
			list.Actions = append(list.Actions, newTeamActionDTO(entry, false))
		}
	}
	if format == "json" {
		return writeTeamActionJSON(out, list)
	}
	if len(list.Actions) == 0 {
		_, err := fmt.Fprintln(out, "no action catalog entries")
		return err
	}
	writer := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "ID\tCAPABILITY\tTYPE\tAGENT\tSIDE EFFECT\tRECOVERY\tPROPOSAL\tMAX"); err != nil {
		return err
	}
	for _, action := range list.Actions {
		proposal := "optional"
		if action.RequireProposal {
			proposal = "required"
		}
		if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\n", action.ID, action.Capability, action.Type, action.Agent,
			action.SideEffect, action.Recovery, proposal, action.MaxInvocations); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func runTeamActionShow(command *cobra.Command, args []string, out io.Writer) error {
	id, teamArgs := args[0], args[1:]
	catalog, format, err := loadTeamActionCatalog(command, teamArgs)
	if err != nil {
		return err
	}
	entry, ok := catalog.Lookup(id)
	if !ok {
		return fmt.Errorf("action %q is not defined in this team's action catalog", id)
	}
	dto := newTeamActionDTO(entry, true)
	if format == "json" {
		return writeTeamActionJSON(out, dto)
	}
	return writeTeamActionText(out, dto)
}

func writeTeamActionJSON(out io.Writer, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode action catalog: %w", err)
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

func writeTeamActionText(out io.Writer, dto teamActionDTO) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\ndescription: %s\ncapability: %s\ntype: %s\nagent: %s\n", dto.ID, dto.Description, dto.Capability, dto.Type, dto.Agent)
	fmt.Fprintf(&b, "side-effect: %s\nrecovery: %s\n", dto.SideEffect, dto.Recovery)
	fmt.Fprintf(&b, "discover: %s\npropose: %s\n", joinOrDash(dto.Discover), joinOrDash(dto.Propose))
	fmt.Fprintf(&b, "require-proposal: %t\nallow-unattended: %t\nmax-invocations: %d\n", dto.RequireProposal, dto.AllowUnattended, dto.MaxInvocations)
	fmt.Fprintf(&b, "entry-hash: %s\n", dto.EntryHash)
	for _, schema := range []struct {
		label string
		value *internalteam.RunInputSchema
	}{{"input-schema", dto.InputSchema}, {"output-schema", dto.OutputSchema}} {
		if schema.value == nil {
			continue
		}
		encoded, err := json.MarshalIndent(schema.value, "", "  ")
		if err != nil {
			return fmt.Errorf("encode %s: %w", schema.label, err)
		}
		fmt.Fprintf(&b, "%s: %s\n", schema.label, encoded)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func joinOrDash(values []string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(values, ", ")
}
