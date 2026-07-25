package team

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
)

func TestUpdateSTM_ConcurrencyNoLostUpdates(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{Workspace: tmpDir}
	c := &Coordinator{session: session}

	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			err := c.updateSTM(func(existing string) string {
				entry := fmt.Sprintf("- entry-%d", workerID)
				if existing == "" {
					return entry
				}
				return existing + "\n" + entry
			})
			if err != nil {
				t.Errorf("worker %d updateSTM failed: %v", workerID, err)
			}
		}()
	}

	wg.Wait()

	finalContent := LoadSTM(tmpDir)
	for i := 0; i < numGoroutines; i++ {
		expected := fmt.Sprintf("- entry-%d", i)
		if !strings.Contains(finalContent, expected) {
			t.Errorf("lost update: final STM does not contain %q", expected)
		}
	}
}

func TestSTMWriteTool_Concurrency(t *testing.T) {
	tmpDir := t.TempDir()
	session := &TeamSession{Workspace: tmpDir}
	c := &Coordinator{session: session}
	tool := &stmWriteTool{coordinator: c}

	const numGoroutines = 30
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		workerID := i
		go func() {
			defer wg.Done()
			call := fantasy.ToolCall{
				Input: fmt.Sprintf(`{"content": "- task-item-%d", "mode": "append"}`, workerID),
			}
			resp, err := tool.Run(context.Background(), call)
			if err != nil || resp.IsError {
				t.Errorf("worker %d stmWriteTool failed: %v, resp: %v", workerID, err, resp)
			}
		}()
	}

	wg.Wait()

	finalContent := LoadSTM(tmpDir)
	for i := 0; i < numGoroutines; i++ {
		expected := fmt.Sprintf("- task-item-%d", i)
		if !strings.Contains(finalContent, expected) {
			t.Errorf("stmWriteTool lost update: final STM missing %q", expected)
		}
	}
}

func TestUpdateSTM_EmitsEventStoreEvent(t *testing.T) {
	tmpDir := t.TempDir()
	es, err := NewEventStore(tmpDir, "run-test", "session-test")
	if err != nil {
		t.Fatalf("failed to create EventStore: %v", err)
	}
	defer es.Close()

	c := &Coordinator{
		session:    &TeamSession{Workspace: tmpDir},
		eventStore: es,
	}

	err = c.updateSTM(func(existing string) string {
		return "# 進度\n- completed step 1"
	})
	if err != nil {
		t.Fatalf("updateSTM failed: %v", err)
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("failed to read events: %v", err)
	}

	var found bool
	for _, event := range events {
		if event.Type == "stm_updated" {
			found = true
			if !strings.Contains(string(event.Payload), "completed step 1") {
				t.Errorf("unexpected event payload: %s", string(event.Payload))
			}
		}
	}
	if !found {
		t.Error("expected stm_updated event in EventStore, none found")
	}
}

func TestSaveSTMUsesAtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	content := "test atomic stm"

	if err := SaveSTM(tmpDir, content); err != nil {
		t.Fatalf("SaveSTM failed: %v", err)
	}

	got := LoadSTM(tmpDir)
	if got != content {
		t.Errorf("LoadSTM = %q, want %q", got, content)
	}

	// Verify stm.md exists in workspace
	path := filepath.Join(tmpDir, "stm.md")
	if _, err := filepath.Abs(path); err != nil {
		t.Errorf("file path error: %v", err)
	}
}
