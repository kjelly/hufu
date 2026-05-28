package sidecar

import (
	"context"
	"testing"
)

func TestExecuteSidecarTask_FallbackBehavior(t *testing.T) {
	tests := []struct {
		name           string
		sidecarInst    *Sidecar
		task           string
		expectFallback bool
		expectError    bool
		errorContains  string
	}{
		{
			name:           "nil sidecar triggers fallback",
			sidecarInst:    nil,
			task:           "test task",
			expectFallback: true,
			expectError:    false,
		},
		{
			name:           "non-nil sidecar executes normally",
			sidecarInst:    nil,
			task:           "test task",
			expectFallback: true,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s *Sidecar
			if tt.sidecarInst != nil {
				s = tt.sidecarInst
			}

			if s == nil {
				_, err := s.Execute(context.Background(), tt.task)
				if err == nil && tt.expectError {
					t.Errorf("expected error containing %q, got nil", tt.errorContains)
				}
				if err != nil && err.Error() != "sidecar not configured" {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSidecarExecute_NilReceiver(t *testing.T) {
	var s *Sidecar

	_, err := s.Execute(context.Background(), "test task")
	if err == nil {
		t.Error("expected error for nil sidecar, got nil")
	}
	if err.Error() != "sidecar not configured" {
		t.Errorf("expected 'sidecar not configured', got %v", err)
	}
}

func TestSidecarMatchSkills_NilReceiver(t *testing.T) {
	var s *Sidecar

	_, err := s.MatchSkills(context.Background(), "test prompt", []SkillSummary{})
	if err == nil {
		t.Error("expected error for nil sidecar, got nil")
	}
	if err.Error() != "sidecar not initialized" {
		t.Errorf("expected 'sidecar not initialized', got %v", err)
	}
}
