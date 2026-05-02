package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
)

const defaultDownloadTimeout = 300 * time.Second
const maxDownloadTimeout = 600 * time.Second

type downloadArgs struct {
	URL      string `json:"url"`
	FilePath string `json:"file_path"`
	Timeout  int    `json:"timeout,omitempty"`
}

func NewDownloadTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "download"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "download",
			Description: "Download a file from a URL and save it to a local path. Supports HTTP and HTTPS URLs. Creates parent directories if needed.",
			Parameters: map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "The URL to download from",
				},
				"file_path": map[string]any{
					"type":        "string",
					"description": "The local file path where the downloaded content should be saved",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 300s, max 600s)",
				},
			},
			Required: []string{"url", "file_path"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeDownload(ctx, call, cfg)
		},
	}
}

func executeDownload(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args downloadArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("url and file_path parameters are required"), nil
	}
	if args.URL == "" {
		return fantasy.NewTextErrorResponse("url parameter is required"), nil
	}
	if args.FilePath == "" {
		return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
	}
	if !strings.HasPrefix(args.URL, "http://") && !strings.HasPrefix(args.URL, "https://") {
		return fantasy.NewTextErrorResponse("url must start with http:// or https://"), nil
	}

	absPath, err := resolveAndValidatePathWithConsent(args.FilePath, cfg)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid file_path: %v", err)), nil
	}

	timeout := defaultDownloadTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxDownloadTimeout {
			timeout = maxDownloadTimeout
		}
	}

	dlCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, "GET", args.URL, nil)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create request: %v", err)), nil
	}
	req.Header.Set("User-Agent", "hufu/1.0")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to download: %v", err)), nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("download failed: HTTP %d", resp.StatusCode)), nil
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create directories: %v", err)), nil
	}

	f, err := os.Create(absPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create file: %v", err)), nil
	}
	defer f.Close()

	written, err := io.Copy(f, resp.Body)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write file: %v", err)), nil
	}

	contentType := resp.Header.Get("Content-Type")
	return fantasy.NewTextResponse(fmt.Sprintf("Successfully downloaded %d bytes to %s (Content-Type: %s)", written, args.FilePath, contentType)), nil
}
