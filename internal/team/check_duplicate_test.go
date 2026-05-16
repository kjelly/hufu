package team

import (
	"context"
	"sync"
	"testing"
)

func TestCheckDuplicateTasks_SameBatch(t *testing.T) {
	c := &Coordinator{
		delegatedTasks:   make(map[string]int),
		delegatedTasksMu: sync.Mutex{},
	}

	tests := []struct {
		name           string
		previousTasks  map[string]int
		batchTasks     []TaskDef
		expectWarnings bool
		expectedDupCount int
		warningContains string
	}{
		{
			name:          "two identical tasks in batch - first should not be duplicate",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "in batch",
		},
		{
			name:          "three identical tasks in batch - first two should not be duplicate",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 2,
			warningContains:  "in batch",
		},
		{
			name:          "identical task was already delegated in previous round",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "EXACT DUPLICATE:",
		},
		{
			name:          "different tasks for same agent - no duplicates",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task2"},
			},
			expectWarnings:   false,
			expectedDupCount: 0,
		},
		{
			name:          "same task different agents - no duplicates",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent2", Goal: "task1"},
			},
			expectWarnings:   false,
			expectedDupCount: 0,
		},
		{
			name:          "batch duplicate with constraints - should detect",
			previousTasks: map[string]int{},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1", Constraints: "use python"},
				{Agent: "agent1", Goal: "task1", Constraints: "use python"},
			},
			expectWarnings:   true,
			expectedDupCount: 1,
			warningContains:  "in batch",
		},
		{
			name:          "mixed: one previous round dup, one in-batch dup",
			previousTasks: map[string]int{"agent1:task1": 1},
			batchTasks: []TaskDef{
				{Agent: "agent1", Goal: "task1"},
				{Agent: "agent1", Goal: "task1"},
			},
			expectWarnings:   true,
			expectedDupCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.delegatedTasks = make(map[string]int)
			for k, v := range tt.previousTasks {
				c.delegatedTasks[k] = v
			}

			warnings, duplicates := c.checkDuplicateTasks(context.Background(), tt.batchTasks)

			if tt.expectWarnings && len(warnings) == 0 {
				t.Errorf("expected warnings but got none")
			}
			if !tt.expectWarnings && len(warnings) > 0 {
				t.Errorf("expected no warnings but got %d: %v", len(warnings), warnings)
			}

			actualDupCount := 0
			for _, isDup := range duplicates {
				if isDup {
					actualDupCount++
				}
			}
			if actualDupCount != tt.expectedDupCount {
				t.Errorf("duplicate count = %d, want %d", actualDupCount, tt.expectedDupCount)
			}

			if tt.warningContains != "" && len(warnings) > 0 {
				found := false
				for _, w := range warnings {
					if containsString(w, tt.warningContains) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("warning should contain %q, got %v", tt.warningContains, warnings)
				}
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstring(s, substr))
}

func containsSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}