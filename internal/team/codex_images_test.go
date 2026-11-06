package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
)

func newCodexImageCoordinator(t *testing.T, version string, steps func(string) []fakeCodexStep, configure ...func(*TeamSession)) (*Coordinator, string) {
	t.Helper()
	return newCodexImageCoordinatorAtRoot(t, mediaStoreDirectory(t), mediaStoreDirectory(t), version, steps, configure...)
}

func newCodexImageCoordinatorAtRoot(t *testing.T, root, home, version string, steps func(string) []fakeCodexStep, configure ...func(*TeamSession)) (*Coordinator, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Codex ProcessSupervisor is not implemented on Windows")
	}
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), "codex/gpt-5-codex")
	preserveRegisteredModelSpec(t, GlobalModelSpecRegistry(), "gpt-5-codex")
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(home, "script.json")
	logPath := filepath.Join(home, "calls.jsonl")
	t.Setenv("CODEX_HOME", home)
	t.Setenv(fakeCodexServerScriptEnvVar, scriptPath)
	c := newMediaPolicyCoordinatorAtRoot(t, root, "http://fixture.invalid/v1", nil, providerintrospection.CapabilityYes, func(session *TeamSession) {
		session.Agents["worker"].Generation.Model = "codex/gpt-5-codex"
		session.Config.WorkerModel = "codex/gpt-5-codex"
		session.Config.SubagentProviders = map[string]agent.SubagentProviderConfig{"codex": {Type: codexAppServerProviderType, Command: []string{os.Args[0]}, Protocol: "app-server-v2", InheritEnv: []string{"CODEX_HOME", fakeCodexServerScriptEnvVar}, StartupTimeout: "5s", InterruptGrace: "100ms", ShutdownGrace: "100ms"}}
		session.MediaPolicy.AllowedTargets = []media.Target{{Backend: "codex", Model: "gpt-5-codex", ImageTokenUpperBound: 4096}}
		for _, apply := range configure {
			apply(session)
		}
	})
	c.reportStatus = func(event StatusEvent) {
		if event.Type == "error" || event.Type == "task_failed" {
			t.Log(event.Message)
		}
	}
	script := fakeCodexScript{ImageProtocolVersion: version, Steps: steps(c.projectDir), RawLogPath: logPath}
	encoded, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return c, logPath
}

func codexImageSteps(t *testing.T, root, output string, hashes []string) []fakeCodexStep {
	t.Helper()
	turn := fakeCodexTurnCompletedStep(t, "turn-1", output)
	turn.ImageHashes = hashes
	return []fakeCodexStep{codexInitializeStep(t), fakeCodexThreadStartStep("thread-1", root, "gpt-5-codex", codexSandboxReadOnly, t), turn}
}

