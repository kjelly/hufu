//go:build linux

package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestCodexCancelledTurnSendsInterruptWithAcknowledgedIdentity(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const threadID, turnID = "cancel-late-thread", "cancel-late-turn"
	workspace := newCodexWorkspace(t)
	gate := filepath.Join(t.TempDir(), "turn-start-reply")
	_, provider, item, callsPath := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t), codexThreadStartStep(t, threadID, workspace),
		{ReplyGate: gate, Result: rawJSON(t, map[string]any{"turn": map[string]string{"id": turnID}})}, {},
	})
	provider.config.InterruptGrace = "2s"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	gateResult := releaseCodexReplyAfterCancel(gate, cancel)
	result, err := provider.RunAttempt(ctx, codexAttemptRequest(item, "do the work"))
	if gateErr := <-gateResult; gateErr != nil {
		t.Fatal(gateErr)
	}
	if failure, ok := errors.AsType[*CodexProviderError](err); !ok || failure.Class != CodexFailureCancelled || result.ProviderTurnID != turnID {
		t.Fatalf("canceled delayed turn = %+v, error %v", result, err)
	}
	assertCodexInterruptIdentity(t, callsPath, threadID, turnID)
}

func releaseCodexReplyAfterCancel(gate string, cancel context.CancelFunc) <-chan error {
	gateResult := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(gate + ".waiting"); err == nil {
				cancel()
				gateResult <- os.WriteFile(gate, nil, 0o600)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		gateResult <- errors.New("script did not receive turn/start")
	}()
	return gateResult
}

func assertCodexInterruptIdentity(t *testing.T, callsPath, threadID, turnID string) {
	t.Helper()
	call := findRawCall(t, readRawCalls(t, callsPath), codexMethodTurnInterrupt)
	var sent codexTurnInterruptParams
	if err := json.Unmarshal(call["params"], &sent); err != nil || sent.ThreadID != threadID || sent.TurnID != turnID {
		t.Fatalf("interrupt wire params = %+v, error %v", sent, err)
	}
}

func TestCodexCorrectionHonorsWireCancellationOrder(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "delayed ACK", true: "completed before cancel"}[completed], func(t *testing.T) {
			const threadID = "correction-cancel-thread"
			workspace := newCodexWorkspace(t)
			correction := validProposalJSON(`"files_read":["original.txt"]`)
			steps := codexRepairScript(t, workspace, threadID, "", correction,
				fakeCodexStep{WriteFile: "original.txt", WriteFileContent: "work"}, fakeCodexStep{})
			turnID := "turn-repair"
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var gateResult <-chan error
			if !completed {
				gate := filepath.Join(t.TempDir(), "correction-reply")
				steps[5].ReplyGate, steps[5].Notifications = gate, nil
				steps = append(steps, fakeCodexStep{})
				gateResult = releaseCodexReplyAfterCancel(gate, cancel)
			}
			c, provider, item, callsPath := newCodexHarness(t, workspace, steps)
			provider.config.InterruptGrace = "2s"
			c.reportStatus = func(event StatusEvent) {
				if completed && event.Message == "Codex result correction accepted" {
					// The fake sends the matching terminal before its ACK.
					cancel()
				}
			}
			result, err := provider.RunAttempt(ctx, codexAttemptRequest(item, "do the work"))
			if completed {
				if err != nil || ctx.Err() == nil || result.CanonicalResult == nil ||
					len(result.CanonicalResult.FilesRead) != 1 || !result.repairProvenance.Success {
					t.Fatalf("completed correction lost its result: %+v, %v", result, err)
				}
				return
			}
			if gateErr := <-gateResult; gateErr != nil {
				t.Fatal(gateErr)
			}
			if failure, ok := errors.AsType[*CodexProviderError](err); !ok || failure.Class != CodexFailureCancelled ||
				result.CanonicalResult != nil || result.ProviderTurnID != turnID || result.repairProvenance.ProviderCorrectionTurnID != turnID {
				t.Fatalf("canceled correction returned %+v, %v", result, err)
			}
			assertCodexInterruptIdentity(t, callsPath, threadID, turnID)
		})
	}
}

