//go:build linux

package main

import (
	"bytes"
	"encoding/json/v2"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func nativeImageChatArgs(f nativeCLIProcessFixture, endpoint, ui string) []string {
	return []string{"chat", "--ui", ui, "--agent-team", "native-cli", "--agent-team-search-path", f.search,
		"--workspace", f.workspace, "--provider-url", endpoint + "/v1", "--context-window", "32768", "--no-spinner", "--no-color"}
}

func waitChatPlainPrompt(t *testing.T, process *ptyProcess, offset int) {
	t.Helper()
	process.waitForPTYOutput(t, "\x1b[6n", offset, true, 15*time.Second)
	process.writeInput(t, "\x1b[1;1R")
	process.waitForOutputAfter(t, "hufu> ", offset, 15*time.Second)
}

func chatImageWorkspace(t *testing.T, root string) string {
	t.Helper()
	var workspaces []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() == "runtime.sqlite" {
			workspaces = append(workspaces, filepath.Dir(path))
		}
		return err
	})
	if err != nil || len(workspaces) != 1 {
		t.Fatal("expected one canonical chat workspace", workspaces, err)
	}
	return workspaces[0]
}

func TestE2EChatImageCrashRestoresOnlyCommittedInput(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	binary := buildTestBinary(t)
	for _, committed := range []bool{false, true} {
		t.Run(strconv.FormatBool(committed), func(t *testing.T) {
			requests := make(chan nativeCLIRequest, 8)
			release := make(chan struct{})
			var rounds atomic.Int32
			server := nativeCLIProviderWithResponder(t, requests, func(w http.ResponseWriter, request nativeCLIRequest) {
				if request.Model == "vision" {
					writeCoordinatorCLIToolStream(t, w, request.Model, "submit_result", map[string]any{"status": "success", "summary": "Recovered image observations.", "details": "Original canonical bytes."})
					return
				}
				round := rounds.Add(1)
				if committed && round == 1 {
					<-release
					return
				}
				if committed && round == 2 {
					writeCoordinatorCLIToolStream(t, w, request.Model, "agent", map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "Inspect recovered input.", "image_inputs": []any{map[string]any{"turn_id": coordinatorCLITurnID(t, request), "part_index": 1}}}}})
					return
				}
				writeCoordinatorCLIToolStream(t, w, request.Model, "finish", map[string]any{"response": "CHAT_CRASH_RECOVERED"})
			})
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
			})
			fixture := newNativeCLIProcessFixtureAtRoot(t, binary, server.URL, "true", ptyTestDir(t))
			start := func() *ptyProcess {
				process := startPTYInDir(t, binary, nativeImageChatArgs(fixture, server.URL, "plain"), 30, 100, fixture.home, fixture.project)
				process.waitForOutput(t, "Pending images start empty", 15*time.Second)
				waitChatPlainPrompt(t, process, 0)
				return process
			}
			process := start()
			offset := len(process.output.Bytes())
			process.writeInput(t, "/image add "+fixture.paths[0]+"\r")
			process.waitForOutput(t, strconv.Quote(filepath.Base(fixture.paths[0])), 15*time.Second)
			waitChatPlainPrompt(t, process, offset)
			if committed {
				process.writeInput(t, "Inspect before crash.\r")
				select {
				case request := <-requests:
					if request.Model != "text" || coordinatorCLITurnID(t, request) == "" {
						t.Fatal("first request did not carry canonical image metadata")
					}
				case <-time.After(15 * time.Second):
					t.Fatal("canonical input never reached the fake coordinator")
				}
			}
			if err := process.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			process.waitExit(t, 15*time.Second)
			close(release)
			if err := os.Remove(fixture.paths[0]); err != nil {
				t.Fatal(err)
			}
			workspace := chatImageWorkspace(t, fixture.workspace)
			before := team.LoadSession(workspace)
			wantSnapshots := 0
			if committed {
				wantSnapshots = 1
			}
			if before == nil || len(before.MediaInputSnapshots) != wantSnapshots {
				t.Fatal("crash checkpoint did not distinguish staging from committed input")
			}
			restarted := start()
			offset = len(restarted.output.Bytes())
			restarted.writeInput(t, "/image list\r")
			restarted.waitForOutput(t, "No pending images.", 15*time.Second)
			waitChatPlainPrompt(t, restarted, offset)
			offset = len(restarted.output.Bytes())
			restarted.writeInput(t, "Continue after crash.\r")
			restarted.waitForOutput(t, "CHAT_CRASH_RECOVERED", 20*time.Second)
			waitChatPlainPrompt(t, restarted, offset)
			restarted.writeInput(t, "/exit\r")
			if code := restarted.waitExit(t, 15*time.Second); code != 0 {
				t.Fatal("restarted chat did not exit cleanly", code)
			}
			after := team.LoadSession(workspace)
			if after == nil || len(after.MediaInputSnapshots) != wantSnapshots {
				t.Fatal("restart created an image input turn from process-local staging")
			}
			if committed {
				if before.MediaInputSnapshots[0].ID != after.MediaInputSnapshots[0].ID || len(requests) != 3 {
					t.Fatal("recovery replaced or resubmitted the original image snapshot")
				}
				<-requests
				assertNativeCLIImages(t, <-requests, [][]byte{fixture.images[0]})
			} else if len(requests) != 1 {
				t.Fatal("uncommitted staging survived process death")
			}
		})
	}
}

