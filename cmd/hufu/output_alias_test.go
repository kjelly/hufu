package main

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestResolveOutputAliasAcceptsEquivalentAndRejectsConflict(t *testing.T) {
	allowed := []string{"text", "json"}
	got, err := resolveOutputAlias("format", "json", true, "output", "json", true, "text", allowed)
	if err != nil || got != "json" {
		t.Fatalf("equivalent aliases = %q, %v", got, err)
	}
	if _, err := resolveOutputAlias("format", "text", true, "output", "json", true, "text", allowed); err == nil {
		t.Fatal("conflicting aliases were accepted")
	}
	if _, err := resolveOutputAlias("output", "mermaid", true, "json", "json", false, "text", []string{"text", "json", "mermaid"}); err != nil {
		t.Fatalf("domain-specific format was rejected: %v", err)
	}
}

func TestJSONBoolAliasTruthTable(t *testing.T) {
	tests := []struct {
		name, output string
		outputSet    bool
		json         bool
		jsonSet      bool
		want         string
		wantErr      bool
	}{
		{name: "unspecified", want: "text"},
		{name: "true", json: true, jsonSet: true, want: "json"},
		{name: "false", jsonSet: true, want: "text"},
		{name: "text false", output: "text", outputSet: true, jsonSet: true, want: "text"},
		{name: "json true", output: "json", outputSet: true, json: true, jsonSet: true, want: "json"},
		{name: "text true conflict", output: "text", outputSet: true, json: true, jsonSet: true, wantErr: true},
		{name: "json false conflict", output: "json", outputSet: true, jsonSet: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveOutputAlias("output", test.output, test.outputSet, "json", outputForJSONAlias(test.json), test.jsonSet, "text", []string{"text", "json"})
			if (err != nil) != test.wantErr || !test.wantErr && got != test.want {
				t.Fatalf("resolve = %q, %v; want %q, error=%t", got, err, test.want, test.wantErr)
			}
		})
	}
}

func TestSessionAndTeamCheckJSONFalseResolveToText(t *testing.T) {
	previousSessionJSON := sessionJSON
	t.Cleanup(func() { sessionJSON = previousSessionJSON })

	sessionCommand := &cobra.Command{Use: "status"}
	output := ""
	sessionCommand.Flags().StringVar(&output, "output", "", "")
	sessionCommand.Flags().BoolVar(&sessionJSON, "json", false, "")
	if err := sessionCommand.Flags().Set("json", "false"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSessionOutput(sessionCommand, output); err != nil || got != "text" {
		t.Fatalf("session --json=false = %q, %v", got, err)
	}

	teamOptions := new(teamCheckOptions)
	teamCommand := &cobra.Command{Use: "check"}
	teamCommand.Flags().StringVar(&teamOptions.output, "output", "text", "")
	teamCommand.Flags().BoolVar(&teamOptions.json, "json", false, "")
	if err := teamCommand.Flags().Set("json", "false"); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveTeamCheckOutput(teamCommand, teamOptions); err != nil || got != "text" {
		t.Fatalf("team check --json=false = %q, %v", got, err)
	}
}
