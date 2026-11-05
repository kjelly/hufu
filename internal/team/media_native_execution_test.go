package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

type mediaWorkerWireMessage struct {
	Role    string         `json:"role"`
	Content jsontext.Value `json:"content"`
}

type mediaWorkerWirePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func TestNativeMediaPublicInvocationReachesSDKWithCanonicalImages(t *testing.T) {
	for _, scenario := range []struct {
		mode      media.InvocationMode
		transport agent.ResultTransport
	}{
		{media.InvocationSingleTask, agent.ResultTransportTool},
		{media.InvocationDirectAgent, agent.ResultTransportTool},
		{media.InvocationSingleTask, agent.ResultTransportRuntime},
		{media.InvocationDirectAgent, agent.ResultTransportRuntime},
	} {
		t.Run(string(scenario.mode)+"/"+string(scenario.transport), func(t *testing.T) {
			requests := make(chan []mediaWorkerWireMessage, 4)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				number := calls.Add(1)
				var request struct {
					Model    string                   `json:"model"`
					Messages []mediaWorkerWireMessage `json:"messages"`
					Stream   bool                     `json:"stream"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				select {
				case requests <- request.Messages:
				default:
					http.Error(w, "unexpected extra invocation", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if scenario.transport == agent.ResultTransportRuntime {
					text := "Images inspected by the offline fixture. Fixture comparison result."
					if number > 1 {
						text = `{"status":"success","summary":"Images inspected by the offline fixture.","details":"Fixture comparison result."}`
					}
					if !request.Stream {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprintf(w, `{"id":"media-collector","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, request.Model, text)
					} else {
						fmt.Fprintf(w, "data: {\"id\":\"media-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\ndata: {\"id\":\"media-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", request.Model, text, request.Model)
					}
					return
				}
				arguments := `{"status":"success","summary":"Images inspected by the offline fixture.","details":"Fixture comparison result."}`
				fmt.Fprintf(w, "data: {\"id\":\"media-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"submit-media\",\"type\":\"function\",\"function\":{\"name\":\"submit_result\",\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", request.Model, arguments)
				_, _ = io.WriteString(w, "data: {\"id\":\"media-worker\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"vision\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}))
			t.Cleanup(server.Close)
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				return providerproxy.StartInProcess(ctx, cfg)
			}
			c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
				session.Agents["worker"].Tools = "submit_result"
				session.Agents["worker"].ResultTransport = scenario.transport
				session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
			})
			path := filepath.Join(c.projectDir, "input.png")
			data := mediaStorePNG(t)
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var second bytes.Buffer
			if err := png.Encode(&second, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
				t.Fatal(err)
			}
			secondPath := filepath.Join(c.projectDir, "second.png")
			if err := os.WriteFile(secondPath, second.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			input := MediaTurnRequest{Images: []string{path, secondPath, path}}
			var runErr error
			if scenario.mode == media.InvocationSingleTask {
				_, runErr = c.RunTaskWithMedia(ctx, TaskDef{Agent: "worker", Goal: "Compare the images.", Execution: ExecutionContract{RequiresResult: true}}, input)
			} else {
				_, runErr = c.RunDirectAgentWithMedia(ctx, "worker", "Compare the images.", input)
			}
			items := c.TaskTracker().TodoList().Items()
			wantCalls := int32(1)
			if scenario.transport == agent.ResultTransportRuntime {
				wantCalls = 2
			}
			if calls.Load() != wantCalls || len(items) != 1 || items[0].Status != TaskDone || c.GetTaskResult(items[0].ID) == nil {
				for _, item := range items {
					t.Logf("task status=%s detail=%s", item.Status, item.Detail)
				}
				t.Fatalf("native worker did not finish once: calls=%d tasks=%+v err=%v", calls.Load(), items, runErr)
			}
			// Completion of the worker does not invent whole-run acceptance.
			if result := c.LastRunResult(); scenario.mode == media.InvocationSingleTask && runErr == nil || result == nil || result.Outcome != RunOutcomeUnverified || result.StopReason != StopReasonAcceptanceNotSet {
				t.Fatalf("native worker changed acceptance semantics: result=%+v err=%v", result, runErr)
			}
			assertMediaWorkerWire(t, <-requests, items[0].MediaBinding)
			if scenario.transport == agent.ResultTransportRuntime {
				collector, err := json.Marshal(<-requests)
				if err != nil || bytes.Contains(collector, []byte("image_url")) || bytes.Contains(collector, []byte("media-v1-")) || bytes.Contains(collector, []byte("base64,")) {
					t.Fatal("result collection inherited image content or references", err)
				}
			}
			if len(items[0].ExecutionReceipts) != 1 || items[0].MediaBinding.InvocationMode != scenario.mode || items[0].ExecutionReceipt.MediaInputDigest != items[0].MediaInputDigest {
				t.Fatal("native worker lost its execution receipt or invocation mode")
			}
			events, err := c.EventJournal().ReadEvents(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
				t.Fatal(err)
			}
			admissions := 0
			for _, event := range events {
				if event.Type == string(EventMediaProviderAdmitted) {
					admissions++
					payload, err := decodeMediaProviderEvent(event)
					if err != nil || payload.ImageCount != 3 || payload.ImageTokens != 3*4096 {
						t.Fatal("native admission lost repeated image accounting", err)
					}
				}
			}
			if admissions != 1 || c.DualWriteFailures() != 0 {
				t.Fatal("native worker lacks exact durable permission or persistence integrity")
			}
			if scenario.transport == agent.ResultTransportRuntime {
				for index, event := range events {
					if event.Type != string(EventTaskResultCollectionCheckpointed) {
						continue
					}
					var payload resultCollectionEvent
					if err := json.Unmarshal(event.Payload, &payload, collectionDurationFormat); err != nil {
						t.Fatal(err)
					}
					payload.Receipt.MediaInputDigest = "forged"
					changed := slices.Clone(events)
					changed[index].Payload, err = json.Marshal(payload, collectionDurationFormat)
					if err != nil {
						t.Fatal(err)
					}
					requireMediaCode(t, VerifyNativeMediaLineage(t.Context(), c.session.Workspace, changed), media.BindingConflict)
					break
				}
			}
			assertNativeCompilerCreditAvailable(t)
		})
	}
}

