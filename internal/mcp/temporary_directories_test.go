package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// A real stdio child proves environment delivery, JSON-RPC framing, internal
// file writes, and shutdown-before-cleanup without npm, browsers or providers.
func TestTemporaryDirectoryServerHelper(t *testing.T) {
	if os.Getenv("HUFU_MCP_TEMP_HELPER") != "1" {
		return
	}
	output := os.Getenv("HUFU_TEST_OUTPUT")
	if err := os.WriteFile(filepath.Join(output, "server.log"), []byte("private server output"), 0o600); err != nil {
		os.Exit(21)
	}
	if err := os.WriteFile(os.Getenv("HUFU_TEST_STARTED"), []byte(output), 0o600); err != nil {
		os.Exit(22)
	}
	if os.Getenv("HUFU_MCP_TEMP_EARLY_EXIT") == "1" {
		os.Exit(31)
	}
	if os.Getenv("HUFU_MCP_TEMP_BLOCK_INIT") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
	} else if os.Getenv("HUFU_MCP_TEMP_BAD_INIT") == "1" {
		fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"fixture initialization rejected"}}`)
		_, _ = io.Copy(io.Discard, os.Stdin)
	} else {
		s := server.NewMCPServer("temporary-output-fixture", "1.0", server.WithToolCapabilities(false))
		s.AddTool(mcpgo.NewTool("probe"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText(output), nil
		})
		if err := server.NewStdioServer(s).Listen(context.Background(), os.Stdin, os.Stdout); err != nil {
			os.Exit(23)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "server.log")); err != nil {
		os.Exit(24)
	}
	if err := os.WriteFile(os.Getenv("HUFU_TEST_STOPPED"), []byte(output), 0o600); err != nil {
		os.Exit(25)
	}
	os.Exit(0)
}

func temporaryServerFixture(t *testing.T) (MCPServerConfig, string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probe := t.TempDir()
	started, stopped := filepath.Join(probe, "started"), filepath.Join(probe, "stopped")
	return MCPServerConfig{
		Type: "local", Command: []string{executable, "-test.run=^TestTemporaryDirectoryServerHelper$"},
		Environment:           map[string]string{"HUFU_MCP_TEMP_HELPER": "1", "HUFU_TEST_STARTED": started, "HUFU_TEST_STOPPED": stopped},
		TemporaryDirectoryEnv: []string{"HUFU_TEST_OUTPUT"},
	}, started, stopped
}

func TestManagedMCPTemporaryDirectoriesArePrivateAndCleanedAfterShutdown(t *testing.T) {
	// A parent value is overridden only for the child and must never be deleted.
	parentOutput := t.TempDir()
	t.Setenv("HUFU_TEST_OUTPUT", parentOutput)
	managers := []*MCPToolManager{NewMCPToolManager("", ""), NewMCPToolManager("", "")}
	var outputs, stops []string
	for _, manager := range managers {
		t.Cleanup(func() { _ = manager.Close() })
		cfg, start, stop := temporaryServerFixture(t)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		err := manager.LoadTools(ctx, map[string]MCPServerConfig{"fixture": cfg})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(start)
		if err != nil {
			t.Fatal(err)
		}
		output := string(data)
		if output == parentOutput || output == "" {
			t.Fatal("child inherited shared output")
		}
		info, err := os.Stat(output)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("private directory: %v, %v", info, err)
		}
		if len(cfg.Environment) != 3 || cfg.Environment["HUFU_TEST_OUTPUT"] != "" {
			t.Fatal("runtime paths mutated configuration")
		}
		outputs, stops = append(outputs, output), append(stops, stop)
	}
	if outputs[0] == outputs[1] {
		t.Fatal("independent managers share output")
	}
	for i, manager := range managers {
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(stops[i]); err != nil {
			t.Fatalf("server did not confirm output existed during shutdown: %v", err)
		}
		if _, err := os.Stat(filepath.Dir(outputs[i])); !os.IsNotExist(err) {
			t.Fatalf("temporary root retained: %v", err)
		}
		if i == 0 {
			if _, err := os.Stat(outputs[1]); err != nil {
				t.Fatal("closing one manager removed another's output")
			}
		}
	}
	if _, err := os.Stat(parentOutput); err != nil {
		t.Fatal("parent output was removed")
	}
}

func TestManagedMCPTemporaryDirectoriesCleanFailedInitializationAndDuplicate(t *testing.T) {
	for _, mode := range []string{"initialization", "early_exit", "duplicate", "missing_command"} {
		t.Run(mode, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("TMPDIR", base)
			manager := NewMCPToolManager("", "")
			t.Cleanup(func() { _ = manager.Close() })
			cfg, start, stop := temporaryServerFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if mode == "missing_command" {
				cfg.Command = []string{filepath.Join(base, "missing-executable")}
			} else if mode == "initialization" {
				cfg.Environment["HUFU_MCP_TEMP_BAD_INIT"] = "1"
			} else if mode == "early_exit" {
				cfg.Environment["HUFU_MCP_TEMP_EARLY_EXIT"] = "1"
			} else {
				if err := manager.LoadTools(ctx, map[string]MCPServerConfig{"fixture": cfg}); err != nil {
					t.Fatal(err)
				}
			}
			err := manager.LoadTools(ctx, map[string]MCPServerConfig{"fixture": cfg})
			if mode != "duplicate" && err == nil {
				t.Fatal("invalid server was accepted")
			}
			if mode != "missing_command" {
				data, err := os.ReadFile(start)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(stop); err != nil && mode != "early_exit" {
					t.Fatal("failed/rejected server was not stopped")
				}
				if _, err := os.Stat(filepath.Dir(string(data))); !os.IsNotExist(err) {
					t.Fatal("failed/rejected server retained output")
				}
			}
			if err := manager.Close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary resources retained: %v, %v", entries, err)
			}
		})
	}
}

func TestMCPTemporaryDirectoryDeclarationValidationAndJSON(t *testing.T) {
	for _, cfg := range []MCPServerConfig{
		{Type: "remote", TemporaryDirectoryEnv: []string{"OUTPUT"}},
		{TemporaryDirectoryEnv: []string{"../OUTPUT"}},
		{TemporaryDirectoryEnv: []string{"1OUTPUT"}},
		{TemporaryDirectoryEnv: []string{"LD_PRELOAD"}},
		{TemporaryDirectoryEnv: []string{"OUTPUT", "OUTPUT"}},
		{TemporaryDirectoryEnv: []string{"OUTPUT"}, Environment: map[string]string{"OUTPUT": "/shared"}},
	} {
		if err := ValidateWorkerToolPolicies(cfg); err == nil {
			t.Fatalf("invalid temporary declaration accepted: %+v", cfg)
		}
	}
	var decoded MCPServerConfig
	if err := json.Unmarshal([]byte(`{"command":["fixture"],"temporary-directory-env":["OUTPUT"]}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Type != "local" || len(decoded.TemporaryDirectoryEnv) != 1 || decoded.TemporaryDirectoryEnv[0] != "OUTPUT" {
		t.Fatalf("declaration lost in JSON: %+v", decoded)
	}
}

