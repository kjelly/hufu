package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

// These integration tests use the public constructor because policy admission
// needs registered, provider-bound backends. Every model boundary is a local
// rejecting fixture; an accidental dispatch can never contact a real service.
func newMediaPolicyCoordinator(t *testing.T, limits ...media.Limits) *Coordinator {
	return newMediaPolicyCoordinatorWithTransport(t, "http://fixture.invalid/v1", nil, providerintrospection.CapabilityUnknown, nil, limits...)
}

func newMediaPolicyCoordinatorWithTransport(t *testing.T, endpoint string, factory agent.ProviderBoundaryFactory, imageInput providerintrospection.CapabilityState, configure func(*TeamSession), limits ...media.Limits) *Coordinator {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	control, subject := filepath.Join(root, "control"), filepath.Join(root, "subject")
	for _, dir := range []string{control, subject} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	generation := agent.GenerationParams{Model: "fixture/vision", ContextWindow: 32768, MaxTokens: "1024"}
	worker := testWorkerDef()
	worker.Generation = generation
	worker.Memory.Mode = agent.WorkerMemoryOff
	worker.MaxRetries = 0
	session := &TeamSession{Dir: root, Workspace: control, Agents: map[string]*agent.AgentDef{"worker": worker},
		Config: agent.TeamConfig{Name: "media-fixture", Generation: generation, WorkerModel: generation.Model, CoordinatorModel: generation.Model, DefaultLLMBackend: "fixture", MaxRetries: 0,
			Providers: map[string]config.ProviderConfig{"fixture": {ProviderURL: endpoint, MaxConcurrent: 1}}},
		MediaPolicy: &media.Policy{Enabled: true, AllowedTargets: []media.Target{{Backend: "fixture", ProviderKey: "fixture", Model: "vision", ImageTokenUpperBound: 8192}}}}
	if len(limits) > 0 {
		session.MediaPolicy.Image = limits[0]
	}
	if configure != nil {
		configure(session)
	}
	if err := session.SetCompatibilityWorkspaceScope(subject); err != nil {
		t.Fatal(err)
	}
	if factory == nil {
		factory = func(context.Context, string, providerproxy.Config) (providerproxy.Boundary, error) {
			t.Error("media unexpectedly dispatched a provider")
			return nil, errors.New("fixture rejects provider dispatch")
		}
	}
	c, err := NewCoordinator(session, endpoint, "", nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false, WithProviderBoundaryFactory(factory))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// Metadata resolution is offline too. Unknown image support must reject
	// before the provider boundary, without even trying fixture.invalid.
	c.modelProfileRuntime.resolver = modelprofile.NewRuntimeResolver(func(providerintrospection.ProviderRef) providerintrospection.ModelIntrospector {
		return imageProfileIntrospector{info: providerintrospection.RuntimeModelInfo{ConfiguredContext: 32768,
			ImageInput: imageInput, Capabilities: []string{"tools"}}}
	}, modelprofile.ProfileCacheOptions{})
	c.SetSessionData(NewSession())
	c.SetInvocationID(NewInvocationID())
	return c
}