func TestCodexRejectsReadonlyWorkspaceEffects(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, stage := range []string{"initialize rejection", "turn disconnect", "dangling link", "directory link"} {
		t.Run(stage, func(t *testing.T) {
			f := newVersionFixture(t)
			versionCoordinator := runtimeCoordinator(t, f)
			if err := versionCoordinator.admitWorkspaceVersion(t.Context()); err != nil {
				t.Fatal(err)
			}
			before := f.head("main")
			path := "outside.txt"
			write := fakeCodexStep{WriteFile: path, WriteFileContent: "out of scope"}
			steps := []fakeCodexStep{codexInitializeStep(t)}
			if stage == "initialize rejection" {
				write.ErrorMessage = "injected initialization rejection"
				steps[0] = write
			} else if stage == "turn disconnect" {
				write.CloseAfter = true
				steps = append(steps, codexThreadResumeStep(t, "readonly-disconnected", f.subject, codexSandboxReadOnly), write)
			} else {
				path = "outside.link"
				write = fakeCodexTurnCompletedStep(t, "readonly-turn", validProposalJSON(""))
				write.SymlinkPath = path
				write.SymlinkTarget = "missing-target"
				if stage == "dangling link" {
					write.Notifications = nil
					write.CloseAfterResponse = true
				} else {
					f.write("tracked-dir/regular", "file")
					write.SymlinkTarget = "tracked-dir"
				}
				steps = append(steps, codexThreadResumeStep(t, "readonly-disconnected", f.subject, codexSandboxReadOnly), write)
			}
			c, provider, item, _ := newCodexHarness(t, f.subject, steps)
			c.session.WorkspaceVersion = f.version
			request := codexAttemptRequest(item, "inspect only")
			request.Task.SideEffect = SideEffectNone
			result, err := provider.RunAttempt(t.Context(), request)
			if failure, ok := errors.AsType[*CodexProviderError](err); !ok || failure.Class != CodexFailureWorkspaceViolation ||
				result.CanonicalResult != nil || result.TranscriptRef == "" {
				t.Fatalf("failed readonly attempt returned %+v, error %v", result, err)
			}
			if added := result.WorkspaceDelta.Added; len(added) != 1 || added[0].Path != path {
				t.Fatalf("failed readonly attempt lost its workspace evidence: %v", added)
			}
			if write.SymlinkPath != "" && fs.FileMode(result.WorkspaceDelta.Added[0].Mode)&fs.ModeSymlink == 0 {
				t.Fatal("snapshot lost the link type")
			}
			f.reopen()
			if state := subjectState(t, f); !state.RecoveryRequired || state.RecoveryCode != "unauthorized_mutation" {
				t.Fatalf("reopened workspace recovery state = %+v", state)
			}
			versionCoordinator.checkpointWorkspaceVersion(t.Context(), &RunResult{})
			if after := f.head("main"); after.ID != before.ID {
				t.Fatalf("failed readonly attempt advanced head from %s to %s", before.ID, after.ID)
			}
		})
	}
}

