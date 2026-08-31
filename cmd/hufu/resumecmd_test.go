package main

import (
	"strings"
	"testing"
)

func TestResolveCommandWorkspaceAndTeam(t *testing.T) {
	tests := []struct {
		name              string
		workspace         string
		requestedTeam     string
		workspaceExplicit bool
		wantWorkspace     string
		wantTeam          string
		wantErr           string
	}{
		{
			name:              "explicit per-team workspace",
			workspace:         "./workspace/reviewer",
			workspaceExplicit: true,
			wantWorkspace:     "workspace/reviewer",
			wantTeam:          "reviewer",
		},
		{
			name:          "team flag uses default workspace root",
			workspace:     "./workspace",
			requestedTeam: "Reviewer",
			wantWorkspace: "workspace/reviewer",
			wantTeam:      "reviewer",
		},
		{
			name:      "root workspace needs team flag",
			workspace: "./workspace",
			wantErr:   "pass --agent-team",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace, teamName, err := resolveCommandWorkspaceAndTeam(tt.workspace, tt.requestedTeam, tt.workspaceExplicit)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveCommandWorkspaceAndTeam() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveCommandWorkspaceAndTeam() error = %v", err)
			}
			if workspace != tt.wantWorkspace || teamName != tt.wantTeam {
				t.Fatalf("resolveCommandWorkspaceAndTeam() = (%q, %q), want (%q, %q)", workspace, teamName, tt.wantWorkspace, tt.wantTeam)
			}
		})
	}
}
