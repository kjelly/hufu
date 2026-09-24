package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/execution"
	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

const workerHubSecret = "api_key=SECRET-HUB-SURFACES"

// writeWorkerHubWorkspace writes a real, hash-chained event store with one
// completed worker attempt, plus the session checkpoint status reads.
func writeWorkerHubWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-hub", "session-hub")
	if err != nil {
		t.Fatal(err)
	}
	target := execution.ExecutionTarget{Backend: "ollama", Model: "qwen3"}
	task := map[string]any{
		"id": "1", "agent": "coder", "desc": "write the feature", "goal": workerHubSecret, "attempt": 1,
		"execution_target": target, "execution_topology": []execution.ExecutionTarget{target},
	}
	with := func(extra map[string]any) []byte {
		payload := map[string]any{}
		for key, value := range task {
			payload[key] = value
		}
		for key, value := range extra {
			payload[key] = value
		}
		encoded, _ := json.Marshal(payload)
		return encoded
	}
	receipt := map[string]any{
		"run_id": "run-hub", "task_id": "1", "attempt": 1, "occurrence_attempt": 1, "execution_target": target,
		"usage": map[string]any{"total_tokens": 42}, "started_at": "2026-09-24T10:00:01Z", "finished_at": "2026-09-24T10:00:05Z",
		"repair_provenance": map[string]any{"prompt": workerHubSecret},
	}
	for _, event := range []team.RunEvent{
		{Type: string(team.EventTaskCreated), TaskID: "1", Actor: "coder", Payload: with(map[string]any{"status": "pending"})},
		{Type: string(team.EventTaskStarted), TaskID: "1", Actor: "coder", Payload: with(map[string]any{"status": "in_progress", "dispatch_attempt": 1})},
		{Type: string(team.EventTaskCompleted), TaskID: "1", Actor: "coder", Payload: with(map[string]any{
			"status": "done", "output": workerHubSecret, "execution_receipt": receipt, "execution_receipts": []any{receipt},
		})},
	} {
		if _, err := store.AppendPersisted(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := team.SaveSession(workspace, &team.SessionData{CreatedAt: time.Now().UTC().Format(time.RFC3339), Tasks: []*team.TodoItem{{ID: "1", Status: team.TaskDone}}}); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestWorkerHubSurfacesRenderOneProjection(t *testing.T) {
	workspace := writeWorkerHubWorkspace(t)
	hub, err := inspectpkg.LoadWorkerAttempts(context.Background(), workspace, nil, time.Now())
	if err != nil {
		t.Fatalf("LoadWorkerAttempts: %v", err)
	}
	if len(hub.Attempts) != 1 {
		t.Fatalf("attempts = %+v", hub.Attempts)
	}
	attempt := hub.Attempts[0]
	if attempt.TaskID != "1" || attempt.ExecutionTarget != "ollama/qwen3" || attempt.Usage == nil || attempt.Usage.TotalTokens != 42 || attempt.Activity != inspectpkg.WorkerActivityTerminal {
		t.Fatalf("attempt = %+v", attempt)
	}

	// CLI JSON carries exactly the projection.
	previous := [4]any{statusWorkspace, statusJSON, statusWorkers, statusVerbose}
	t.Cleanup(func() {
		statusWorkspace, statusJSON, statusWorkers, statusVerbose = previous[0].(string), previous[1].(bool), previous[2].(bool), previous[3].(bool)
	})
	statusWorkspace, statusJSON, statusWorkers, statusVerbose = workspace, true, true, false
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	var decoded workspaceStatus
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, output.String())
	}
	if decoded.Workers == nil || !reflect.DeepEqual(decoded.Workers.Attempts, hub.Attempts) {
		t.Fatalf("status JSON workers = %+v, want %+v", decoded.Workers, hub.Attempts)
	}
	jsonOutput := output.String()

	// CLI text.
	statusJSON, statusVerbose = false, true
	output.Reset()
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, want := range []string{"TASK  AGENT", "ollama/qwen3", "terminal", "42 tokens", "identity: key " + attempt.AttemptKey} {
		if !strings.Contains(text, want) {
			t.Fatalf("status text missing %q:\n%s", want, text)
		}
	}

	// TUI conversion.
	details := tuiAttemptDetails(hub.Attempts)
	if len(details) != 1 || details[0].Target != attempt.ExecutionTarget || !details[0].TokensKnown || details[0].Tokens != 42 || details[0].Activity != attempt.Activity {
		t.Fatalf("TUI details = %+v", details)
	}

	// Report.
	var report strings.Builder
	writeWorkerAttemptReport(&report, &hub)
	if !strings.Contains(report.String(), "| 1 | coder | 1 | done | terminal | ollama/qwen3 | shared | ") || !strings.Contains(report.String(), " | 42 | 0 | — |") {
		t.Fatalf("report Workers section:\n%s", report.String())
	}

	for name, surface := range map[string]string{"json": jsonOutput, "text": text, "report": report.String()} {
		if strings.Contains(surface, "SECRET-HUB") {
			t.Fatalf("%s surface leaked excluded content:\n%s", name, surface)
		}
	}
}