func TestCodexRejectsReplacedExecutionRootWithIdenticalFiles(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	for _, mode := range []string{"directory", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			f := newVersionFixture(t)
			versionCoordinator := runtimeCoordinator(t, f)
			f.write("same.txt", "same")
			if err := versionCoordinator.admitWorkspaceVersion(ctx); err != nil {
				t.Fatal(err)
			}
			before := f.head("main")
			replacement := t.TempDir()
			for name, content := range map[string]string{".hufuignore": "", "same.txt": "same"} {
				if err := os.WriteFile(filepath.Join(replacement, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			step := fakeCodexTurnCompletedStep(t, "root-replacement-turn", validProposalJSON(""))
			step.ReplacementRoot, step.LinkToReplacement = replacement, mode == "symlink"
			c, provider, item, _ := newCodexHarness(t, newCodexWorkspace(t), []fakeCodexStep{
				codexInitializeStep(t), codexThreadResumeStep(t, "root-replacement-thread", f.subject, codexSandboxReadOnly), step,
			})
			c.projectDir, c.session.WorkspaceVersion = f.subject, f.version
			request := codexAttemptRequest(item, "read only")
			request.Task.SideEffect = SideEffectNone
			result, err := provider.RunAttempt(ctx, request)
			failure, failed := errors.AsType[*CodexProviderError](err)
			if !failed || failure.Class != CodexFailureWorkspaceViolation || !errors.Is(err, errWorkspaceSnapshotRootChanged) ||
				result.CanonicalResult != nil || result.ProviderTurnID != "root-replacement-turn" {
				t.Fatalf("provider root replacement returned %+v, error %v", result, err)
			}
			versionCoordinator.checkpointWorkspaceVersion(ctx, &RunResult{})
			f.reopen()
			state := subjectState(t, f)
			if head := f.head("main"); head.ID != before.ID || !state.RecoveryRequired || state.RecoveryCode != "provider_effects_unverified" {
				t.Fatalf("root replacement advanced head or lost its recovery marker: %s, %+v", head.ID, state)
			}
		})
	}
}

func TestCodexRedactsFrozenEnvironmentCredential(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const variable = "TEST_PROVIDER_TOKEN"
	for _, stage := range []string{"original round trip", "correction", "correction filename", "thread RPC error", "numeric RPC code"} {
		t.Run(stage, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			const threadID = "thread-inherited-secret"
			credential := "ordinary secret words from the environment"
			if stage == "numeric RPC code" {
				credential = "-32000"
			}
			secretPath := credential + ".txt"
			body := `{"status":"success","summary":"observed ` + credential + `"}`
			steps := []fakeCodexStep{codexInitializeStep(t), codexThreadStartStep(t, threadID, workspace)}
			switch stage {
			case "correction":
				steps = codexRepairScript(t, workspace, threadID, "", body, fakeCodexStep{}, fakeCodexStep{})
				steps[5].StderrOnEOF = "stderr mentions " + credential
			case "correction filename":
				steps = codexRepairScript(t, workspace, threadID, "", body,
					fakeCodexStep{WriteFile: secretPath, WriteFileContent: "safe"}, fakeCodexStep{})
			case "thread RPC error":
				steps[1] = fakeCodexStep{ErrorMessage: "thread failed with " + credential, StderrOnEOF: credential}
			case "numeric RPC code":
				steps[1] = fakeCodexStep{ErrorMessage: "thread failed", StderrOnEOF: `{"sample":-32000}`}
			default:
				steps = append(steps, fakeCodexTurnCompletedStep(t, "turn-inherited-secret", body))
				steps[2].StderrOnEOF = "stderr mentions " + credential
			}
			c, provider, item, _ := newCodexHarness(t, workspace, steps)
			t.Setenv(variable, credential)
			provider.config.InheritEnv = append(provider.config.InheritEnv, variable)
			var activity strings.Builder
			c.verbose = true
			c.reportStatus = func(event StatusEvent) { activity.WriteString(event.Message) }

			result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
			_, transcript := readCodexTranscriptArtifact(t, workspace, result.TranscriptRef)
			if strings.Contains(string(transcript), credential) || strings.Contains(activity.String(), credential) {
				t.Fatal("inherited credential reached transcript or visible activity")
			}
			if stage == "thread RPC error" || stage == "numeric RPC code" {
				if err == nil || strings.Contains(err.Error(), credential) || !strings.Contains(err.Error(), "[REDACTED]") {
					t.Fatal("thread error did not safely redact the inherited credential")
				}
				if stage == "numeric RPC code" {
					if rpcErr, ok := errors.AsType[*jsonRPCError](err); !ok || !rpcErr.codeRedacted || rpcErr.Code != 0 {
						t.Fatal("typed RPC error retained an inherited numeric credential")
					}
				}
				return
			}
			if stage == "correction filename" {
				if localResultRejectionReason(err) != RepairFailureSource || result.CanonicalResult != nil {
					t.Fatalf("credential-bearing artifact path became accepted evidence: %v", err)
				}
				if added := result.WorkspaceDelta.Added; len(added) != 1 || added[0].Path != secretPath {
					t.Fatal("original workspace effect was lost during correction")
				}
				key := externalResultCorrectionKey(c.activeBranchID(), item, item.DispatchID, 0)
				docs := workspaceDocs(workspace)
				stored, found, err := docs.getDocument(key)
				if err != nil || !found || strings.Contains(string(stored), credential) {
					t.Fatal("reopened correction work receipt missing or retained a credential-bearing filename")
				}
				if _, found, err := docs.getDocument(key + "/response"); err != nil || found {
					t.Fatal("rejected artifact path produced a durable correction response")
				}
				return
			}
			if err != nil || result.CanonicalResult == nil || result.CanonicalResult.Summary != "observed [REDACTED]" {
				t.Fatalf("canonical summary was not redacted, error %v", err)
			}
			if stage == "correction" {
				key := externalResultCorrectionKey(c.activeBranchID(), item, item.DispatchID, 0)
				for _, name := range []string{key, key + "/response"} {
					stored, found, err := workspaceDocs(workspace).getDocument(name)
					if err != nil || !found || strings.Contains(string(stored), credential) {
						t.Fatal("reopened correction evidence missing or retained inherited credential")
					}
				}
			}
		})
	}
}

