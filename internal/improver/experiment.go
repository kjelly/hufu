package improver

import (
	"context"
	"errors"
	"time"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

type Experiment struct {
	Plan       paired.ExperimentPlan
	workspace  *ProbeWorkspace
	objects    *ObjectStore
	journal    *team.EventStore
	controller *Controller
	prepared   Prepared
}

func CreateExperiment(ctx context.Context, workspace *ProbeWorkspace, prepared Prepared, settings RuntimeSettings) (result *Experiment, retErr error) {
	if workspace == nil || workspace.readOnly {
		return nil, errors.New("creating an experiment requires a writable workspace")
	}
	experiment, err := openExperimentStores(workspace, prepared.Config.ExperimentID)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, experiment.Close())
		}
	}()
	plan, bindings, err := FreezePlan(prepared, workspace.Metadata.WorkspaceID, workspace.BranchID, experiment.objects, settings)
	if err != nil {
		return nil, err
	}
	runID, err := team.NewLogicalRunID()
	if err != nil {
		return nil, err
	}
	scope := LedgerScope{ExperimentID: plan.ExperimentID, WorkspaceID: workspace.Metadata.WorkspaceID,
		BranchID: workspace.BranchID, RunID: runID, SessionID: workspace.Metadata.SessionID}
	controller, err := NewController(StoreJournal{experiment.journal}, experiment.objects, scope, plan, nil)
	if err != nil {
		return nil, err
	}
	if err := controller.Create(ctx); err != nil {
		return nil, err
	}
	experiment.Plan, experiment.controller = plan, controller
	experiment.prepared, err = LoadPrepared(experiment.objects, plan, bindings)
	if err != nil {
		return nil, err
	}
	// These bindings are also in the frozen closure: resume can finish a
	// partially acknowledged initialization without rereading source files.
	for _, binding := range bindings {
		if err := controller.BindVariant(ctx, binding); err != nil {
			return nil, err
		}
	}
	return experiment, nil
}

func OpenExperiment(ctx context.Context, workspace *ProbeWorkspace, id string) (result *Experiment, retErr error) {
	if workspace == nil {
		return nil, errors.New("experiment workspace is required")
	}
	experiment, err := openExperimentStores(workspace, id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, experiment.Close())
		}
	}()
	journal := StoreJournal{experiment.journal}
	plan, scope, err := LoadCommittedPlan(ctx, journal, experiment.objects, workspace.BranchID, id)
	if err != nil {
		return nil, err
	}
	if scope.WorkspaceID != workspace.Metadata.WorkspaceID || scope.SessionID != workspace.Metadata.SessionID {
		return nil, errors.New("experiment belongs to a different control workspace")
	}
	controller, err := NewController(journal, experiment.objects, scope, plan, nil)
	if err != nil {
		return nil, err
	}
	state, err := controller.Read(ctx)
	if err != nil {
		return nil, err
	}
	var bindings []VariantBinding
	for _, binding := range state.Variants {
		bindings = append(bindings, binding)
	}
	experiment.prepared, err = LoadPrepared(experiment.objects, plan, bindings)
	if err != nil {
		return nil, err
	}
	experiment.Plan, experiment.controller = plan, controller
	return experiment, nil
}

func openExperimentStores(workspace *ProbeWorkspace, id string) (*Experiment, error) {
	objects, err := OpenObjects(workspace.Root, id, workspace.readOnly)
	if err != nil {
		return nil, err
	}
	var journal *team.EventStore
	if workspace.readOnly {
		journal, err = team.OpenEventStoreReadOnly(workspace.Root)
	} else {
		journal, err = team.OpenEventStore(workspace.Root)
	}
	if err != nil {
		return nil, errors.Join(err, objects.Close())
	}
	return &Experiment{workspace: workspace, objects: objects, journal: journal}, nil
}

func (e *Experiment) Close() error {
	// The caller invokes this only after Run has drained its executor.
	return errors.Join(e.journal.Close(), e.objects.Close())
}

func (e *Experiment) Report(ctx context.Context) (ExperimentReport, error) {
	state, err := e.controller.Read(ctx)
	if err != nil {
		return ExperimentReport{}, err
	}
	return ProjectReport(ctx, e.objects, e.Plan, state, false)
}

