package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

type metadataContractRecoveryKey struct{}

// WithMetadataOnlyContractRecovery explicitly selects a lineage-only child for
// an unversioned, managed, read-only team. Project files remain shared.
func WithMetadataOnlyContractRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, metadataContractRecoveryKey{}, true)
}

func metadataContractRecoveryRequested(ctx context.Context) bool {
	requested, _ := ctx.Value(metadataContractRecoveryKey{}).(bool)
	return requested
}

func (c *Coordinator) validateMetadataContractRecovery(ctx context.Context, session *SessionData) error {
	version := c.session.WorkspaceVersion
	if version.Mode != versionstore.ModeOff || version.WorkspaceID == "" || version.ProjectID == "" || version.StateDir == "" || version.ControlRoot != c.session.Workspace {
		return fmt.Errorf("metadata-only contract recovery requires a managed workspace with versioning off")
	}
	if !c.session.Config.Delegation.BindTaskGoalContracts {
		return fmt.Errorf("metadata-only recovery requires static task-goal contracts")
	}
	selected := contractResultRepairID(ctx)
	for _, definition := range c.session.ContractTasks {
		worker := c.session.Agents[normalizedName(definition.Agent)]
		if worker == nil {
			return fmt.Errorf("metadata-only recovery requires configured worker %q", definition.Agent)
		}
		effect, _, _ := resolveTaskRecovery(worker, definition)
		browserContract := selected != "" && effect == SideEffectBrowserInteraction &&
			!structuredStepsContainEffect(definition.Execution.Steps, ExecutionEffectMutate) && !structuredStepsContainEffect(definition.Execution.Steps, ExecutionEffectProduce)
		if !metadataReadOnlyTask(effect, definition.Execution) && !browserContract {
			return fmt.Errorf("metadata-only recovery requires read-only static contract %q", definition.ID)
		}
	}
	for _, item := range session.Tasks {
		if item == nil || IsPrimaryOccurrence(item) {
			continue
		}
		preservedBrowser := selected != "" && collectionRecoveryBrowserTask(item, selected)
		if !metadataReadOnlyTask(item.SideEffect, item.Execution) && !preservedBrowser {
			return fmt.Errorf("metadata-only recovery refuses task %s side effects", item.ID)
		}
	}
	// Never use a metadata fork to bypass a recorded native versioning floor,
	// pending native operation, or historical workspace snapshot.
	if versionstore.PlatformSupported() {
		store, err := versionstore.Open(ctx, versionstore.Options{StateDir: version.StateDir, ReadOnly: true})
		if errors.Is(err, versionstore.ErrStoreNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = store.Close() }()
		_, recorded, err := store.GetSubjectState(ctx, version.SubjectRoot)
		if err != nil {
			return err
		}
		if recorded {
			return fmt.Errorf("metadata-only recovery refuses existing workspace versioning history; use required versioning")
		}
	}
	return nil
}

func (c *Coordinator) forkContractRecovery(ctx context.Context, source *ResumeSource, tree *SessionTree, name string, metadataOnly bool) (*SessionBranch, error) {
	if metadataOnly {
		if err := c.validateMetadataContractRecovery(ctx, source.Session); err != nil {
			return nil, err
		}
		branch, err := ForkSessionBranch(ctx, c.session.Workspace, tree, c.eventStore, name, source.BranchID)
		if err == nil {
			c.eventStore.SetBranchID(branch.ID)
		}
		return branch, err
	}
	version := c.session.WorkspaceVersion
	store, err := version.OpenStore(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = store.Close() }()
	ops := &WorkspaceSessionOps{Version: version, Workspace: c.session.Workspace, Tree: tree, Events: c.eventStore, Store: store}
	if _, err = ops.prepareDerivedFork(ctx, source.BranchID, func(session *SessionData) error {
		return validateContractRecoverySessionSafety(ctx, session)
	}); err != nil {
		return nil, err
	}
	result, err := ops.Fork(ctx, name, source.BranchID)
	if err != nil {
		return nil, err
	}
	return result.Branch, nil
}

func metadataReadOnlyTask(effect SideEffectClass, execution ExecutionContract) bool {
	return effect == SideEffectNone && !structuredStepsContainEffect(execution.Steps, ExecutionEffectMutate) && !structuredStepsContainEffect(execution.Steps, ExecutionEffectProduce)
}
