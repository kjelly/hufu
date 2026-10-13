package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestLegacyMemoryFlagsAreDeprecated(t *testing.T) {
	tests := []struct {
		name    string
		command *cobra.Command
		flag    string
	}{
		{name: "root --memory", command: newRootCommand(), flag: "memory"},
		{name: "root --memory-model", command: newRootCommand(), flag: "memory-model"},
		{name: "chat --memory", command: replCmd, flag: "memory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flag := tc.command.Flags().Lookup(tc.flag)
			if flag == nil || !strings.Contains(flag.Deprecated, "no effect") {
				t.Fatalf("--%s deprecation = %+v, want a no-effect notice", tc.flag, flag)
			}
		})
	}
}

func TestContextMigrateMemoryWarnsItIsDeprecated(t *testing.T) {
	contextProject, contextLegacyProject = "", ""
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"context", "migrate-memory", "--workspace", t.TempDir()})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--legacy-project are required") {
		t.Fatalf("migrate-memory without flags = %v, want the required-flags error", err)
	}
	if !strings.Contains(out.String(), `Command "migrate-memory" is deprecated`) || !strings.Contains(out.String(), "legacy-memory-store.md") {
		t.Fatalf("migrate-memory output = %q, want the deprecation notice", out.String())
	}
}