func TestE2EChatImagesUseStagedBytesWithoutRecordingSourcePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	binary := buildTestBinary(t)
	for _, ui := range []string{"plain", "chat"} {
		t.Run(ui, func(t *testing.T) {
			requests := make(chan nativeCLIRequest, 4)
			var rounds atomic.Int32
			server := nativeCLIProviderWithResponder(t, requests, func(w http.ResponseWriter, request nativeCLIRequest) {
				tool := "submit_result"
				args := map[string]any{"status": "success", "summary": "Native image observations.", "details": "Original staged image bytes."}
				if request.Model == "text" {
					if rounds.Add(1) == 1 {
						tool = "agent"
						args = map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "Inspect the staged image.", "image_inputs": []any{map[string]any{"turn_id": coordinatorCLITurnID(t, request), "part_index": 1}}}}}
					} else {
						tool, args = "finish", map[string]any{"response": "CHAT_IMAGE_COMPLETE"}
					}
				}
				writeCoordinatorCLIToolStream(t, w, request.Model, tool, args)
			})
			fixture := newNativeCLIProcessFixtureAtRoot(t, binary, server.URL, "true", ptyTestDir(t))
			process := startPTYInDir(t, binary, nativeImageChatArgs(fixture, server.URL, ui), 30, 100, fixture.home, fixture.project)
			ready := "Pending images start empty"
			process.waitForOutput(t, ready, 15*time.Second)
			waitPlainPrompt := func(offset int) {
				if ui != "plain" {
					return
				}
				process.waitForPTYOutput(t, "\x1b[6n", offset, true, 15*time.Second)
				process.writeInput(t, "\x1b[1;1R")
				process.waitForOutputAfter(t, "hufu> ", offset, 15*time.Second)
			}
			waitPlainPrompt(0)
			addOffset := len(process.output.Bytes())
			process.writeInput(t, "/image add "+fixture.paths[0]+"\r")
			if ui == "chat" {
				process.waitForOutput(t, "Pending images: 1", 15*time.Second)
				process.writeInput(t, "/image list\r")
			}
			process.waitForOutput(t, strconv.Quote(filepath.Base(fixture.paths[0])), 15*time.Second)
			if len(requests) != 0 {
				t.Fatal("image staging called a model before a user message")
			}
			if ui == "chat" {
				offset := len(process.output.Bytes())
				process.writeInput(t, "\r")
				process.waitForOutputAfter(t, "Message the team", offset, 15*time.Second)
			}
			waitPlainPrompt(addOffset)
			if err := os.Remove(fixture.paths[0]); err != nil {
				t.Fatal(err)
			}
			turnOffset := len(process.output.Bytes())
			process.writeInput(t, "Inspect the staged image.\r")
			process.waitForOutput(t, "CHAT_IMAGE_COMPLETE", 20*time.Second)
			waitPlainPrompt(turnOffset)
			if ui == "chat" {
				process.waitForOutputAfter(t, "Coordinator · completed", turnOffset, 15*time.Second)
			}
			process.writeInput(t, "/exit\r")
			if code := process.waitExit(t, 15*time.Second); code != 0 {
				t.Fatal("image chat exit failed", code)
			}
			if len(requests) != 3 {
				t.Fatal("chat repeated image work or lost a provider turn", len(requests))
			}
			for index := range 3 {
				request := <-requests
				if index == 1 {
					assertNativeCLIImages(t, request, [][]byte{fixture.images[0]})
					continue
				}
				wire, err := json.Marshal(request.Messages)
				if err != nil || request.Model != "text" || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64,")) || bytes.Contains(wire, []byte(fixture.paths[0])) {
					t.Fatal("coordinator received pixels or an original file path", err)
				}
			}
			assertNativeCLIDurableEvidence(t, fixture.workspace, fixture.paths)
			history, err := os.ReadFile(filepath.Join(fixture.home, ".hufu", "prompt_history"))
			if ui == "plain" && (err != nil || strings.Contains(string(history), fixture.paths[0]) || strings.Contains(string(history), "/image add") || !strings.Contains(string(history), "Inspect the staged image.")) {
				t.Fatal("plain chat saved an image control path or lost ordinary history", err)
			}
		})
	}
}
