package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestCollectDoctorContractFindings(t *testing.T) {
	session := &team.TeamSession{
		Dir: t.TempDir(),
		ContractTasks: []team.TaskDef{{
			Verify: "missing-wp18-doctor-command || true",
		}},
	}

	findings := collectDoctorContractFindings(session, session.Dir)
	if !hasDoctorFinding(findings, team.FindingVerifierNotAsserting, "tasks[0].verify") {
		t.Fatalf("doctor findings = %#v, want task verifier lint", findings)
	}
	if !hasDoctorFinding(findings, team.FindingExecutableUnresolved, "tasks[0].execution") {
		t.Fatalf("doctor findings = %#v, want task executable resolution", findings)
	}
}

func TestCollectDoctorContractFindingsUsesRuntimeProjectDir(t *testing.T) {
	projectDir := t.TempDir()
	teamDir := t.TempDir()
	const scriptName = "wp18-runtime-check.sh"

	tests := []struct {
		name           string
		scriptDir      string
		wantUnresolved bool
	}{
		{
			name:           "accepts executable in runtime project directory",
			scriptDir:      projectDir,
			wantUnresolved: false,
		},
		{
			name:           "rejects executable only beside team definition",
			scriptDir:      teamDir,
			wantUnresolved: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, dir := range []string{projectDir, teamDir} {
				if err := os.Remove(filepath.Join(dir, scriptName)); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(tt.scriptDir, scriptName), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
				t.Fatal(err)
			}

			session := &team.TeamSession{
				Dir: teamDir,
				ContractTasks: []team.TaskDef{{
					Verify: "./" + scriptName,
				}},
			}
			findings := collectDoctorContractFindings(session, projectDir)
			gotUnresolved := hasDoctorFinding(findings, team.FindingExecutableUnresolved, "tasks[0].execution")
			if gotUnresolved != tt.wantUnresolved {
				t.Fatalf("unresolved finding = %t, want %t; findings=%#v", gotUnresolved, tt.wantUnresolved, findings)
			}
		})
	}
}

func hasDoctorFinding(findings []doctorContractFinding, code, location string) bool {
	for _, finding := range findings {
		if finding.Finding.Code == code && finding.Location == location {
			return true
		}
	}
	return false
}