func openMediaInvocation(t *testing.T, c *Coordinator) context.Context {
	t.Helper()
	ctx, end, err := c.beginPublicInvocationExecutionRun(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(end)
	if err := c.checkRunAdmission(); err != nil {
		t.Fatal(err)
	}
	return ctx
}

func admittedMediaTask(t *testing.T, c *Coordinator, ctx context.Context, turn string) TaskDef {
	t.Helper()
	path := filepath.Join(c.session.Workspace, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	// Generic M0 binding fixtures remain mode-neutral and can exercise
	// independent owners. Public ingress assigns a supported execution mode.
	task, err := c.prepareTaskMedia(ctx, TaskDef{Agent: "worker", Goal: "compare image"}, MediaTurnRequest{TurnID: turn, Images: []string{path, path}}, "")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func requireMediaCode(t *testing.T, err error, code media.Code) {
	t.Helper()
	if typed, ok := errors.AsType[*media.Error](err); !ok || typed.Code != code {
		t.Fatalf("error=%v want %s", err, code)
	}
}

func TestMediaInputAdmission(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "turn")
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.snapshots) != 1 || len(task.MediaBinding.Selections) != 2 || len(task.MediaBinding.Refs) != 2 {
		t.Fatal("input lost repeated occurrences")
	}
	var snapshot media.InputSnapshot
	for _, s := range view.snapshots {
		snapshot = s
	}
	if len(snapshot.Refs) != 1 || len(snapshot.Occurrences) != 2 {
		t.Fatal("content deduplication changed occurrence count")
	}
	if _, err := c.admitMediaInput(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	again, err := c.mediaLineage(ctx)
	if err != nil || len(again.snapshots) != 1 {
		t.Fatalf("idempotent admission: %v", err)
	}
	changed := snapshot.Clone()
	changed.Input.Parts[0].Text = "changed"
	changed, err = media.SealSnapshot(changed, media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.admitMediaInput(ctx, changed)
	requireMediaCode(t, err, media.BindingConflict)
	changed = snapshot.Clone()
	changed.RunID = "other-run"
	for i := range changed.Occurrences {
		changed.Occurrences[i].RunID = changed.RunID
	}
	changed, err = media.SealSnapshot(changed, media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.admitMediaInput(ctx, changed)
	requireMediaCode(t, err, media.BindingConflict)
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToSessionData(events)
	if err := compareMediaInputSnapshotProjection(c.SessionData().MediaInputSnapshots, replayed.MediaInputSnapshots); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(replayed)
	if err != nil {
		t.Fatal(err)
	}
	var restored SessionData
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := compareMediaInputSnapshotProjection(restored.MediaInputSnapshots, replayed.MediaInputSnapshots); err != nil {
		t.Fatal(err)
	}
}

func TestMediaPolicyFreezesIdentity(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	base := c.executionPolicy.snapshot
	c.session.MediaPolicy.AllowedTargets = []media.Target{{Backend: "fixture", ProviderKey: "fixture", Model: "vision", ImageTokenUpperBound: 16384}}
	state, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if state.snapshot.Media == nil || state.snapshot.Media.Policy.AllowedTargets[0].IdentityHash == "" || state.snapshot.ConfigurationHash == base.ConfigurationHash {
		t.Fatal("media target identity was not frozen")
	}
	tampered := cloneExecutionPolicySnapshot(state.snapshot)
	tampered.Media.Policy.AllowedTargets[0].IdentityHash = "changed"
	if err := validateMediaPolicySnapshot(tampered.Media, tampered); err == nil {
		t.Fatal("target substitution accepted")
	}
	legacy := cloneExecutionPolicySnapshot(state.snapshot)
	legacy.Version = executionPolicySnapshotVersion - 1
	if err := validateMediaPolicySnapshot(legacy.Media, legacy); err == nil {
		t.Fatal("legacy policy enabled media")
	}
	c.session.MediaPolicy.AllowedTargets[0].Model = "unconfigured-model"
	if _, err := newExecutionPolicyState(c); err == nil {
		t.Fatal("unknown image target accepted")
	}
	c.session.MediaPolicy = nil
	plain, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	c.session.Config.Media = &media.Policy{Enabled: true}
	teamOnly, err := newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	if teamOnly.snapshot.Media != nil || plain.snapshot.ConfigurationHash != teamOnly.snapshot.ConfigurationHash {
		t.Fatal("team enabled media without operator grant or changed text-only hash")
	}
}

func TestMediaInputCommitFailureDoesNotAdmitOrDeleteContent(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	task := admittedMediaTask(t, c, ctx, "first")
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot media.InputSnapshot
	for _, s := range view.snapshots {
		snapshot = s.Clone()
	}
	snapshot.TurnID = "second"
	for i := range snapshot.Occurrences {
		snapshot.Occurrences[i].TurnID = snapshot.TurnID
	}
	snapshot, err = media.SealSnapshot(snapshot, media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.admitMediaInput(ctx, snapshot); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
	store, err := OpenFileArtifactStoreReadOnly(c.session.Workspace, c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(t.Context(), imageArtifactRef(task.MediaBinding.Refs[0])); err != nil {
		t.Fatal(err)
	}
	view, err = c.mediaLineage(t.Context())
	if err != nil || len(view.snapshots) != 1 {
		t.Fatalf("canceled input projected: %v", err)
	}
}

type mediaAppendFailure struct {
	ConditionalEventJournal
	afterCommit bool
	failure     error
}

func (j mediaAppendFailure) AppendAtHead(ctx context.Context, e RunEvent, head string) (RunEvent, error) {
	if j.afterCommit {
		if _, err := j.ConditionalEventJournal.AppendAtHead(ctx, e, head); err != nil {
			return RunEvent{}, err
		}
	}
	return RunEvent{}, j.failure
}

func TestMediaInputReconcilesUncertainCommit(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		t.Run(map[bool]string{false: "before-commit", true: "after-commit"}[afterCommit], func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			_ = admittedMediaTask(t, c, ctx, "first")
			view, err := c.mediaLineage(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot media.InputSnapshot
			for _, s := range view.snapshots {
				snapshot = s.Clone()
			}
			snapshot.TurnID = "second"
			for i := range snapshot.Occurrences {
				snapshot.Occurrences[i].TurnID = snapshot.TurnID
			}
			snapshot, err = media.SealSnapshot(snapshot, media.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			original := c.EventJournal().(ConditionalEventJournal)
			failure := errors.New("fixture media append failure")
			c.SetEventJournal(mediaAppendFailure{ConditionalEventJournal: original, afterCommit: afterCommit, failure: failure})
			if _, err := c.admitMediaInput(ctx, snapshot); !errors.Is(err, failure) {
				t.Fatalf("append: %v", err)
			}
			if len(c.SessionData().MediaInputSnapshots) != 1 {
				t.Fatal("failed append published a live projection")
			}
			c.SetEventJournal(original)
			if _, err := c.admitMediaInput(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			if len(c.SessionData().MediaInputSnapshots) != 2 {
				t.Fatal("retry did not reconcile the canonical projection")
			}
		})
	}
}

func TestSingleTaskMediaOfflineBoundary(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	worker := scriptWorker()
	c.workerAgentOverride = worker
	path := filepath.Join(c.session.Workspace, "single.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "describe the image"}, MediaTurnRequest{Images: []string{path}})
	if err == nil {
		t.Fatal("M0 image task completed without image transport")
	}
	if worker.Calls() != 0 {
		t.Fatal("unsupported media reached a worker")
	}
	items := c.taskTracker.TodoList().Items()
	if len(items) != 1 || items[0].MediaBinding == nil || items[0].Status != TaskBlocked {
		t.Fatalf("M0 occurrence: %+v error=%v", items, err)
	}
	if len(c.SessionData().MediaInputSnapshots) != 1 {
		t.Fatal("input turn not committed")
	}
}
