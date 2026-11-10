package main

import (
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

func TestRestoreResumeLaunchDefaults(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	for _, name := range []string{"saved", "explicit_false", "profile", "legacy"} {
		t.Run(name, func(t *testing.T) {
			opts = runOptions{}
			cmd := &cobra.Command{Use: "resume"}
			cmd.Flags().BoolVar(&opts.noNet, "no-net", false, "")
			cmd.Flags().StringVar(&opts.executionProfile, "execution-profile", "", "")
			launch := &team.LaunchSettings{Options: team.LaunchOptions{
				ExecutionProfile: "strict-verification", NoNet: true, RestrictedBash: true,
				Unattended: true, MaxDuration: 90, ForcedSkills: []string{"fixture"},
			}}
			if name == "legacy" {
				launch = nil
			}
			if name == "explicit_false" {
				if err := cmd.Flags().Set("no-net", "false"); err != nil {
					t.Fatal(err)
				}
			}
			if name == "profile" {
				// applyProfile uses pflag.Set, so selected profile values are
				// explicit overrides too, rather than ambient startup defaults.
				if err := cmd.Flags().Set("execution-profile", "default"); err != nil {
					t.Fatal(err)
				}
			}
			source := &team.ResumeSource{Session: &team.SessionData{ExecutionPolicySnapshot: &team.ExecutionPolicySnapshot{Launch: launch}}}
			restoreResumeLaunchDefaults(cmd, source)
			if name == "legacy" {
				if !reflect.DeepEqual(opts, runOptions{}) {
					t.Fatal("legacy startup values were invented")
				}
				return
			}
			if opts.noNet != (name != "explicit_false") || !opts.rbashMode || !opts.unattended || opts.maxDuration != 90 {
				t.Fatalf("wrong launch defaults: %+v", opts)
			}
			wantProfile := "strict-verification"
			if name == "profile" {
				wantProfile = "default"
			}
			if opts.executionProfile != wantProfile {
				t.Fatal("profile override was ignored")
			}
			opts.forcedSkills[0] = "changed"
			if launch.Options.ForcedSkills[0] != "fixture" {
				t.Fatal("restoration mutated saved settings")
			}
		})
	}
}

func TestResolveExecutionRoleModelsKeepsRestoredDisabledRoles(t *testing.T) {
	session := &team.TeamSession{RestoredLaunchSettings: true, Config: agent.TeamConfig{
		WorkerModel: "ollama/worker", CoordinatorModel: "ollama/coordinator",
	}}
	roles, err := resolveExecutionRoleModels(session, &config.Config{SidecarModel: "ollama/new-default"})
	if err != nil || roles.Sidecar != "" || roles.Guard != "" || roles.Judge != "" {
		t.Fatalf("current defaults enabled a disabled saved role: %+v %v", roles, err)
	}
	applyCLIModelOverrides(&session.Config, ModelCLIOverrides{SidecarModel: "ollama/explicit"})
	roles, err = resolveExecutionRoleModels(session, &config.Config{})
	if err != nil || roles.Sidecar != "ollama/explicit" {
		t.Fatalf("explicit saved-role override ignored: %+v %v", roles, err)
	}
}

func TestRestoredUnattendedDisablesInteractiveFlags(t *testing.T) {
	previous := opts
	t.Cleanup(func() { opts = previous })
	opts = runOptions{displayMode: "terminal", eventFormat: "text", tuiMode: true, stepsMode: true}
	source := &team.ResumeSource{Session: &team.SessionData{ExecutionPolicySnapshot: &team.ExecutionPolicySnapshot{
		Launch: &team.LaunchSettings{Options: team.LaunchOptions{Unattended: true}},
	}}}
	restoreResumeLaunchDefaults(&cobra.Command{}, source)
	if err := validateRunFlags(); err != nil || opts.tuiMode || opts.stepsMode || !opts.unattended {
		t.Fatalf("restored unattended mode retained interactive flags: %v", err)
	}
}