func TestStatusWithoutWorkersFlagOmitsWorkers(t *testing.T) {
	workspace := writeWorkerHubWorkspace(t)
	previous := [3]any{statusWorkspace, statusJSON, statusWorkers}
	t.Cleanup(func() {
		statusWorkspace, statusJSON, statusWorkers = previous[0].(string), previous[1].(bool), previous[2].(bool)
	})
	statusWorkspace, statusJSON, statusWorkers = workspace, true, false
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	if err := runStatus(command, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), `"workers"`) {
		t.Fatalf("status JSON without --workers carries workers: %s", output.String())
	}
}

// TestWorkerHubRefreshRacesWithConcurrentWorkers runs parallel workers
// appending attempt events while the hub is reloaded and pushed into a TUI
// model, as the TUI snapshot reporter does. Run with -race.
func TestWorkerHubRefreshRacesWithConcurrentWorkers(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-race", "session-race")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	target := execution.ExecutionTarget{Backend: "ollama", Model: "qwen3"}
	payload := func(taskID string, extra map[string]any) []byte {
		base := map[string]any{"id": taskID, "agent": "coder", "desc": "work", "attempt": 1, "execution_target": target, "execution_topology": []execution.ExecutionTarget{target}}
		for key, value := range extra {
			base[key] = value
		}
		encoded, _ := json.Marshal(base)
		return encoded
	}
	const workers, attempts = 4, 5
	for worker := 1; worker <= workers; worker++ {
		taskID := fmt.Sprint(worker)
		if _, err := store.AppendPersisted(team.RunEvent{Type: string(team.EventTaskCreated), TaskID: taskID, Actor: "coder", Payload: payload(taskID, map[string]any{"status": "pending"})}); err != nil {
			t.Fatal(err)
		}
	}
	var group sync.WaitGroup
	for worker := 1; worker <= workers; worker++ {
		taskID := fmt.Sprint(worker)
		group.Go(func() {
			for attempt := 1; attempt <= attempts; attempt++ {
				_, _ = store.AppendPersisted(team.RunEvent{Type: string(team.EventTaskStarted), TaskID: taskID, Actor: "coder", Payload: payload(taskID, map[string]any{"status": "in_progress", "dispatch_attempt": attempt})})
			}
		})
	}
	stop := make(chan struct{})
	refreshed := make(chan int, 1)
	go func() {
		model := tuipkg.NewWithOptions("prompt", tuipkg.TeamInfo{}, tuipkg.Options{Owner: true})
		updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		model = updated.(tuipkg.Model)
		refreshes := 0
		for {
			select {
			case <-stop:
				refreshed <- refreshes
				return
			default:
			}
			hub, err := inspectpkg.LoadWorkerAttempts(context.Background(), workspace, nil, time.Now())
			if err != nil {
				continue // a refresh racing an append may see a partial tail
			}
			updated, _ := model.Update(tuipkg.OperatorDetailsMsg{Attempts: tuiAttemptDetails(hub.Attempts)})
			model = updated.(tuipkg.Model)
			_ = model.View()
			refreshes++
		}
	}()
	group.Wait()
	close(stop)
	<-refreshed
	hub, err := inspectpkg.LoadWorkerAttempts(context.Background(), workspace, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(hub.Attempts) != workers*attempts {
		t.Fatalf("attempts = %d, want %d", len(hub.Attempts), workers*attempts)
	}
}
