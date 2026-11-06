package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func TestResultsImagesBinaryExportsOriginalMCPImage(t *testing.T) {
	binary := buildTestBinary(t)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	var rpc atomic.Int32
	server := mcpserver.NewMCPServer("camera", "1", mcpserver.WithToolCapabilities(false))
	server.AddTool(mcpgo.NewTool("capture", mcpgo.WithDescription("Read an observation image.")), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		rpc.Add(1)
		return &mcpgo.CallToolResult{Content: []mcpgo.Content{mcpgo.NewImageContent(base64.StdEncoding.EncodeToString(imageBytes.Bytes()), "image/png")}}, nil
	})
	mcpHTTP := httptest.NewServer(mcpserver.NewStreamableHTTPServer(server))
	t.Cleanup(mcpHTTP.Close)
	requests := make(chan nativeCLIRequest, 8)
	var workerCalls atomic.Int32
	provider := nativeCLIProviderWithResponder(t, requests, func(w http.ResponseWriter, request nativeCLIRequest) {
		if request.Model == "text" {
			writeImageProcessStream(w, request.Model, map[string]any{"role": "assistant", "content": "Published image fixture complete."}, "stop")
			return
		}
		name, args := "submit_result", `{"status":"success","summary":"Captured image.","details":"Published the observed image."}`
		if workerCalls.Add(1) == 1 {
			name, args = "use_dynamic_tool", `{"action":"call","target":"camera__capture","arguments":{}}`
		}
		writeImageProcessStream(w, request.Model, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "image-call", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "tool_calls")
	})
	f := newNativeCLIProcessFixture(t, binary, provider.URL, "true")
	manifest := filepath.Join(f.search, "native-cli", "team.yaml")
	file, err := os.OpenFile(manifest, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(file, "mcp-servers:\n  camera:\n    type: remote\n    url: %s\n    noOAuth: true\n    allowedTools: [capture]\n    toolPolicies:\n      capture:\n        sideEffect: none\n        filesystem: none\n        inputSchema: {type: object, additionalProperties: false}\n", mcpHTTP.URL)
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal("write fixture MCP configuration", err, closeErr)
	}
	worker := "---\nname: worker\nrole: worker\nmodel: ollama/vision\ntools: camera__capture,submit_result\nresult-transport: tool\nmemory: {mode: off}\n---\nCapture an observation image and submit the result.\n"
	if err := os.WriteFile(filepath.Join(f.search, "native-cli", "worker.md"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	output, runErr, diagnostic := f.run(t, f.args("run", "@worker Capture an observation image.", "--unattended"))
	if runErr != nil || rpc.Load() != 1 || workerCalls.Load() != 2 {
		t.Fatalf("image CLI run failed: RPC=%d worker=%d err=%v\n%s\n%s", rpc.Load(), workerCalls.Load(), runErr, output, diagnostic)
	}
	listing, err, diagnostic := f.run(t, []string{"results", "list", "--workspace", f.workspace, "--team", "native-cli", "--output", "json"})
	if err != nil {
		t.Fatal(err, diagnostic)
	}
	var results resultsEnvelope
	if err := json.Unmarshal(listing, &results); err != nil || len(results.Results) != 1 {
		t.Fatalf("result query failed: %v %s", err, listing)
	}
	args := []string{"results", "images", results.Results[0].ResultID, "--workspace", f.workspace, "--team", "native-cli", "--output", "json"}
	metadata, err, diagnostic := f.run(t, args)
	if err != nil {
		t.Fatal(err, diagnostic)
	}
	var images resultImagesEnvelope
	if err := json.Unmarshal(metadata, &images); err != nil || len(images.Images) != 1 {
		t.Fatalf("image metadata query failed: %v %s", err, metadata)
	}
	destination := filepath.Join(f.project, "original.png")
	_, err, diagnostic = f.run(t, append(args, "--artifact", images.Images[0].Image.ID, "--export", destination))
	if err != nil {
		t.Fatal(err, diagnostic)
	}
	exported, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(exported, imageBytes.Bytes()) || rpc.Load() != 1 || workerCalls.Load() != 2 {
		t.Fatal("offline export changed bytes or dispatched another model/tool", err)
	}
}

func writeImageProcessStream(w http.ResponseWriter, model string, delta map[string]any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, choice := range []map[string]any{{"index": 0, "delta": delta, "finish_reason": nil}, {"index": 0, "delta": map[string]any{}, "finish_reason": finish}} {
		data, _ := json.Marshal(map[string]any{"id": "image-process", "object": "chat.completion.chunk", "created": 1, "model": model, "choices": []any{choice}})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}
