package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/spf13/cobra"
)

type distillationCheckpointGenerator struct {
	maintenanceTextGenerator
	store   *runtimestore.Store
	job     string
	command *cobra.Command
	audits  []string
}

type distillationStageRecord struct {
	Job        string                 `json:"job"`
	Request    string                 `json:"request"`
	SHA256     string                 `json:"sha256"`
	Checkpoint taskdistill.Checkpoint `json:"checkpoint"`
	Audits     []string               `json:"audit_workspaces"`
}

func newDistillationCheckpointGenerator(cmd *cobra.Command, workspace string, plan workspaceDistillationPlan, generator maintenanceTextGenerator, progress maintenanceTextGenerator) (maintenanceTextGenerator, func(), error) {
	identity, ok := generator.(interface{ CheckpointIdentity() string })
	if !ok || identity.CheckpointIdentity() == "" {
		return progress, func() {}, nil
	}
	job := taskdistill.Digest(struct {
		Version                                                  string
		Project, Workspace, Scope, Team, Model, Policy, Identity string
		Sources                                                  []taskdistill.Source
		Tasks                                                    string
		Trash                                                    []distillationTrashSource
	}{"complete-adaptive-v1", plan.ProjectID, plan.WorkspaceID, plan.ContextScopeID, plan.Team, plan.Model, plan.Policy, identity.CheckpointIdentity(), plan.Sources, taskdistill.Digest(plan.Tasks), plan.Trash})
	store, err := runtimestore.Open(cmd.Context(), workspace)
	if err != nil {
		return nil, nil, err
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Distillation checkpoint scope: %s; identical source, policy and model settings reuse verified batches. Audit workspace: %s\n", job, plan.DraftWorkspace)
	audits := []string{}
	if plan.DraftWorkspace != "" {
		audits = append(audits, plan.DraftWorkspace)
	}
	return &distillationCheckpointGenerator{maintenanceTextGenerator: progress, store: store, job: job, command: cmd, audits: audits}, func() { _ = store.Close() }, nil
}

func (g *distillationCheckpointGenerator) LoadDraftCheckpoint(ctx context.Context, request string) (taskdistill.Checkpoint, bool, error) {
	raw, exists, err := g.store.GetDocument(ctx, "task_distillation_checkpoint:"+g.job+":"+request)
	if err != nil || !exists {
		return taskdistill.Checkpoint{}, false, err
	}
	if len(raw) > taskdistill.MaxDraftBytes*2 {
		return taskdistill.Checkpoint{}, false, errors.New("distillation checkpoint exceeds storage budget")
	}
	var record distillationStageRecord
	if err = json.Unmarshal(raw, &record, json.RejectUnknownMembers(true)); err != nil {
		return taskdistill.Checkpoint{}, false, err
	}
	payload, err := checkpointEvidence(record.Checkpoint, record.Audits)
	if err != nil {
		return taskdistill.Checkpoint{}, false, err
	}
	hash := sha256.Sum256(payload)
	if record.Job != g.job || record.Request != request || record.SHA256 != hex.EncodeToString(hash[:]) {
		return taskdistill.Checkpoint{}, false, errors.New("distillation checkpoint identity or content mismatch")
	}
	g.audits = append(g.audits, record.Audits...)
	slices.Sort(g.audits)
	g.audits = slices.Compact(g.audits)
	_, _ = fmt.Fprintf(g.command.ErrOrStderr(), "Distillation reuse verified checkpoint %.12s\n", request)
	return record.Checkpoint, true, nil
}

func (g *distillationCheckpointGenerator) SaveDraftCheckpoint(ctx context.Context, request string, checkpoint taskdistill.Checkpoint) error {
	payload, err := checkpointEvidence(checkpoint, g.audits)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	raw, err := json.Marshal(distillationStageRecord{Job: g.job, Request: request, SHA256: hex.EncodeToString(hash[:]), Checkpoint: checkpoint, Audits: g.audits})
	if err != nil {
		return err
	}
	if len(raw) > taskdistill.MaxDraftBytes*2 {
		return errors.New("distillation checkpoint exceeds storage budget")
	}
	return g.store.PutDocument(ctx, "task_distillation_checkpoint:"+g.job+":"+request, raw)
}

func checkpointEvidence(checkpoint taskdistill.Checkpoint, audits []string) ([]byte, error) {
	return json.Marshal(struct {
		Checkpoint taskdistill.Checkpoint
		Audits     []string
	}{checkpoint, audits})
}
