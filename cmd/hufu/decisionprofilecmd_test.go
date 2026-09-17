package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/spf13/cobra"
)

func decisionProfileOutputCommand() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(os.Stdout)
	return cmd
}

func TestDecisionProfileInspectionUsesStableDTOs(t *testing.T) {
	decisionJSON = true
	decisionProfileTeam = ""
	t.Cleanup(func() { decisionJSON = false; decisionProfileTeam = "" })
	out := captureStdout(t, func() {
		if err := runDecisionProfileList(decisionProfileOutputCommand(), nil); err != nil {
			t.Fatal(err)
		}
	})
	var list decisionProfileListView
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("list JSON: %v", err)
	}
	if list.SchemaVersion != 1 || list.Kind != "decision_profile_list" {
		t.Fatalf("list envelope = %#v", list)
	}
	if len(list.Profiles) != 10 || list.Profiles[0].Name != "builtin/high-stakes@v1" || list.Profiles[len(list.Profiles)-1].Name != "standard" {
		t.Fatalf("unexpected built-in list: %#v", list.Profiles)
	}
	out = captureStdout(t, func() {
		if err := runDecisionProfileShow(decisionProfileOutputCommand(), []string{"standard"}); err != nil {
			t.Fatal(err)
		}
	})
	var show decisionProfileView
	if err := json.Unmarshal([]byte(out), &show); err != nil {
		t.Fatalf("show JSON: %v", err)
	}
	if show.SchemaVersion != 1 || show.Kind != "decision_profile" || !show.Enabled || show.Identity.ResolvedRef != "builtin/standard@v1" || show.Plan == nil {
		t.Fatalf("show projection = %#v", show)
	}
}

func TestDecisionProfileInspectionTeamAddsLocalProfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("decision:\n  profile: custom\n  profiles:\n    custom:\n      policy:\n        independent-judgments: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	decisionJSON = true
	decisionProfileTeam = dir
	t.Cleanup(func() { decisionJSON = false; decisionProfileTeam = "" })
	out := captureStdout(t, func() {
		if err := runDecisionProfileList(decisionProfileOutputCommand(), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"name": "custom"`) || !strings.Contains(out, `"kind": "local"`) {
		t.Fatalf("team list = %s", out)
	}
	out = captureStdout(t, func() {
		if err := runDecisionProfileShow(decisionProfileOutputCommand(), []string{"custom"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"requested_name": "custom"`) || !strings.Contains(out, `"origin": "team-inline"`) {
		t.Fatalf("team show = %s", out)
	}
}

func TestDecisionProfileInspectionDistinguishesSyntheticAliasFromLocalOverride(t *testing.T) {
	tests := map[string]struct {
		manifest string
		kind     string
	}{
		"synthetic alias remains alias": {
			manifest: "decision:\n  profile: standard\n",
			kind:     "alias",
		},
		"local standard replaces alias": {
			manifest: "decision:\n  profile: standard\n  profiles:\n    standard:\n      policy:\n        independent-judgments: 2\n",
			kind:     "local",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(tt.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			decisionJSON = true
			decisionProfileTeam = dir
			t.Cleanup(func() { decisionJSON = false; decisionProfileTeam = "" })
			out := captureStdout(t, func() {
				if err := runDecisionProfileList(decisionProfileOutputCommand(), nil); err != nil {
					t.Fatal(err)
				}
			})
			var list decisionProfileListView
			if err := json.Unmarshal([]byte(out), &list); err != nil {
				t.Fatal(err)
			}
			matches := 0
			for _, item := range list.Profiles {
				if item.Name == "standard" {
					matches++
					if item.Kind != tt.kind {
						t.Fatalf("standard kind = %q, want %q", item.Kind, tt.kind)
					}
				}
			}
			if matches != 1 {
				t.Fatalf("standard rows = %d, want 1: %#v", matches, list.Profiles)
			}
		})
	}
}

func TestDecisionProfileInspectionTeamStillResolvesBuiltins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: inspect\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := resolveDecisionProfileView("standard", dir)
	if err != nil {
		t.Fatal(err)
	}
	if view.Identity.ResolvedRef != agent.DecisionProfileBuiltinStandardV1 {
		t.Fatalf("resolved ref = %q", view.Identity.ResolvedRef)
	}
}

func TestDecisionAuthoringDocumentationCommandsAreRegistered(t *testing.T) {
	root := newRootCommand()
	for _, path := range [][]string{{"decision", "profile", "list"}, {"decision", "profile", "show"}, {"decision", "plan"}, {"team", "migrate"}, {"team", "explain"}, {"team", "validate"}} {
		command, _, err := root.Find(path)
		if err != nil {
			t.Fatalf("documented command %v is not registered: %v", path, err)
		}
		if len(path) >= 2 && path[0] == "decision" && (path[1] == "profile" || path[1] == "plan") && command.Flags().Lookup("team") == nil {
			t.Fatalf("documented command %v is missing --team", path)
		}
	}
}