type temporaryCloseErrorTransport struct {
	transport.Interface
	calls int
}

func (tr *temporaryCloseErrorTransport) Close() error {
	tr.calls++
	if tr.calls == 1 {
		return errors.New("shutdown unconfirmed")
	}
	return nil
}

func TestMCPUnconfirmedShutdownRetainsOnlyItsOwnedDirectory(t *testing.T) {
	manager := NewMCPToolManager("", "")
	root := t.TempDir()
	tr := &temporaryCloseErrorTransport{}
	cli := client.NewClient(tr)
	manager.clients["fixture"] = cli
	manager.localServers[cli] = &localServerResources{root: root}
	if err := manager.Close(); err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("unconfirmed shutdown did not report retained output: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("unconfirmed consumer's output was deleted")
	}
	if err := manager.Close(); err == nil {
		t.Fatal("repeated close erased unconfirmed shutdown")
	}
	if tr.calls != 1 {
		t.Fatalf("transport close repeated %d times", tr.calls)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("repeated close deleted unconfirmed consumer output")
	}
}

func TestManagedMCPTemporaryDirectoryCloseCancelsAndDrainsInitialization(t *testing.T) {
	manager := NewMCPToolManager("", "")
	cfg, start, stop := temporaryServerFixture(t)
	cfg.Environment["HUFU_MCP_TEMP_BLOCK_INIT"] = "1"
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	finished := make(chan error, 1)
	go func() { finished <- manager.LoadTools(ctx, map[string]MCPServerConfig{"fixture": cfg}) }()
	drained := false
	t.Cleanup(func() {
		cancel()
		_ = manager.Close()
		if !drained {
			select {
			case <-finished:
			case <-time.After(30 * time.Second):
				t.Error("initialization did not drain")
			}
		}
	})
	ready := time.NewTicker(10 * time.Millisecond)
	defer ready.Stop()
	for {
		if _, err := os.Stat(start); err == nil {
			break
		}
		select {
		case <-ready.C:
		case <-ctx.Done():
			t.Fatal("stdio fixture did not start")
		}
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		drained = true
		if err == nil {
			t.Fatal("cancelled initialization succeeded")
		}
	case <-ctx.Done():
		t.Fatal("Close returned without draining initialization")
	}
	data, err := os.ReadFile(start)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stop); err != nil {
		t.Fatal("server was not stopped before cleanup")
	}
	if _, err := os.Stat(filepath.Dir(string(data))); !os.IsNotExist(err) {
		t.Fatal("initializing server retained temporary output")
	}
}
