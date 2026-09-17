package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	internalteam "github.com/kjelly/hufu/internal/team"
)

var decisionProfileTeam string
var decisionProfileName string

type decisionProfileIdentityView struct {
	RequestedName string `json:"requested_name" yaml:"requested_name"`
	ResolvedRef   string `json:"resolved_ref,omitempty" yaml:"resolved_ref,omitempty"`
	Origin        string `json:"origin,omitempty" yaml:"origin,omitempty"`
	Version       string `json:"version,omitempty" yaml:"version,omitempty"`
	PolicyDigest  string `json:"policy_digest,omitempty" yaml:"policy_digest,omitempty"`
	BundleDigest  string `json:"bundle_digest,omitempty" yaml:"bundle_digest,omitempty"`
}

type decisionPlanView struct {
	Proposal        bool   `json:"proposal" yaml:"proposal"`
	Reference       bool   `json:"reference" yaml:"reference"`
	JudgeCount      int    `json:"judge_count" yaml:"judge_count"`
	Aggregation     string `json:"aggregation,omitempty" yaml:"aggregation,omitempty"`
	ChallengeCount  int    `json:"challenge_count" yaml:"challenge_count"`
	Revision        bool   `json:"revision" yaml:"revision"`
	Premortem       bool   `json:"premortem" yaml:"premortem"`
	Forecast        bool   `json:"forecast" yaml:"forecast"`
	Finalization    string `json:"finalization,omitempty" yaml:"finalization,omitempty"`
	PolicyMaxTokens int64  `json:"policy_max_tokens,omitempty" yaml:"policy_max_tokens,omitempty"`
	StopMaxTokens   int64  `json:"stop_max_tokens,omitempty" yaml:"stop_max_tokens,omitempty"`
}

type decisionProfileView struct {
	SchemaVersion  int                                    `json:"schema_version" yaml:"schema_version"`
	Kind           string                                 `json:"kind" yaml:"kind"`
	Enabled        bool                                   `json:"enabled" yaml:"enabled"`
	Identity       decisionProfileIdentityView            `json:"identity" yaml:"identity"`
	Plan           *decisionPlanView                      `json:"plan,omitempty" yaml:"plan,omitempty"`
	RoleResolution *agent.DecisionProfileRoleResolutionV2 `json:"role_resolution,omitempty" yaml:"role_resolution,omitempty"`
	Evidence       *agent.DecisionProfileEvidenceV2       `json:"evidence,omitempty" yaml:"evidence,omitempty"`
	Limits         *agent.DecisionProfileLimitsV2         `json:"limits,omitempty" yaml:"limits,omitempty"`
}

type decisionProfileListItemView struct {
	Name       string `json:"name" yaml:"name"`
	Kind       string `json:"kind" yaml:"kind"`
	ResolvesTo string `json:"resolves_to,omitempty" yaml:"resolves_to,omitempty"`
}

type decisionProfileListView struct {
	SchemaVersion int                           `json:"schema_version" yaml:"schema_version"`
	Kind          string                        `json:"kind" yaml:"kind"`
	Profiles      []decisionProfileListItemView `json:"profiles" yaml:"profiles"`
}

func decisionPlanViewFromPlan(plan internalteam.DecisionExecutionPlan, policy agent.DecisionPolicy) *decisionPlanView {
	return &decisionPlanView{Proposal: plan.Proposal, Reference: plan.Reference, JudgeCount: plan.JudgeCount, Aggregation: plan.Aggregation, ChallengeCount: plan.ChallengeCount, Revision: plan.Revision, Premortem: plan.Premortem, Forecast: plan.Forecast, Finalization: plan.Finalization, PolicyMaxTokens: policy.MaxTokens, StopMaxTokens: policy.Discipline.Stop.MaxTokens}
}

var decisionProfileCmd = &cobra.Command{Use: "profile", Short: "Inspect built-in and team decision profiles"}
var decisionProfileListCmd = &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: runDecisionProfileList}
var decisionProfileShowCmd = &cobra.Command{Use: "show <profile>", Args: cobra.ExactArgs(1), RunE: runDecisionProfileShow}
var decisionPlanCmd = &cobra.Command{Use: "plan", Short: "Show the stage plan for a decision profile", Args: cobra.NoArgs, RunE: runDecisionPlan}

