package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/processutil"
)

const codexImageRecoveryMode = "HUFU_TEST_CODEX_IMAGE_RECOVERY_MODE"
const codexImageRecoveryRoot = "HUFU_TEST_CODEX_IMAGE_RECOVERY_ROOT"
const codexImageRecoveryHome = "HUFU_TEST_CODEX_IMAGE_RECOVERY_HOME"

// The first OS process exits after durable admission, before turn/start. Its
// projection and session survive only on disk. A different process resumes
// from the canonical CAS, including when the projection copy was reclaimed.
func TestCodexNativeImageResumeInNewProcess(t *testing.T) {
	if mode := os.Getenv(codexImageRecoveryMode); mode != "" {
		root, home := os.Getenv(codexImageRecoveryRoot), os.Getenv(codexImageRecoveryHome)
		digest := sha256.Sum256(mediaStorePNG(t))
		hash := hex.EncodeToString(digest[:])
		c, logPath := newCodexImageCoordinatorAtRoot(t, root, home, codexImageProtocolVersion, func(subject string) []fakeCodexStep {
			return codexImageSteps(t, subject, validProposalJSON(""), []string{hash, hash})
		})
		if mode == "prepare" {
			ctx := openMediaInvocation(t, c)
			task := nativeInvocationTask(t, c, ctx, media.InvocationSingleTask)
			task.Execution.RequiresResult = true
			spec, id := prepareMediaOccurrenceForModel(t, c, ctx, task, "codex/gpt-5-codex")
			if _, err := c.CommitTaskCreationResolved(ctx, []TodoSpec{spec}, []string{id}); err != nil {
				t.Fatal(err)
			}
			if err := c.CommitTaskTransition(ctx, id, TaskPending, TaskInProgress, "running", "", nil); err != nil {
				t.Fatal(err)
			}
			c.setCurrentTaskAttempt(id, 1)
			identity, ok := c.activeTaskResultOccurrence(id)
			if !ok {
				t.Fatal("missing lease")
			}
			ctx = withSubmitResultRuntimeIdentity(ctx, identity)
			item := c.todoItemByID(id)
			ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: c.executionRunID, TaskID: id, AgentName: item.Agent, AgentRole: "worker", Attempt: 1, Purpose: "task_execution"})
			request := AttemptRequest{RunID: c.executionRunID, BranchID: c.activeBranchID(), TaskID: id, Attempt: 1, Agent: c.session.Agents["worker"], Task: taskDefFromTodoItem(item), Prompt: item.Goal, ExecutionTarget: item.ExecutionTarget, ModelID: item.ExecutionTarget.Model}
			backend, err := c.ExecutionRegistry().ResolveBackend("codex")
			if err != nil {
				t.Fatal(err)
			}
			provider := backend.(*AgentExecutionBackend).provider.(*CodexSubagentProvider)
			// This fixture has no economic policy. The production backend tests cover
			// reservation linkage; the interrupted admission records no usage claim.
			c.costManager = nil
			input, err := provider.prepareImageAttempt(ctx, request, provider.childEnvironment())
			if err != nil {
				t.Fatal(err)
			}
			if err := c.persistProviderSessionBinding(ctx, id, 1, ProviderBinding{Provider: "codex", Protocol: "app-server-v2", SessionID: "thread-1", CWD: c.projectDir, EffectiveModel: "gpt-5-codex", SandboxMode: codexSandboxReadOnly, ResumeSupported: true}); err != nil {
				t.Fatal(err)
			}
			if err := provider.admitImageTurn(ctx, request, input); err != nil {
				t.Fatal(err)
			}
			c.saveCheckpoint()
			// Intentional process death: the parent owns and cleans this exact root.
			// No app-server remains alive; the version probe has already been reaped.
			os.Exit(0)
		}
		checkpoint := LoadSession(filepath.Join(root, "control"))
		if checkpoint == nil || len(checkpoint.Tasks) != 1 {
			t.Fatal("missing recovery checkpoint")
		}
		c.SetSessionData(checkpoint)
		ctx := openMediaInvocation(t, c)
		count, err := c.ResumeInterruptedTasks(ctx)
		item := c.todoItemByID(checkpoint.Tasks[0].ID)
		if mode == "tampered" {
			if count == 0 || item == nil || item.Status == TaskDone {
				t.Fatalf("tampered projection accepted: count=%d err=%v item=%+v", count, err, item)
			}
			for _, call := range codexImageWireCalls(t, logPath) {
				if call.Method == codexMethodTurnStart {
					t.Fatal("tampered projection reached image turn")
				}
			}
			return
		}
		if err != nil || count != 1 || item == nil || item.Status != TaskDone {
			t.Fatalf("resume failed count=%d err=%v item=%+v", count, err, item)
		}
		resumed := false
		for _, call := range codexImageWireCalls(t, logPath) {
			if call.Method == codexMethodThreadResume {
				resumed = true
			}
			if call.Method == codexMethodThreadStart {
				t.Fatal("resume created a fresh thread")
			}
		}
		if !resumed {
			t.Fatal("durable thread was not resumed")
		}
		events, err := c.EventJournal().ReadEvents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, scenario := range []string{"matching", "missing", "tampered"} {
		t.Run(scenario, func(t *testing.T) {
			root, home := mediaStoreDirectory(t), mediaStoreDirectory(t)
			runCodexImageRecoveryProcess(t, "prepare", root, home)
			checkpoint := LoadSession(filepath.Join(root, "control"))
			if checkpoint == nil || len(checkpoint.Tasks) != 1 {
				t.Fatal("missing prepared task")
			}
			item := checkpoint.Tasks[0]
			hash, err := codexImageProjectionHash(item.MediaBinding, item.ExecutionTarget.String())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "control", logsDir, "media-projections", "input-"+strings.TrimPrefix(hash, "sha256:"), codexImageFilename(item.MediaBinding.Refs[0]))
			if err := os.Remove(filepath.Join(root, "control", "input.png")); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "tampered":
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("changed"), 0o400); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			mode := "resume"
			if scenario == "tampered" {
				mode = scenario
			}
			runCodexImageRecoveryProcess(t, mode, root, home)
		})
	}
}

func runCodexImageRecoveryProcess(t *testing.T, mode, root, home string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCodexNativeImageResumeInNewProcess$", "-test.timeout=40s")
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, fakeCodexServerScriptEnvVar+"=") || strings.HasPrefix(entry, codexImageRecoveryMode+"=") || strings.HasPrefix(entry, codexImageRecoveryRoot+"=") || strings.HasPrefix(entry, codexImageRecoveryHome+"=") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, codexImageRecoveryMode+"="+mode, codexImageRecoveryRoot+"="+root, codexImageRecoveryHome+"="+home)
	output := newBoundedBuffer(32 << 10)
	command.Stdout = output
	command.Stderr = output
	if err := processutil.RunContextCommand(command, time.Second); err != nil {
		t.Fatalf("Codex image recovery %s: %v\n%s", mode, err, output.String())
	}
}