func TestCodexRejectsPrivateDataBeforeBindingExternalEvidence(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const variable, plainCredential = "TEST_PROVIDER_TOKEN", "unsafe-synthetic-provider-identity"
	const threadID, turnID = "private-data-thread", "private-data-turn"
	compiled := compileFixtureJSONSchema(t, "numeric-response", `{"type":"object","required":["sample"],"properties":{"sample":{"type":"integer"}},"additionalProperties":false}`)
	for _, stage := range []string{"thread identity", "original turn identity", "correction turn identity", "numeric payload",
		"malformed thread reply id", "unknown original reply id", "unknown correction reply id"} {
		t.Run(stage, func(t *testing.T) {
			workspace := newCodexWorkspace(t)
			credential := plainCredential
			var wantDiagnostic string
			steps := []fakeCodexStep{
				codexInitializeStep(t), codexThreadStartStep(t, threadID, workspace),
				fakeCodexTurnCompletedStep(t, turnID, validProposalJSON("")),
			}
			switch stage {
			case "thread identity":
				steps[1] = codexThreadStartStep(t, "thread-"+credential, workspace)
			case "original turn identity":
				steps[2] = fakeCodexTurnCompletedStep(t, "turn-"+credential, validProposalJSON(""))
			case "correction turn identity":
				steps = codexRepairScript(t, workspace, threadID, "", validProposalJSON(""), fakeCodexStep{}, fakeCodexStep{})
				steps[5] = fakeCodexTurnCompletedStep(t, "correction-"+credential, validProposalJSON(""))
			case "numeric payload":
				credential = "49186278312"
				payload := `{"sample":` + credential + `}`
				steps[2] = fakeCodexTurnCompletedStep(t, turnID, validProposalJSON(fmt.Sprintf(`"structured_payload_json":%q`, payload)))
			case "malformed thread reply id":
				credential = "49186278312"
				steps[1].RawLine = `{"jsonrpc":"2.0","id":-` + credential + `,"result":{}}`
				wantDiagnostic = "response with an invalid id"
			case "unknown original reply id", "unknown correction reply id":
				credential = "49186278312"
				if stage == "unknown correction reply id" {
					steps = codexRepairScript(t, workspace, threadID, "", validProposalJSON(""), fakeCodexStep{}, fakeCodexStep{})
				}
				last := &steps[len(steps)-1]
				last.Notifications = nil
				last.AfterResponseLine = `{"jsonrpc":"2.0","id":` + credential + `,"result":{}}`
				wantDiagnostic = "response for an unknown request id"
			}
			t.Setenv(variable, credential)
			worker := &agent.AgentDef{
				Name: "worker", Role: "worker", SubagentProvider: "codex", Timeout: 10,
				Generation: agent.GenerationParams{Model: "gpt-5-codex"},
			}
			c := newCodexE2ECoordinatorBase(t, workspace, steps, worker)
			config := c.session.Config.SubagentProviders["codex"]
			config.InheritEnv = append(config.InheritEnv, variable)
			c.session.Config.SubagentProviders["codex"] = config
			c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			item := admitCodexE2ETaskWithSpec(t, c, worker, nil, func(spec *TodoSpec) {
				if stage == "numeric payload" {
					spec.ResultContract = new(compiled.ref(true))
				}
			})

			_, err := c.executeTask(t.Context(), taskDefFromTodoItem(item), item.ID)
			got := c.todoItemByID(item.ID)
			if err == nil || strings.Contains(err.Error(), credential) || got.TypedResult != nil || got.Status == TaskDone {
				t.Fatal("private data escaped provider evidence validation")
			}
			if stage == "numeric payload" {
				if localResultRejectionReason(err) != RepairFailureSource || got.ExecutionReceipt == nil ||
					got.ExecutionReceipt.WorkSubmissionFailure != RepairFailureSource {
					t.Fatalf("numeric payload rejection did not preserve its classification: %v", err)
				}
			} else if wantDiagnostic != "" {
				if !strings.Contains(err.Error(), wantDiagnostic) {
					t.Fatalf("transport rejection lost its diagnostic: %v", err)
				}
			} else if !errors.Is(err, errCodexPrivateProviderIdentity) {
				t.Fatalf("unexpected rejection for provider identity: %v", err)
			}
			store, err := OpenEventStoreReadOnly(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(events)
			if err != nil || strings.Contains(string(data), credential) {
				t.Fatal("reopened event history retained private data")
			}
			receipt := got.ExecutionReceipt
			if receipt == nil || receipt.ProviderTranscriptRef == "" {
				t.Fatal("rejected provider run lost its diagnostic transcript reference")
			}
			_, transcript := readCodexTranscriptArtifact(t, workspace, receipt.ProviderTranscriptRef)
			if strings.Contains(string(transcript), credential) || !strings.Contains(string(transcript), wantDiagnostic) {
				t.Fatal("reopened transcript retained private data or lost its diagnostic")
			}
		})
	}
}

func TestLocalExecutionWorldLaunchKeepsAdmittedDirectory(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	cwd := filepath.Join(root, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	world := NewLocalExecutionWorld()
	spec := ExecutionWorldSpec{Root: root, CWD: link, SideEffect: SideEffectWorkspaceWrite}
	if escaped, err := world.Prepare(ctx, spec); err == nil {
		_ = world.Release(ctx, escaped)
		t.Fatal("admitted a working directory outside the workspace")
	}
	spec.CWD = cwd
	prepared, err := world.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = world.Release(ctx, prepared) })

	step := codexInitializeStep(t)
	step.WriteFile, step.WriteFileContent = "marker", "written by the provider"
	scriptPath := filepath.Join(parent, "script.json")
	if err := os.WriteFile(scriptPath, rawJSON(t, fakeCodexScript{Steps: []fakeCodexStep{step}}), 0o600); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}

	sup := NewProcessSupervisor()
	proc, err := StartCodexAppServer(ctx, sup, CodexProcessConfig{
		Argv: []string{os.Args[0]}, Dir: prepared.processDir(),
		Env: []string{fakeCodexServerScriptEnvVar + "=" + scriptPath}, StartupTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = proc.Client.Close()
	if !proc.waitForTreeExit(5 * time.Second) {
		_ = sup.KillTree(ctx, proc.Handle())
		t.Fatal("provider did not exit")
	}
	if _, err := os.Stat(filepath.Join(cwd, "marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider wrote to the replacement directory: %v", err)
	}
	if err := os.Rename(root, filepath.Join(parent, "replacement")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(moved, root); err != nil {
		t.Fatal(err)
	}
	final, err := world.Snapshot(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := world.snapshotter.Diff(ctx, prepared.Baseline, final)
	if err != nil || len(delta.Added) != 1 || delta.Added[0].Path != "work/marker" {
		t.Fatalf("snapshot omitted the provider write in the admitted directory: %+v, %v", delta, err)
	}
}

func TestCodexRejectsReplacedManagedSubjectBeforeLaunch(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	f := newVersionFixture(t)
	f.write("admitted.txt", "original")
	identity, err := os.Stat(f.subject)
	if err != nil {
		t.Fatal(err)
	}
	f.version.SubjectIdentity = identity
	if err := runtimeCoordinator(t, f).admitWorkspaceVersion(t.Context()); err != nil {
		t.Fatal(err)
	}
	head := f.head("main")
	c, provider, item, calls := newCodexHarness(t, f.subject, []fakeCodexStep{codexInitializeStep(t)})
	c.session.WorkspaceVersion = f.version
	original := filepath.Join(t.TempDir(), "original")
	if err := os.Rename(f.subject, original); err != nil {
		t.Fatal(err)
	}
	f.write("admitted.txt", "replacement")

	result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "inspect"))
	failure, ok := errors.AsType[*CodexProviderError](err)
	if !ok || failure.Class != CodexFailureUnavailable || !errors.Is(err, errWorkspaceSnapshotRootChanged) ||
		result.ProviderTurnID != "" || result.CanonicalResult != nil {
		t.Fatalf("attempt did not reject the unadmitted subject: %+v, %v", result, err)
	}
	if _, err := os.Stat(calls); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("provider started after the admitted workspace was replaced: %v", err)
	}
	entries, err := os.ReadDir(f.subject)
	if err != nil || len(entries) != 1 || entries[0].Name() != "admitted.txt" {
		t.Fatalf("rejection wrote into the replacement workspace: %v, %v", entries, err)
	}
	contents, err := os.ReadFile(filepath.Join(original, "admitted.txt"))
	if err != nil || string(contents) != "original" || f.read("admitted.txt") != "replacement" || f.head("main").ID != head.ID {
		t.Fatalf("rejection modified the original, replacement, or accepted head: %v", err)
	}
	state, found, err := f.store.GetSubjectState(t.Context(), f.subject)
	if err != nil || !found || state.RecoveryRequired || state.MaterializedSnapshotID != head.ID ||
		head.State != versionstore.StatePublished {
		t.Fatalf("rejection changed the published workspace state: %+v, %v", state, err)
	}
}