func resolveDecisionTeamDir(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("team name is required")
	}
	if info, err := os.Stat(name); err == nil && info.IsDir() {
		return name, nil
	}
	return resolveTeamDirArg(nil, name)
}

func init() {
	decisionCmd.AddCommand(decisionProfileCmd, decisionPlanCmd)
	decisionProfileCmd.AddCommand(decisionProfileListCmd, decisionProfileShowCmd)
	decisionProfileListCmd.Flags().StringVar(&decisionProfileTeam, "team", "", "Discoverable team name or team directory")
	decisionProfileShowCmd.Flags().StringVar(&decisionProfileTeam, "team", "", "Discoverable team name or team directory")
	decisionPlanCmd.Flags().StringVar(&decisionProfileName, "profile", "", "Decision profile name (required)")
	decisionPlanCmd.Flags().StringVar(&decisionProfileTeam, "team", "", "Discoverable team name or team directory")
	_ = decisionPlanCmd.MarkFlagRequired("profile")
}

func decisionAliasRef(name string) string {
	switch name {
	case "light":
		return agent.DecisionProfileBuiltinLightV1
	case "standard":
		return agent.DecisionProfileBuiltinStandardV1
	case "high-stakes":
		return agent.DecisionProfileBuiltinHighStakesV1
	default:
		return ""
	}
}

func runDecisionProfileList(cmd *cobra.Command, _ []string) error {
	items := []decisionProfileListItemView{{Name: agent.DecisionProfileOff, Kind: "off"}}
	for _, metadata := range agent.BuiltInDecisionProfileMetadata() {
		items = append(items, decisionProfileListItemView{Name: metadata.Ref, Kind: "builtin", ResolvesTo: metadata.Ref})
		name := strings.TrimSuffix(strings.TrimPrefix(metadata.Ref, "builtin/"), "@v1")
		if name == "high-stakes" || name == "standard" || name == "light" {
			items = append(items, decisionProfileListItemView{Name: name, Kind: "alias", ResolvesTo: metadata.Ref})
		}
	}
	if strings.TrimSpace(decisionProfileTeam) != "" {
		teamDir, err := resolveDecisionTeamDir(decisionProfileTeam)
		if err != nil {
			return err
		}
		spec, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
		if err != nil {
			return err
		}
		local := make(map[string]decisionProfileListItemView)
		for _, profile := range spec.Decision.Profiles {
			if profile.Local {
				local[profile.Name] = decisionProfileListItemView{Name: profile.Name, Kind: "local", ResolvesTo: profile.Ref}
			}
		}
		filtered := items[:0]
		for _, item := range items {
			if replacement, ok := local[item.Name]; ok && item.Kind == "alias" {
				filtered = append(filtered, replacement)
				delete(local, item.Name)
			} else {
				filtered = append(filtered, item)
			}
		}
		for _, item := range local {
			filtered = append(filtered, item)
		}
		items = filtered
	}
	slices.SortFunc(items, func(a, b decisionProfileListItemView) int { return cmp.Compare(a.Name, b.Name) })
	view := decisionProfileListView{SchemaVersion: 1, Kind: "decision_profile_list", Profiles: items}
	if decisionJSON {
		return writeDecisionJSON(cmd, view)
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.Name, item.Kind, shortOrDash(item.ResolvesTo)); err != nil {
			return err
		}
	}
	return nil
}

func profileViewFromPolicy(name, kind string, policy agent.DecisionPolicy, metadata agent.DecisionProfileMetadata) (decisionProfileView, error) {
	digest, err := agent.DecisionPolicyDigest(policy)
	if err != nil {
		return decisionProfileView{}, err
	}
	plan, err := internalteam.CompileDecisionExecutionPlan(policy)
	if err != nil {
		return decisionProfileView{}, err
	}
	return decisionProfileView{SchemaVersion: 1, Kind: kind, Enabled: true, Identity: decisionProfileIdentityView{RequestedName: name, ResolvedRef: metadata.Ref, Origin: metadata.Origin, Version: metadata.Version, PolicyDigest: digest}, Plan: decisionPlanViewFromPlan(plan, policy)}, nil
}

