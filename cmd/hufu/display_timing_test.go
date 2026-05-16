package main

import (
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

func TestFormatTodoItemTime_SkippedWithoutStart(t *testing.T) {
	tests := []struct {
		name     string
		item     *team.TodoItem
		expected string
	}{
		{
			name: "skipped with zero StartedAt and EndedAt",
			item: &team.TodoItem{
				ID:     "1",
				Status: team.TaskSkipped,
			},
			expected: "",
		},
		{
			name: "skipped with StartedAt but no EndedAt (task was running before skip)",
			item: &team.TodoItem{
				ID:        "2",
				Status:    team.TaskSkipped,
				StartedAt: time.Now().Add(-5 * time.Minute),
			},
			expected: "(5m0s)",
		},
		{
			name: "done with StartedAt and EndedAt",
			item: &team.TodoItem{
				ID:        "3",
				Status:    team.TaskDone,
				StartedAt: time.Now().Add(-10 * time.Minute),
				EndedAt:   time.Now(),
			},
			expected: "(10m0s)",
		},
		{
			name: "in_progress with StartedAt but no EndedAt",
			item: &team.TodoItem{
				ID:        "4",
				Status:    team.TaskInProgress,
				StartedAt: time.Now().Add(-3 * time.Minute),
			},
			expected: "(3m0s)",
		},
		{
			name: "pending with no times",
			item: &team.TodoItem{
				ID:     "5",
				Status: team.TaskPending,
			},
			expected: "",
		},
		{
			name: "error with no times",
			item: &team.TodoItem{
				ID:     "6",
				Status: team.TaskError,
			},
			expected: "",
		},
		{
			name: "done with model and tool time",
			item: &team.TodoItem{
				ID:        "7",
				Status:    team.TaskDone,
				StartedAt: time.Now().Add(-10 * time.Minute),
				EndedAt:   time.Now(),
				ModelTime: 5 * time.Minute,
				ToolTime:  3 * time.Minute,
			},
			expected: "(10m0s: 5m0s model + 3m0s tools)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := formatTodoItemTime(tt.item)
			if result != tt.expected {
				t.Errorf("formatTodoItemTime() = %q, want %q", result, tt.expected)
			}
		})
	}
}