func TestNativeMediaPublicInvocationRejectsBeforeHTTP(t *testing.T) {
	for _, variant := range []string{"unknown capability", "denied capability", "insufficient context", "corrupt CAS"} {
		t.Run(variant, func(t *testing.T) {
			var calls, boundaries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "unexpected transport", http.StatusBadRequest)
			}))
			t.Cleanup(server.Close)
			var c *Coordinator
			factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
				boundaries.Add(1)
				if variant == "corrupt CAS" {
					item := c.TaskTracker().TodoList().Items()[0]
					store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
					if err != nil {
						return nil, err
					}
					var changed bytes.Buffer
					if err := png.Encode(&changed, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
						return nil, err
					}
					if err := os.WriteFile(store.mediaPath("data", item.MediaBinding.Refs[0].ID), changed.Bytes(), 0o600); err != nil {
						return nil, err
					}
				}
				return providerproxy.StartInProcess(ctx, cfg)
			}
			capability := providerintrospection.CapabilityYes
			switch variant {
			case "unknown capability":
				capability = providerintrospection.CapabilityUnknown
			case "denied capability":
				capability = providerintrospection.CapabilityNo
			}
			c = newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, capability, func(session *TeamSession) {
				session.Agents["worker"].Tools = "submit_result"
				session.Agents["worker"].ResultTransport = agent.ResultTransportTool
				if variant == "insufficient context" {
					session.Agents["worker"].Generation.ContextWindow = 8192
				}
			})
			path := filepath.Join(c.projectDir, "input.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := c.RunDirectAgentWithMedia(t.Context(), "worker", "Compare the images.", MediaTurnRequest{Images: []string{path, path}})
			if err == nil || calls.Load() != 0 || c.DualWriteFailures() != 0 {
				t.Fatalf("invalid native input reached HTTP or lost failure evidence: calls=%d err=%v", calls.Load(), err)
			}
			if variant != "corrupt CAS" && boundaries.Load() != 0 {
				t.Fatal("metadata rejection started a provider boundary")
			}
			if variant == "unknown capability" || variant == "denied capability" {
				requireMediaCode(t, err, media.Unsupported)
			} else if variant == "insufficient context" {
				if _, ok := errors.AsType[*CannotFitError](err); !ok {
					t.Fatal("capacity failure lost its structured error", err)
				}
			} else {
				if typed, ok := errors.AsType[*media.Error](err); !ok || typed.Code != media.IntegrityMismatch && typed.Code != media.InputMissing {
					t.Fatal("corrupt CAS failure lost its media category", err)
				}
			}
			item := c.TaskTracker().TodoList().Items()[0]
			if item.Status == TaskDone || !isTerminalTaskStatus(item.Status) {
				t.Fatal("rejected native invocation lacks a terminal failure")
			}
			assertNativeCompilerCreditAvailable(t)
		})
	}
}

func assertMediaWorkerWire(t *testing.T, messages []mediaWorkerWireMessage, binding *media.TaskBinding) {
	t.Helper()
	var hashes []string
	imageTurns, matchingText := 0, 0
	for _, message := range messages {
		raw := bytes.TrimSpace(message.Content)
		var parts []mediaWorkerWirePart
		if len(raw) > 0 && raw[0] == '"' {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				t.Fatal(err)
			}
			parts = []mediaWorkerWirePart{{Type: "text", Text: text}}
		} else if err := json.Unmarshal(raw, &parts); err != nil {
			t.Fatal(err)
		}
		turnImages := 0
		for _, part := range parts {
			if message.Role == "user" && strings.Contains(part.Text, "Compare the images.") {
				matchingText++
			}
			if part.Type != "image_url" {
				continue
			}
			if message.Role != "user" || len(hashes) >= len(binding.Refs) {
				t.Fatal("native wire changed image role or occurrence count")
			}
			ref := binding.Refs[len(hashes)]
			encoded, ok := strings.CutPrefix(part.ImageURL.URL, "data:"+ref.MediaType+";base64,")
			data, err := base64.StdEncoding.DecodeString(encoded)
			if !ok || err != nil {
				t.Fatal("native wire lost its admitted image MIME or encoding", err)
			}
			digest := sha256.Sum256(data)
			hashes = append(hashes, hex.EncodeToString(digest[:]))
			if hashes[len(hashes)-1] != ref.SHA256 {
				t.Fatal("native wire differs from the canonical image bytes/order")
			}
			turnImages++
		}
		if turnImages > 0 {
			imageTurns++
		}
	}
	if len(hashes) != len(binding.Refs) || imageTurns != 1 || matchingText != 1 {
		t.Fatalf("native worker duplicated its input or lost images: images=%d turns=%d text=%d", len(hashes), imageTurns, matchingText)
	}
}