func TestCodexTurnsKeepAdmittedDirectoryDuringPathReplacement(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	originalReplacement, correctionReplacement := t.TempDir(), t.TempDir()
	steps := codexRepairScript(t, workspace, "thread-pinned", "", validProposalJSON(""),
		fakeCodexStep{}, fakeCodexStep{})
	steps[2].TurnDirectoryReplacement = originalReplacement
	steps[5].TurnDirectoryReplacement = correctionReplacement
	_, provider, item, calls := newCodexHarness(t, workspace, steps)
	result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
	if err != nil || result.CanonicalResult == nil {
		t.Fatalf("original turn and correction did not complete in the admitted directory: %v", err)
	}
	want := []string{"initialize", "thread/start", "turn/start", "initialize", "thread/resume", "turn/start"}
	if got := rawCallMethods(t, readRawCalls(t, calls)); !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
}

func TestCodexResultRepairStopsDescendantBeforeWorkspaceValidation(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	ready, childPID := codexDetachedDescendant(t)

	steps := codexRepairScript(t, workspace, "thread-repair-descendant", "", validProposalJSON(""),
		fakeCodexStep{WriteFile: "original.txt", WriteFileContent: "from the original turn"}, fakeCodexStep{})
	steps[5].DetachedChildReady = ready
	steps[5].CloseAfterResponse = true
	steps[5].AfterResponse, steps[5].Notifications = steps[5].Notifications, nil
	c, provider, item, calls := newCodexHarness(t, workspace, steps)
	var aliveAtCorrectionAck bool
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "codex_activity" && event.Message == "Codex result correction accepted" {
			aliveAtCorrectionAck = childPID() > 0 && processAlive(childPID())
		}
	}

	result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
	if result.CanonicalResult == nil || err != nil || len(readRawCalls(t, calls)) != 6 {
		t.Fatalf("valid correction failed: %v", err)
	}
	if pid := childPID(); !aliveAtCorrectionAck || pid <= 0 || processAlive(pid) {
		t.Fatalf("correction did not quiesce its surviving child: pid %d, alive at ack %t", pid, aliveAtCorrectionAck)
	}
	if added := result.WorkspaceDelta.Added; len(added) != 1 || added[0].Path != "original.txt" {
		t.Fatalf("correction changed the original workspace delta: %v", added)
	}
}