func resolveDecisionProfileView(name, teamName string) (decisionProfileView, error) {
	name = strings.TrimSpace(name)
	if name == agent.DecisionProfileOff {
		return decisionProfileView{SchemaVersion: 1, Kind: "decision_profile", Enabled: false, Identity: decisionProfileIdentityView{RequestedName: name}}, nil
	}
	if strings.TrimSpace(teamName) != "" {
		teamDir, err := resolveDecisionTeamDir(teamName)
		if err != nil {
			return decisionProfileView{}, err
		}
		spec, err := internalteam.CompileTeam(teamDir, nil, nil, internalteam.DefaultProviderRegistry)
		if err != nil {
			return decisionProfileView{}, err
		}
		for _, profile := range spec.Decision.Profiles {
			if profile.Name == name && profile.Local {
				return profileViewFromPolicy(name, "decision_profile", profile.Policy, agent.DecisionProfileMetadata{Ref: profile.Ref, Origin: profile.Origin, Version: profile.Version})
			}
		}
	}
	ref := name
	if alias := decisionAliasRef(name); alias != "" {
		ref = alias
	}
	if slices.Contains(agent.BuiltInDecisionProfileBundleRefs(), ref) {
		bundle, err := agent.ResolveBuiltInDecisionProfileBundle(ref)
		if err != nil {
			return decisionProfileView{}, err
		}
		view, err := profileViewFromPolicy(name, "decision_profile", bundle.Policy, agent.DecisionProfileMetadata{Ref: bundle.Ref, Origin: agent.DecisionProfileOriginBuiltin, Version: "v2"})
		if err != nil {
			return decisionProfileView{}, err
		}
		view.Identity.BundleDigest = bundle.BundleDigest
		view.RoleResolution = &bundle.RoleResolution
		view.Evidence = &bundle.Evidence
		view.Limits = &bundle.Limits
		return view, nil
	}
	policy, metadata, err := agent.BuiltInDecisionProfileCatalog().Resolve(agent.DecisionProfileRef{Name: ref})
	if err != nil {
		return decisionProfileView{}, err
	}
	return profileViewFromPolicy(name, "decision_profile", policy, metadata)
}

func runDecisionProfileShow(cmd *cobra.Command, args []string) error {
	view, err := resolveDecisionProfileView(args[0], decisionProfileTeam)
	if err != nil {
		return err
	}
	if decisionJSON {
		return writeDecisionJSON(cmd, view)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "profile: %s\nenabled: %t\n", view.Identity.RequestedName, view.Enabled); err != nil {
		return err
	}
	if view.Identity.ResolvedRef != "" {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "resolved: %s\norigin: %s\nversion: %s\n", view.Identity.ResolvedRef, view.Identity.Origin, view.Identity.Version); err != nil {
			return err
		}
		if view.Identity.BundleDigest != "" {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "bundle-digest: %s\n", view.Identity.BundleDigest); err != nil {
				return err
			}
		}
	}
	if view.Plan != nil {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "plan: judges=%d aggregation=%s challenge=%d revision=%t finalization=%s\n", view.Plan.JudgeCount, view.Plan.Aggregation, view.Plan.ChallengeCount, view.Plan.Revision, view.Plan.Finalization)
		return err
	}
	return nil
}

func runDecisionPlan(cmd *cobra.Command, _ []string) error {
	view, err := resolveDecisionProfileView(decisionProfileName, decisionProfileTeam)
	if err != nil {
		return err
	}
	view.Kind = "decision_plan"
	if decisionJSON {
		return writeDecisionJSON(cmd, view)
	}
	if view.Plan == nil {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "profile is disabled; no decision stages")
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "profile: %s\nproposal: %t\nreference: %t\njudges: %d\naggregation: %s\nchallenge: %d\nrevision: %t\npremortem: %t\nforecast: %t\nfinalization: %s\n", view.Identity.RequestedName, view.Plan.Proposal, view.Plan.Reference, view.Plan.JudgeCount, view.Plan.Aggregation, view.Plan.ChallengeCount, view.Plan.Revision, view.Plan.Premortem, view.Plan.Forecast, view.Plan.Finalization)
	return err
}
