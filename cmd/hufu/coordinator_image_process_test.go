package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCoordinatorImageCLISelectsNativeWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: mise run test-integration")
	}
	binary := buildTestBinary(t)
	for _, facade := range []string{"canonical", "legacy"} {
		t.Run(facade, func(t *testing.T) {
			requests := make(chan nativeCLIRequest, 4)
			var rounds atomic.Int32
			server := nativeCLIProviderWithResponder(t, requests, func(w http.ResponseWriter, request nativeCLIRequest) {
				tool := "submit_result"
				args := map[string]any{"status": "success", "summary": "CLI worker comparison.", "details": "Canonical CLI image observations."}
				if request.Model == "text" {
					if rounds.Add(1) == 1 {
						turnID := coordinatorCLITurnID(t, request)
						tool = "agent"
						args = map[string]any{"tasks": []any{map[string]any{"agent": "worker", "goal": "Compare selected images.", "image_inputs": []any{
							map[string]any{"turn_id": turnID, "part_index": 2}, map[string]any{"turn_id": turnID, "part_index": 1}, map[string]any{"turn_id": turnID, "part_index": 2}, map[string]any{"turn_id": turnID, "part_index": 3},
						}}}}
					} else {
						tool, args = "finish", map[string]any{"response": "CLI worker comparison."}
					}
				}
				writeCoordinatorCLIToolStream(t, w, request.Model, tool, args)
			})
			fixture := newNativeCLIProcessFixture(t, binary, server.URL, "true")
			stdout, runErr, diagnostic := fixture.run(t, fixture.args(facade, "Compare the images."))
			var output jsonRunOutput
			if err := json.Unmarshal(stdout, &output); err != nil || runErr != nil || !output.GoalSatisfied || !strings.Contains(output.Result, "CLI worker comparison.") || len(requests) != 3 {
				t.Fatalf("coordinator CLI: decode=%v run=%v calls=%d output=%s\n%s", err, runErr, len(requests), stdout, diagnostic)
			}
			for index := range 3 {
				request := <-requests
				if index == 1 {
					assertNativeCLIImages(t, request, [][]byte{fixture.images[1], fixture.images[0], fixture.images[1], fixture.images[0]})
					continue
				}
				wire, err := json.Marshal(request.Messages)
				if err != nil || request.Model != "text" || bytes.Contains(wire, []byte("image_url")) || bytes.Contains(wire, []byte("base64,")) || strings.Count(string(wire), "Image input metadata") != 3 {
					t.Fatal("CLI coordinator lost metadata or received image bytes", err)
				}
			}
			assertNativeCLIDurableEvidence(t, fixture.workspace, fixture.paths)
		})
	}
}

func coordinatorCLITurnID(t *testing.T, request nativeCLIRequest) string {
	t.Helper()
	const prefix = "Image input metadata (image content is not provided): "
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if text, ok := strings.CutPrefix(part.Text, prefix); ok {
				var metadata struct {
					TurnID string `json:"turn_id"`
				}
				if json.Unmarshal([]byte(text), &metadata) == nil && metadata.TurnID != "" {
					return metadata.TurnID
				}
			}
		}
	}
	t.Error("coordinator received no selectable image turn")
	return "missing-turn"
}

func writeCoordinatorCLIToolStream(t *testing.T, w http.ResponseWriter, model, tool string, args map[string]any) {
	t.Helper()
	arguments, err := json.Marshal(args)
	if err != nil {
		t.Error(err)
		return
	}
	fmt.Fprintf(w, "data: {\"id\":\"cli-coordinator\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%q}}]},\"finish_reason\":null}]}\n\n", model, "cli-"+tool, tool, arguments)
	fmt.Fprintf(w, "data: {\"id\":\"cli-coordinator\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", model)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}
