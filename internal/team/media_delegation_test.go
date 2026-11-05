package team

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/media"
)

func TestDelegatedImageInputsBindOrderedSourcesBeforeTaskCreation(t *testing.T) {
	c, ctx, _ := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
	selectors := []media.ImageInputSelector{{TurnID: "native-invocation", PartIndex: 2}, {TurnID: "native-invocation", PartIndex: 1}, {TurnID: "native-invocation", PartIndex: 2}}
	original := []TaskDef{{Agent: "worker", Goal: "inspect selected images", ImageInputs: selectors}}
	tasks, err := c.bindDelegatedImageInputs(ctx, original)
	if err != nil || len(tasks) != 1 {
		t.Fatal(err)
	}
	binding := tasks[0].MediaBinding
	if binding == nil || binding.TaskID != "" || binding.InvocationMode != media.InvocationDelegatedTask || binding.RunID != c.executionRunID || tasks[0].MediaInputDigest == "" || tasks[0].ImageInputs != nil {
		t.Fatal("delegation did not create a fresh runtime-owned input binding")
	}
	var indices []int
	for _, selection := range binding.Selections {
		indices = append(indices, selection.PartIndex)
	}
	if !slices.Equal(indices, []int{2, 1, 2}) || len(binding.Refs) != 3 || !reflect.DeepEqual(original[0].ImageInputs, selectors) || original[0].MediaBinding != nil {
		t.Fatal("delegation reordered, deduplicated or mutated the model selection")
	}
	item := createMediaOccurrence(t, c, ctx, tasks[0])
	if item.MediaBinding.TaskID != item.ID || item.MediaInputDigest != binding.InputDigest {
		t.Fatal("canonical task lost its selected input or fresh owner")
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal("independent audit rejected an explicitly delegated input", err)
	}
	// A new M1 owner still cannot adopt an already delegated source. Only
	// an existing owner may retain its original grant after later delegation.
	direct := item.MediaBinding.Clone()
	direct.TaskID, direct.InvocationMode = "another-owner", media.InvocationSingleTask
	sealed, err := direct.Seal()
	if err != nil {
		t.Fatal(err)
	}
	requireMediaCode(t, validateMediaInvocationSourceOwner(&sealed, events), media.AccessDenied)
	other, err := c.bindDelegatedImageInputs(ctx, []TaskDef{{Agent: "worker", Goal: original[0].Goal, ImageInputs: selectors[:1]}})
	if err != nil || other[0].MediaInputDigest == binding.InputDigest {
		t.Fatal("different ordered image selections shared a cache/dedup digest", err)
	}
}

func TestDelegatedImageInputsRejectSourcesOutsideCallerGrant(t *testing.T) {
	for _, scenario := range []string{"no scope", "unknown turn", "text part", "unavailable index", "stale lease", "runtime binding override"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, _ := coordinatorMetadataFixture(t, "https://fixture.invalid/v1", nil)
			task := TaskDef{Agent: "worker", Goal: "inspect", ImageInputs: []media.ImageInputSelector{{TurnID: "native-invocation", PartIndex: 1}}}
			switch scenario {
			case "no scope":
				ctx = context.WithValue(ctx, coordinatorReferenceMetadataKey{}, (*coordinatorReferenceMetadata)(nil))
			case "unknown turn":
				task.ImageInputs[0].TurnID = "unavailable-turn"
			case "text part":
				task.ImageInputs[0].PartIndex = 0
			case "unavailable index":
				task.ImageInputs[0].PartIndex = 99
			case "stale lease":
				c.activeInvocationLease().release()
			case "runtime binding override":
				task.MediaInputDigest = "model-supplied-digest"
			}
			if _, err := c.bindDelegatedImageInputs(ctx, []TaskDef{task}); err == nil {
				t.Fatal("unavailable image scope silently became runnable work")
			}
			if len(c.TaskTracker().TodoList().Items()) != 0 {
				t.Fatal("rejected selection created a task before source authorization")
			}
		})
	}
}

func TestWorkerImageDelegationCannotExpandItsParentInput(t *testing.T) {
	c, ctx, parent, _ := nativeMediaProviderFixture(t)
	_ = admittedMediaTask(t, c, ctx, "another-turn")
	allowed := media.ImageInputSelector{TurnID: parent.MediaBinding.Selections[0].TurnID, PartIndex: parent.MediaBinding.Selections[0].PartIndex}
	tasks, err := c.bindDelegatedImageInputs(ctx, []TaskDef{{Agent: "worker", Goal: "inspect inherited image", ImageInputs: []media.ImageInputSelector{allowed}}})
	if err != nil || len(tasks[0].MediaBinding.Selections) != 1 || tasks[0].MediaBinding.Selections[0] != parent.MediaBinding.Selections[0] {
		t.Fatal("worker lost its explicitly selected parent source", err)
	}
	createMediaOccurrence(t, c, ctx, tasks[0])
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativeMediaSourceScope(parent.MediaBinding, events); err != nil {
		t.Fatal("later explicit delegation revoked the original owner's input", err)
	}
	if _, err := c.bindDelegatedImageInputs(ctx, []TaskDef{{Agent: "worker", Goal: "inspect foreign image", ImageInputs: []media.ImageInputSelector{{TurnID: "another-turn", PartIndex: 1}}}}); err == nil {
		t.Fatal("worker expanded its input using the coordinator's other history")
	}
}
