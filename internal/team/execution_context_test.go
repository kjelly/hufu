package team

import (
	"encoding/json"
	"testing"
)

func TestExecutionContext_Validate(t *testing.T) {
	tests := []struct {
		name    string
		ctx     ExecutionContext
		wantErr bool
	}{
		{
			name: "valid context",
			ctx: ExecutionContext{
				Team:           "test-team",
				RepositoryRoot: "/tmp/repository",
				CurrentPhase:   PhasePrepare,
				Workflow: Workflow{
					Phases: []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify},
				},
				RuntimeWorkspace: RuntimeWorkspace{Root: "/tmp/workspace"},
				Policies:         Policies{MaxRetries: 3},
			},
			wantErr: false,
		},
		{
			name: "missing team",
			ctx: ExecutionContext{
				Workflow: Workflow{
					Phases: []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify},
				},
				RuntimeWorkspace: RuntimeWorkspace{Root: "/tmp/workspace"},
			},
			wantErr: true,
		},
		{
			name: "empty workflow",
			ctx: ExecutionContext{
				Team:             "test-team",
				RuntimeWorkspace: RuntimeWorkspace{Root: "/tmp/workspace"},
			},
			wantErr: true,
		},
		{
			name: "invalid policy",
			ctx: ExecutionContext{
				Team: "test-team",
				Workflow: Workflow{
					Phases: []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify},
				},
				RuntimeWorkspace: RuntimeWorkspace{Root: "/tmp/workspace"},
				Policies:         Policies{MaxRetries: -1},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.ctx.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestExecutionContext_RuntimeSchemaRoundTrip(t *testing.T) {
	want := ExecutionContext{
		RunID: "run-1", Team: "team", CurrentPhase: PhaseExecute,
		RepositoryRoot:   "/repo",
		Workflow:         Workflow{Phases: []Phase{PhasePrepare, PhaseAudit, PhaseExecute, PhaseVerify}},
		RuntimeWorkspace: RuntimeWorkspace{Root: "/repo-runtime"},
		ArtifactPaths: map[string]string{
			"artifacts": "/repo-runtime/artifacts", "receipts": "/repo-runtime/receipts",
		},
		Policies: Policies{RequirePhaseSuccess: true},
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	var got ExecutionContext
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("round-tripped context rejected: %v", err)
	}
	if got.CurrentPhase != PhaseExecute || got.RepositoryRoot != "/repo" || got.ArtifactPaths["receipts"] != "/repo-runtime/receipts" {
		t.Fatalf("runtime schema fields lost after round trip: %#v", got)
	}
}
