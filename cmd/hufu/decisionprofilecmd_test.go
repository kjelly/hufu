package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionProfileInspectionUsesStableDTOs(t *testing.T) {
	decisionJSON = true
	decisionProfileTeam = ""
	t.Cleanup(func() { decisionJSON = false; decisionProfileTeam = "" })
	out := captureStdout(t, func() {
		if err := runDecisionProfileList(decisionCmd, nil); err != nil {
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
	if len(list.Profiles) != 7 || list.Profiles[0].Name != "builtin/high-stakes@v1" || list.Profiles[len(list.Profiles)-1].Name != "standard" {
		t.Fatalf("unexpected built-in list: %#v", list.Profiles)
	}
	out = captureStdout(t, func() {
		if err := runDecisionProfileShow(decisionCmd, []string{"standard"}); err != nil {
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
		if err := runDecisionProfileList(decisionCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"name": "custom"`) || !strings.Contains(out, `"kind": "local"`) {
		t.Fatalf("team list = %s", out)
	}
	out = captureStdout(t, func() {
		if err := runDecisionProfileShow(decisionCmd, []string{"custom"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"requested_name": "custom"`) || !strings.Contains(out, `"origin": "team-inline"`) {
		t.Fatalf("team show = %s", out)
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
