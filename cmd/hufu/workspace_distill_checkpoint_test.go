package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/spf13/cobra"
)

type identityTextGenerator struct {
	generate func(context.Context, string) (string, error)
	identity string
}

func (g identityTextGenerator) GenerateText(ctx context.Context, prompt string) (string, error) {
	return g.generate(ctx, prompt)
}
func (g identityTextGenerator) CheckpointIdentity() string { return g.identity }

func TestDistillationCheckpointsPersistAndBindScope(t *testing.T) {
	workspace := t.TempDir()
	plan := workspaceDistillationPlan{ProjectID: "project", WorkspaceID: "workspace", ContextScopeID: "scope", Team: "team", Model: "model", Policy: "operator policy", Sources: []taskdistill.Source{{ID: "old", Content: strings.Repeat("e", taskdistill.MaxInputBytes*2)}}}
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetErr(&bytes.Buffer{})
	calls := 0
	attempts := 0
	var auditWorkspaces []string
	fail := true
	generate := func(_ context.Context, prompt string) (string, error) {
		calls++
		if fail && calls == 2 {
			return "", errors.New("provider unavailable")
		}
		_, raw, _ := strings.Cut(prompt, "\nDATA: ")
		var request struct {
			Sources []taskdistill.Source `json:"sources"`
		}
		if err := json.Unmarshal([]byte(raw), &request); err != nil {
			t.Fatal(err)
		}
		draft := taskdistill.Draft{Decisions: []taskdistill.Decision{}, Knowledge: []taskdistill.Knowledge{}}
		for _, source := range request.Sources {
			draft.Decisions = append(draft.Decisions, taskdistill.Decision{SourceID: source.ID, Action: "discard", Reason: "transient"})
		}
		data, err := json.Marshal(draft)
		return string(data), err
	}
	run := func(plan workspaceDistillationPlan, identity string) error {
		attempts++
		plan.DraftWorkspace = filepath.Join(workspace, "maintenance", "task-distillation", fmt.Sprintf("draft-%d", attempts))
		g := identityTextGenerator{generate: generate, identity: identity}
		cached, closeCache, err := newDistillationCheckpointGenerator(command, workspace, plan, g, g)
		if err != nil {
			return err
		}
		defer closeCache()
		_, err = taskdistill.GenerateBatched(t.Context(), cached, plan.Sources, plan.Policy, 32)
		auditWorkspaces = slices.Clone(cached.(*distillationCheckpointGenerator).audits)
		return err
	}
	if err := run(plan, "admitted-policy"); err == nil {
		t.Fatal("failure accepted a partial proposal")
	}
	fail = false
	calls = 0
	if err := run(plan, "admitted-policy"); err != nil || calls != 3 {
		t.Fatalf("completed batch was resent: calls=%d err=%v", calls, err)
	}
	calls = 0
	if err := run(plan, "admitted-policy"); err != nil || calls != 0 {
		t.Fatalf("complete checkpoint ignored: calls=%d err=%v", calls, err)
	}
	for _, attempt := range []int{1, 2} {
		previous := filepath.Join(workspace, "maintenance", "task-distillation", fmt.Sprintf("draft-%d", attempt))
		if !slices.Contains(auditWorkspaces, previous) {
			t.Fatalf("reused batches lost their model call evidence: %v", auditWorkspaces)
		}
	}
	for _, change := range []string{"policy", "model", "source", "destination", "admission"} {
		changed := plan
		changed.Sources = append([]taskdistill.Source{}, plan.Sources...)
		identity := "admitted-policy"
		switch change {
		case "policy":
			changed.Policy += " changed"
		case "model":
			changed.Model = "other"
		case "source":
			changed.Sources[0].Content += "x"
		case "destination":
			changed.ContextScopeID = "other"
		case "admission":
			identity = "other-policy"
		}
		calls = 0
		if err := run(changed, identity); err != nil || calls == 0 {
			t.Fatalf("%s reused an unrelated checkpoint: calls=%d err=%v", change, calls, err)
		}
	}
}
