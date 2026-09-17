package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	internalteam "github.com/kjelly/hufu/internal/team"
)

var (
	teamProfileTeam   string
	teamProfileFormat string
)

var teamProfileCmd = &cobra.Command{Use: "profile", Short: "Inspect normalized decision profiles"}
var teamProfileListCmd = &cobra.Command{Use: "list [team-directory]", Args: cobra.MaximumNArgs(1), RunE: runTeamProfileList}
var teamProfileShowCmd = &cobra.Command{Use: "show <profile> [team-directory]", Args: cobra.RangeArgs(1, 2), RunE: runTeamProfileShow}
var teamProfilePlanCmd = &cobra.Command{Use: "plan <profile> [team-directory]", Args: cobra.RangeArgs(1, 2), RunE: runTeamProfilePlan}

func init() {
	teamCmd.AddCommand(teamProfileCmd)
	teamProfileCmd.AddCommand(teamProfileListCmd, teamProfileShowCmd, teamProfilePlanCmd)
	for _, cmd := range []*cobra.Command{teamProfileListCmd, teamProfileShowCmd, teamProfilePlanCmd} {
		cmd.Flags().StringVar(&teamProfileTeam, "team", "", "Discoverable team name")
		cmd.Flags().StringVar(&teamProfileFormat, "format", "text", "Output format: text, yaml, or json")
	}
}

type teamProfileDTO struct {
	Name    string `json:"name" yaml:"name"`
	Origin  string `json:"origin" yaml:"origin"`
	Ref     string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

func loadTeamProfileProjection(args []string) (internalteam.DecisionAuthoringProjection, error) {
	teamDir, err := resolveTeamDirArg(args, teamProfileTeam)
	if err != nil {
		return internalteam.DecisionAuthoringProjection{}, err
	}
	spec, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
	if err != nil {
		return internalteam.DecisionAuthoringProjection{}, err
	}
	return spec.Decision, nil
}

func emitTeamProfile(format string, value any) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		var text string
		switch v := value.(type) {
		case teamProfileDTO:
			text = fmt.Sprintf("%s\norigin: %s\nref: %s\nversion: %s\n", v.Name, v.Origin, shortOrDash(v.Ref), shortOrDash(v.Version))
		case teamProfilePlanDTO:
			text = fmt.Sprintf("profile: %s\norigin: %s\nref: %s\nplan: %#v\n", v.Name, v.Origin, shortOrDash(v.Ref), v.Plan)
		case string:
			text = v
		default:
			text = fmt.Sprint(v)
		}
		_, err := fmt.Fprint(os.Stdout, text)
		return err
	case "json":
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Println(string(data))
		return err
	case "yaml":
		data, err := yaml.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Print(string(data))
		return err
	default:
		return fmt.Errorf("unknown --format %q: supported formats are text, yaml, json", format)
	}
}

func runTeamProfileList(_ *cobra.Command, args []string) error {
	projection, err := loadTeamProfileProjection(args)
	if err != nil {
		return err
	}
	profiles := make([]teamProfileDTO, 0, len(projection.Profiles))
	for _, profile := range projection.Profiles {
		profiles = append(profiles, teamProfileDTO{Name: profile.Name, Origin: profile.Origin, Ref: profile.Ref, Version: profile.Version})
	}
	slices.SortFunc(profiles, func(a, b teamProfileDTO) int { return strings.Compare(a.Name, b.Name) })
	if strings.EqualFold(strings.TrimSpace(teamProfileFormat), "text") || strings.TrimSpace(teamProfileFormat) == "" {
		var b strings.Builder
		for _, profile := range profiles {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", profile.Name, profile.Origin, shortOrDash(profile.Ref), shortOrDash(profile.Version))
		}
		return emitTeamProfile(teamProfileFormat, b.String())
	}
	return emitTeamProfile(teamProfileFormat, profiles)
}

func profileArgAndTeam(args []string) (string, []string) {
	if len(args) == 1 {
		return args[0], nil
	}
	return args[0], args[1:]
}

func runTeamProfileShow(_ *cobra.Command, args []string) error {
	name, teamArgs := profileArgAndTeam(args)
	projection, err := loadTeamProfileProjection(teamArgs)
	if err != nil {
		return err
	}
	for _, profile := range projection.Profiles {
		if profile.Name != name {
			continue
		}
		return emitTeamProfile(teamProfileFormat, profile)
	}
	return fmt.Errorf("decision profile %q is not defined in this team", name)
}

type teamProfilePlanDTO struct {
	Name   string                             `json:"name" yaml:"name"`
	Origin string                             `json:"origin" yaml:"origin"`
	Ref    string                             `json:"ref,omitempty" yaml:"ref,omitempty"`
	Plan   internalteam.DecisionExecutionPlan `json:"plan" yaml:"plan"`
}

func runTeamProfilePlan(_ *cobra.Command, args []string) error {
	name, teamArgs := profileArgAndTeam(args)
	projection, err := loadTeamProfileProjection(teamArgs)
	if err != nil {
		return err
	}
	for _, profile := range projection.Profiles {
		if profile.Name != name {
			continue
		}
		return emitTeamProfile(teamProfileFormat, teamProfilePlanDTO{Name: profile.Name, Origin: profile.Origin, Ref: profile.Ref, Plan: profile.Plan})
	}
	return fmt.Errorf("decision profile %q is not defined in this team", name)
}