func codexImageWireCalls(t *testing.T, path string) []struct {
	Method string `json:"method"`
	Params struct {
		Input []codexUserInput `json:"input"`
	} `json:"params"`
} {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []struct {
		Method string `json:"method"`
		Params struct {
			Input []codexUserInput `json:"input"`
		} `json:"params"`
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var call struct {
			Method string `json:"method"`
			Params struct {
				Input []codexUserInput `json:"input"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func TestCodexNativeImagesReachAppServer(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(fmt.Sprintf("direct=%t", direct), func(t *testing.T) { testCodexNativeImageWire(t, direct) })
	}
}

func testCodexNativeImageWire(t *testing.T, direct bool) {
	data := mediaStorePNG(t)
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	var second bytes.Buffer
	if err := png.Encode(&second, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	secondDigest := sha256.Sum256(second.Bytes())
	secondHash := hex.EncodeToString(secondDigest[:])

	c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep {
		return codexImageSteps(t, root, validProposalJSON(""), []string{hash, secondHash, hash})
	})
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(c.projectDir, "second.png")
	if err := os.WriteFile(secondPath, second.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	var runErr error
	if direct {
		_, runErr = c.RunDirectAgentWithMedia(t.Context(), "worker", "Compare these images.", MediaTurnRequest{Images: []string{path, secondPath, path}})
	} else {
		_, runErr = c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Compare these images.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path, secondPath, path}})
	}
	items := c.taskTracker.TodoList().Items()
	for _, item := range items {
		if item.Status != TaskDone {
			t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
		}
	}
	if len(items) != 1 || items[0].Status != TaskDone {
		t.Fatalf("items=%+v run=%v", items, runErr)
	}
	calls := codexImageWireCalls(t, logPath)
	var inputs []codexUserInput
	for _, call := range calls {
		if call.Method == codexMethodTurnStart {
			if inputs != nil {
				t.Fatal("image work replayed")
			}
			inputs = call.Params.Input
		}
	}
	if len(inputs) != 4 || inputs[0].Type != "text" || inputs[1].Type != "localImage" || inputs[1].Path != inputs[3].Path || inputs[1].Path == inputs[2].Path || inputs[1].Path == path {
		t.Fatalf("wrong native input: %+v", inputs)
	}
	schema := pinnedCodexUserInputSchema(t)
	for _, input := range inputs {
		raw, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(inputs[1].Path); !os.IsNotExist(err) {
		t.Fatalf("projection survived child drain: %v", err)
	}
	events, err := readValidatedEventJournal(t.Context(), c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	var admissions []MediaProviderAdmittedPayload
	for _, event := range events {
		if event.Type == string(EventMediaProviderAdmitted) {
			payload, err := decodeMediaProviderEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			admissions = append(admissions, payload)
		}
	}
	if len(admissions) != 1 || admissions[0].Codex.Version != codexImageProtocolVersion || admissions[0].ImageCount != 3 || admissions[0].ImageTokens != 12288 || admissions[0].ProviderKey != "" {
		t.Fatalf("wrong adapter admission: %+v", admissions)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"version", "binary", "projection", "capacity source"} {
		changed := slices.Clone(events)
		for index, event := range changed {
			if event.Type == string(EventMediaProviderAdmitted) {
				proof, err := decodeMediaProviderEvent(event)
				if err != nil {
					t.Fatal(err)
				}
				switch variant {
				case "version":
					proof.Codex.Version = "0.167.0"
				case "binary":
					proof.Codex.BinaryHash = "changed"
				case "projection":
					proof.Codex.ProjectionHash = "changed"
				case "capacity source":
					proof.Codex.ContextSource = "fallback"
				}
				encoded, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				changed[index].Payload = encoded
			}
		}
		if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, changed); err == nil {
			t.Fatalf("audit accepted changed Codex %s", variant)
		}
	}
}

func TestCodexNativeImagesRejectUnknownVersionBeforeAppServer(t *testing.T) {
	c, logPath := newCodexImageCoordinator(t, "0.167.0", func(root string) []fakeCodexStep { return codexImageSteps(t, root, validProposalJSON(""), nil) })
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Describe image.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
	items := c.taskTracker.TodoList().Items()
	for _, item := range items {
		if item.Status != TaskDone {
			t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
		}
	}
	if len(items) != 1 || items[0].Status == TaskDone {
		t.Fatal("unverified version accepted image work")
	}
	if calls := codexImageWireCalls(t, logPath); len(calls) != 0 {
		t.Fatalf("unsupported version started app-server: %+v", calls)
	}
	if !strings.Contains(items[0].Detail, codexImageProtocolVersion) {
		t.Fatalf("missing version diagnostic: %s", items[0].Detail)
	}
}

func TestCodexImageInputUnionRejectsMixedVariants(t *testing.T) {
	for _, input := range []codexUserInput{{Type: "text", Text: "x", Path: "/image"}, {Type: "localImage", Path: "/image", Text: "x"}, {Type: "localImage", Path: "relative"}, {Type: "image", Path: "/image"}} {
		if _, err := json.Marshal(input); err == nil {
			t.Fatalf("invalid input serialized: %+v", input)
		}
	}
}

func TestCodexNativeImageResultRepairSendsOnlyText(t *testing.T) {
	data := mediaStorePNG(t)
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep {
		steps := codexImageSteps(t, root, "invalid proposal", []string{hash})
		return append(steps, codexInitializeStep(t), fakeCodexThreadStartStep("thread-1", root, "gpt-5-codex", codexSandboxReadOnly, t), fakeCodexTurnCompletedStep(t, "repair-1", validProposalJSON("")))
	})
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, runErr := c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Inspect image once.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
	items := c.taskTracker.TodoList().Items()
	for _, item := range items {
		if item.Status != TaskDone {
			t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
		}
	}
	if len(items) != 1 || items[0].Status != TaskDone {
		t.Fatalf("repair did not complete: %+v run=%v", items, runErr)
	}
	var turns [][]codexUserInput
	for _, call := range codexImageWireCalls(t, logPath) {
		if call.Method == codexMethodTurnStart {
			turns = append(turns, call.Params.Input)
		}
	}
	if len(turns) != 2 || len(turns[0]) != 2 || len(turns[1]) != 1 || turns[1][0].Type != "text" {
		t.Fatalf("repair replayed images: %+v", turns)
	}
	if _, err := os.Lstat(turns[0][1].Path); !os.IsNotExist(err) {
		t.Fatal("projection survived repair child drain")
	}
}

func TestCodexNativeImagesTimeoutDrainsBeforeCleanup(t *testing.T) {
	data := mediaStorePNG(t)
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep {
		steps := codexImageSteps(t, root, validProposalJSON(""), []string{hash})
		steps[len(steps)-1] = fakeCodexStep{NoResponse: true, ImageHashes: []string{hash}}
		return steps
	}, func(session *TeamSession) { session.Agents["worker"].Timeout = 10 })
	path := filepath.Join(c.projectDir, "input.png")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, _ = c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Inspect image.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
	var input []codexUserInput
	for _, call := range codexImageWireCalls(t, logPath) {
		if call.Method == codexMethodTurnStart {
			input = call.Params.Input
		}
	}
	if len(input) != 2 {
		t.Fatalf("timeout never reached native image turn: %+v", input)
	}
	if _, err := os.Lstat(input[1].Path); !os.IsNotExist(err) {
		t.Fatal("projection survived timed-out child")
	}
	items := c.taskTracker.TodoList().Items()
	for _, item := range items {
		if item.Status != TaskDone {
			t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
		}
	}
	if len(items) != 1 || items[0].Status == TaskDone {
		t.Fatal("timeout accepted as success")
	}
}

func TestCodexNativeImagesRetainEffectiveWorldChecks(t *testing.T) {
	for _, kind := range []string{"cwd", "model", "sandbox"} {
		t.Run(kind, func(t *testing.T) {
			c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep {
				cwd, model, sandbox := root, "gpt-5-codex", codexSandboxReadOnly
				switch kind {
				case "cwd":
					cwd = root + "-different"
				case "model":
					model = "different"
				case "sandbox":
					sandbox = codexSandboxWorkspaceWrite
				}
				return []fakeCodexStep{codexInitializeStep(t), fakeCodexThreadStartStep("thread-1", cwd, model, sandbox, t), fakeCodexTurnCompletedStep(t, "turn-1", validProposalJSON(""))}
			})
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _ = c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Inspect image.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
			started := false
			for _, call := range codexImageWireCalls(t, logPath) {
				if call.Method == codexMethodThreadStart {
					started = true
				}
				if call.Method == codexMethodTurnStart {
					t.Fatal("effective world mismatch reached image turn")
				}
			}
			if !started {
				t.Fatal("fixture did not exercise effective world check")
			}
			items := c.taskTracker.TodoList().Items()
			for _, item := range items {
				if item.Status != TaskDone {
					t.Logf("task status=%s detail=%s failure=%+v", item.Status, item.Detail, item.FailureEvent)
				}
			}
			if len(items) != 1 || items[0].Status == TaskDone {
				t.Fatal("world mismatch accepted")
			}
		})
	}
}

func TestCodexNativeImagesRequireExplicitCapacity(t *testing.T) {
	for _, scenario := range []string{"unknown", "too small"} {
		t.Run(scenario, func(t *testing.T) {
			c, logPath := newCodexImageCoordinator(t, codexImageProtocolVersion, func(root string) []fakeCodexStep { return codexImageSteps(t, root, validProposalJSON(""), nil) }, func(session *TeamSession) {
				window := 0
				if scenario == "too small" {
					window = 1024
				}
				session.Config.Generation.ContextWindow = window
				session.Agents["worker"].Generation.ContextWindow = window
			})
			spec := GlobalModelSpecRegistry().GetSpec("codex/gpt-5-codex")
			spec.ModelID = "codex/gpt-5-codex"
			spec.ContextWindowSource = "fallback"
			GlobalModelSpecRegistry().RegisterSpec(spec)
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			_, runErr := c.RunTaskWithMedia(t.Context(), TaskDef{Agent: "worker", Goal: "Inspect image.", Execution: ExecutionContract{RequiresResult: true}}, MediaTurnRequest{Images: []string{path}})
			items := c.taskTracker.TodoList().Items()
			if len(items) != 1 || items[0].Status == TaskDone || len(codexImageWireCalls(t, logPath)) != 0 {
				t.Fatalf("capacity rejection failed: items=%+v run=%v", items, runErr)
			}
			expected := FailurePolicy
			if scenario == "too small" {
				expected = FailureContract
			}
			if items[0].FailureEvent == nil || items[0].FailureEvent.FailureClass != expected {
				t.Fatalf("wrong capacity failure: %+v detail=%s", items[0].FailureEvent, items[0].Detail)
			}
		})
	}
}