// RunManual advances only planned slots. Previously admitted work is reconciled
// first and never redispatched by replay. progress receives the same projection
// consumed by Report and does not own any runtime resource.
func (e *Experiment) RunManual(ctx context.Context, build BuildIdentity, settings RuntimeSettings, progress func(LedgerState)) (ExperimentReport, error) {
	if e.workspace.readOnly || e.Plan.Mode != "manual" {
		return ExperimentReport{}, errors.New("manual execution requires a writable manual experiment")
	}
	if build.BinaryDigest != e.Plan.BinaryDigest || build.SourceRevision != e.Plan.SourceRevision {
		return ExperimentReport{}, errors.New("runtime build drift; start a new experiment")
	}
	state, err := e.controller.Read(ctx)
	if err != nil {
		return ExperimentReport{}, err
	}
	if state.Report != nil {
		return e.Report(ctx)
	}
	children, err := e.workspace.ChildArea(e.Plan.ExperimentID)
	if err != nil {
		return ExperimentReport{}, err
	}
	executor, err := NewProbeExecutor(e.prepared, e.Plan, e.objects, children, settings)
	if err != nil {
		return ExperimentReport{}, err
	}
	bindings, err := FrozenBindings(e.objects, e.Plan)
	if err != nil {
		return ExperimentReport{}, err
	}
	for _, binding := range bindings {
		if err := e.controller.BindVariant(ctx, binding); err != nil {
			return ExperimentReport{}, err
		}
	}
	runErr := e.controller.Reconcile(ctx, executor)
	specs, _, err := plannedStages(e.Plan)
	if err != nil {
		return ExperimentReport{}, err
	}
	for _, spec := range specs {
		if runErr != nil {
			break
		}
		state, err = e.controller.Read(ctx)
		if err != nil {
			runErr = err
			break
		}
		if progress != nil {
			progress(state)
		}
		stage := state.Stages[spec.ID]
		if stage.State != "planned" {
			continue
		}
		if state.StopReason != "" || state.ResourceOver || state.Budget.UnknownStages > 0 || state.Budget.ActiveStages > 0 {
			runErr = errors.New("experiment admission stopped by unresolved evidence or limits")
			break
		}
		observed := progressStageExecutor{StageExecutor: executor, admitted: func() {
			if progress != nil {
				if current, err := e.controller.Read(ctx); err == nil {
					progress(current)
				}
			}
		}}
		runErr = e.controller.Execute(ctx, spec.ID, observed)
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		runErr = errors.Join(runErr, ctx.Err(), e.controller.Stop(cleanup, "cancel_requested"))
	}
	state, err = e.controller.Read(cleanup)
	if err != nil {
		return ExperimentReport{}, errors.Join(runErr, err)
	}
	if state.StopReason != "" || state.ResourceOver {
		for _, spec := range specs {
			if state.Stages[spec.ID].State == "planned" {
				reason, status := "resource_limits", "skipped_budget"
				if state.StopReason == "cancel_requested" {
					reason, status = "cancel_requested", "cancelled"
				}
				if err := e.controller.Transition(cleanup, spec.ID, 0, status, reason); err != nil {
					return ExperimentReport{}, errors.Join(runErr, err)
				}
			}
		}
		state, err = e.controller.Read(cleanup)
		if err != nil {
			return ExperimentReport{}, errors.Join(runErr, err)
		}
	}
	if progress != nil {
		progress(state)
	}
	canFinish := state.Budget.ActiveStages == 0 && state.Budget.UnknownStages == 0 && state.Budget.ReservedTokens == 0
	report, err := ProjectReport(cleanup, e.objects, e.Plan, state, canFinish)
	if err != nil {
		return ExperimentReport{}, errors.Join(runErr, err)
	}
	if canFinish && report.Decision != "blocked" {
		ref, err := e.objects.PutJSON("experiment_report", report)
		if err == nil {
			err = e.controller.Finish(cleanup, ref, report.Decision)
		}
		runErr = errors.Join(runErr, err)
	}
	return report, runErr
}

type progressStageExecutor struct {
	StageExecutor
	admitted func()
}

func (e progressStageExecutor) Execute(ctx context.Context, request StageRequest, beforeProvider func(paired.RuntimeIdentity) error) (Settlement, error) {
	return e.StageExecutor.Execute(ctx, request, func(identity paired.RuntimeIdentity) error {
		if err := beforeProvider(identity); err != nil {
			return err
		}
		e.admitted()
		return nil
	})
}
