package team

import (
	"context"
	"encoding/json/v2"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestMediaTaskResetRejectsChangedAuthority(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	item := createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
	if err := c.CommitTaskTransition(ctx, item.ID, TaskPending, TaskInProgress, "interrupted", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.prepareInterruptedTaskForResume(ctx, item.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(events, func(event RunEvent) bool {
		_, reset, err := decodeMediaTaskReset(event)
		return err == nil && reset
	})
	if index < 0 {
		t.Fatal("missing reset fixture")
	}
	reset, prior := events[index], events[:index]
	if err := verifyMediaTaskReset(reset, prior); err != nil {
		t.Fatal("valid reset fixture", err)
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"both flags", func(p map[string]any) { p["reset_for_retry"] = true }},
		{"no flag", func(p map[string]any) { delete(p, "reset_for_resume") }},
		{"wrong prior status", func(p map[string]any) { p["previous_status"] = string(TaskPaused) }},
		{"terminal prior status", func(p map[string]any) { p["previous_status"] = string(TaskDone) }},
		{"revision jump", func(p map[string]any) { p["occurrence_revision"] = 100 }},
		{"changed attempts", func(p map[string]any) { p["retries"] = 1 }},
		{"retained dispatch", func(p map[string]any) { p["dispatch_id"] = "old-worker" }},
		{"changed goal", func(p map[string]any) { p["goal"] = "different execution scope" }},
		{"changed agent", func(p map[string]any) { p["agent"] = "other" }},
		{"removed binding", func(p map[string]any) { delete(p, "media_binding") }},
		{"removed media fields", func(p map[string]any) {
			delete(p, "media_binding")
			delete(p, "media_input_digest")
		}},
		{"changed digest", func(p map[string]any) { p["media_input_digest"] = "other" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var payload map[string]any
			if err := json.Unmarshal(reset.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			test.change(payload)
			forged := reset
			forged.Payload, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyMediaTaskReset(forged, prior); err == nil {
				t.Fatal("changed reset was accepted")
			}
			if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, slices.Concat(prior, []RunEvent{forged})); err == nil {
				t.Fatal("independent audit accepted a changed reset")
			}
		})
	}
	for _, missing := range []EventType{EventTaskCreated, EventTaskMediaBound} {
		t.Run("missing "+string(missing), func(t *testing.T) {
			incomplete := slices.DeleteFunc(slices.Clone(prior), func(event RunEvent) bool {
				return event.TaskID == item.ID && event.Type == string(missing)
			})
			if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, append(incomplete, reset)); err == nil {
				t.Fatal("reset replaced a missing original atomic binding")
			}
		})
	}
	current := c.todoItemByID(item.ID)
	if current.MediaBinding.Hash != item.MediaBinding.Hash || current.Status != TaskPending {
		t.Fatal("offline rejection changed the canonical occurrence")
	}
}

func TestMediaInputAdmissionUsesInheritedPolicy(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	originalRun := c.executionRunID
	c.finalizePublicInvocationFailure(context.Canceled)
	end()
	ctx = openMediaInvocation(t, c)
	if c.executionRunID == originalRun {
		t.Fatal("fixture did not open a new invocation")
	}
	createMediaOccurrence(t, c, ctx, nativeInvocationTask(t, c, ctx, media.InvocationSingleTask))
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	policies := 0
	for _, event := range events {
		if event.Type == string(EventExecutionPolicySnapshot) {
			policies++
			if event.RunID != originalRun {
				t.Fatal("fixture recreated the execution policy in the input invocation")
			}
		}
	}
	if policies != 1 {
		t.Fatal("fixture must inherit exactly one original policy")
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal("inherited policy failed independent audit", err)
	}
}