func TestCodexDisconnectedTurnSnapshotsDescendantEffects(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	workspace := newCodexWorkspace(t)
	ready, childPID := codexDetachedDescendant(t)
	const threadID, turnID = "thread-disconnect", "disconnected-turn"
	step := fakeCodexTurnCompletedStep(t, turnID, validProposalJSON(""))
	step.Notifications = nil
	step.CloseAfterResponse = true
	step.DetachedChildReady = ready
	step.DetachedChildTermWrite = true
	_, provider, item, _ := newCodexHarness(t, workspace, []fakeCodexStep{
		codexInitializeStep(t), codexThreadStartStep(t, threadID, workspace), step,
	})

	result, err := provider.RunAttempt(t.Context(), codexAttemptRequest(item, "do the work"))
	var providerErr *CodexProviderError
	if !errors.As(err, &providerErr) || providerErr.Class != CodexFailureProcessCrash ||
		result.ProviderTurnID != turnID || result.TranscriptRef == "" || result.CanonicalResult != nil {
		t.Fatalf("disconnected result = %+v, error %v", result, err)
	}
	if added := result.WorkspaceDelta.Added; len(added) != 1 || added[0].Path != "descendant-termination.txt" {
		t.Fatalf("snapshot omitted the descendant's last write: %v", added)
	}
	if pid := childPID(); pid <= 0 || processAlive(pid) {
		t.Fatalf("descendant %d still active after failed attempt", pid)
	}
}

