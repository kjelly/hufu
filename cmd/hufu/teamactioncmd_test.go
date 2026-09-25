package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const teamActionTestManifest = `name: catalog-team
action-providers:
  diagnostics:
    command: [/opt/team-actions/diagnostics.sh]
    dir: /opt/team-actions
    timeout: 120
action-catalog:
  collect-debug-bundle:
    description: Collect bounded runtime diagnostics for one service.
    capability: diagnostics
    type: collect_debug_bundle
    agent: runtime-engineer
    side-effect: none
    input-schema:
      type: object
      properties:
        service:
          type: string
      required-properties: [service]
      additional-properties: false
    access:
      discover: [runtime-engineer]
      propose: [runtime-engineer]
`

func writeTeamActionTestTeam(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"team.yaml":           teamActionTestManifest,
		"coordinator.md":      "---\nname: coordinator\nrole: coordinator\ndescription: Coordinates.\n---\nCoordinate.",
		"runtime-engineer.md": "---\nname: runtime-engineer\ndescription: Runs diagnostics.\ntools: view\n---\nDiagnose.",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestTeamActionCommandsShowCatalogWithoutProviderDetails(t *testing.T) {
	dir := writeTeamActionTestTeam(t)
	t.Cleanup(func() { teamActionOpts = teamActionOptions{output: "text"} })
	tests := []struct {
		name   string
		output string
		run    func(*bytes.Buffer) error
		want   []string
	}{
		{name: "list text", output: "text", run: func(out *bytes.Buffer) error { return runTeamActionList(teamActionListCmd, []string{dir}, out) },
			want: []string{"collect-debug-bundle", "diagnostics", "collect_debug_bundle", "runtime-engineer", "none", "retry"}},
		{name: "list json", output: "json", run: func(out *bytes.Buffer) error { return runTeamActionList(teamActionListCmd, []string{dir}, out) },
			want: []string{`"catalog_hash": "sha256:`, `"id": "collect-debug-bundle"`, `"entry_hash": "sha256:`}},
		{name: "show text", output: "text", run: func(out *bytes.Buffer) error {
			return runTeamActionShow(teamActionShowCmd, []string{"collect-debug-bundle", dir}, out)
		}, want: []string{"capability: diagnostics", "type: collect_debug_bundle", "input-schema:", "entry-hash: sha256:"}},
		{name: "show json", output: "json", run: func(out *bytes.Buffer) error {
			return runTeamActionShow(teamActionShowCmd, []string{"collect-debug-bundle", dir}, out)
		}, want: []string{`"input_schema"`, `"discover": [`, `"max_invocations": 1`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			teamActionOpts = teamActionOptions{output: tt.output}
			var out bytes.Buffer
			if err := tt.run(&out); err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("output lacks %q:\n%s", want, out.String())
				}
			}
			for _, leak := range []string{"/opt/team-actions", "diagnostics.sh", "provider_identity", "command"} {
				if strings.Contains(out.String(), leak) {
					t.Fatalf("output leaks provider detail %q:\n%s", leak, out.String())
				}
			}
		})
	}
	teamActionOpts = teamActionOptions{output: "text"}
	if err := runTeamActionShow(teamActionShowCmd, []string{"missing-action", dir}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not defined") {
		t.Fatalf("show missing action error = %v", err)
	}
}