func codexDetachedDescendant(t *testing.T) (string, func() int) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "descendant.ready")
	childPID := func() int {
		data, _ := os.ReadFile(ready)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		return pid
	}
	t.Cleanup(func() {
		if pid := childPID(); pid > 0 && processAlive(pid) {
			if child, err := os.FindProcess(pid); err == nil {
				_ = child.Kill()
				_ = child.Release()
			}
		}
	})
	return ready, childPID
}

type codexTerminalSamplingContext struct {
	context.Context
	sampled chan struct{}
	publish chan struct{}
}

func (c codexTerminalSamplingContext) Err() error {
	err := c.Context.Err()
	close(c.sampled)
	<-c.publish
	return err
}

func TestCodexCancellationWaitsForInFlightTerminalDecision(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	terminalCtx := codexTerminalSamplingContext{Context: ctx, sampled: make(chan struct{}), publish: make(chan struct{})}
	release := sync.OnceFunc(func() { close(terminalCtx.publish) })
	defer release()
	const threadID, turnID = "arbitrated-thread", "arbitrated-turn"
	client := &CodexRPCClient{notifications: make(chan CodexNotification, 1), closeCh: make(chan struct{}), advisoryQueueBytes: 4096}
	notification := CodexNotification{
		Method: codexNotificationTurnCompleted, terminalStatus: "completed",
		Params: json.RawMessage(`{"threadId":"arbitrated-thread","turn":{"id":"arbitrated-turn","status":"completed"}}`),
	}
	published := make(chan bool, 1)
	go func() {
		var advisoryDropped bool
		published <- client.queueNotification(notification, 4096, &advisoryDropped, &codexNotificationTarget{ctx: terminalCtx})
	}()
	select {
	case <-terminalCtx.sampled:
	case <-time.After(time.Second):
		t.Fatal("wire reader never sampled terminal disposition")
	}
	cancel()
	consumed := make(chan error, 1)
	go func() {
		turn, err := waitForCodexTurnCompleted(ctx, client, threadID, turnID, nil, false, client.newDiagnostics(nil))
		if err == nil && turn.Status != "completed" {
			err = fmt.Errorf("unexpected terminal status %q", turn.Status)
		}
		consumed <- err
	}()
	select {
	case err := <-consumed:
		t.Fatalf("waiter decided during terminal publication: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if ok := <-published; !ok {
		t.Fatal("authenticated terminal failed to publish")
	}
	select {
	case err := <-consumed:
		if err != nil || !client.completedBeforeCancellation() {
			t.Fatalf("the winning terminal was lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive the winning terminal")
	}
}
