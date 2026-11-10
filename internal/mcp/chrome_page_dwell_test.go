package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"go.yaml.in/yaml/v3"

	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestChromePageDwellConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name, kind, value string
		valid             bool
	}{
		{"disabled", "chrome-cdp", "null", true},
		{"range", "chrome-cdp", `{"min-seconds":300,"max-seconds":600}`, true},
		{"fixed", "chrome-cdp", `{"min-seconds":300,"max-seconds":300}`, true},
		{"missing", "chrome-cdp", `{}`, false},
		{"negative", "chrome-cdp", `{"min-seconds":-1,"max-seconds":600}`, false},
		{"reversed", "chrome-cdp", `{"min-seconds":600,"max-seconds":300}`, false},
		{"excessive", "chrome-cdp", `{"min-seconds":300,"max-seconds":3601}`, false},
		{"unknown", "chrome-cdp", `{"min-seconds":300,"max-seconds":600,"skip":true}`, false},
		{"fraction", "chrome-cdp", `{"min-seconds":300.5,"max-seconds":600}`, false},
		{"wrong_backend", "remote", `{"min-seconds":300,"max-seconds":600}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, format := range []string{"json", "yaml"} {
				var cfg MCPServerConfig
				input := `{"type":"` + tt.kind + `","url":"http://127.0.0.1:9222","page-dwell":` + tt.value + `}`
				var err error
				if format == "json" {
					err = json.Unmarshal([]byte(input), &cfg)
				} else {
					err = yaml.Unmarshal([]byte(input), &cfg)
				}
				if err == nil {
					err = ValidateWorkerToolPolicies(cfg)
				}
				if (err == nil) != tt.valid {
					t.Fatalf("%s: %v", format, err)
				}
				if tt.valid {
					encoded, err := json.Marshal(cfg)
					if err != nil {
						t.Fatal(err)
					}
					var restored MCPServerConfig
					if err := json.Unmarshal(encoded, &restored); err != nil {
						t.Fatal(err)
					}
					if tt.name == "disabled" && strings.Contains(string(encoded), "page-dwell") {
						t.Fatal("disabled dwell changed legacy configuration encoding")
					}
					if tt.name != "disabled" && (restored.PageDwell == nil || *restored.PageDwell != *cfg.PageDwell) {
						t.Fatal("dwell lost through JSON round-trip")
					}
				}
			}
		})
	}
}

func TestChromePageDwellRandomRange(t *testing.T) {
	var disabled *ChromePageDwellConfig
	if disabled.duration() != 0 {
		t.Fatal("omitted dwell enabled a delay")
	}
	cfg := &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 600}
	for range 100 {
		duration := cfg.duration()
		if duration < 5*time.Minute || duration > 10*time.Minute || duration%time.Second != 0 {
			t.Fatalf("duration outside configured interval: %v", duration)
		}
	}
}

func TestChromePageDwellPinsToolDescriptor(t *testing.T) {
	dwell := &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 600}
	manager := NewMCPToolManager("", "")
	defer manager.Close()
	if err := manager.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": {Type: "chrome-cdp", URL: "http://127.0.0.1:9222", PageDwell: dwell}}); err != nil {
		t.Fatal(err)
	}
	descriptor := manager.GetTools()[1]
	before, err := MCPToolDescriptorSHA256(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	dwell.MinSeconds = 1
	dwell.MaxSeconds = 1
	after, err := MCPToolDescriptorSHA256(manager.GetTools()[1])
	if err != nil || before != after || !strings.Contains(descriptor.Description, "300–600 seconds") {
		t.Fatalf("live declaration changed after startup: %v", err)
	}
	changed := NewMCPToolManager("", "")
	defer changed.Close()
	if err := changed.LoadTools(t.Context(), map[string]MCPServerConfig{"chrome": {Type: "chrome-cdp", URL: "http://127.0.0.1:9222", PageDwell: dwell}}); err != nil {
		t.Fatal(err)
	}
	changedHash, err := MCPToolDescriptorSHA256(changed.GetTools()[1])
	if err != nil || changedHash == before {
		t.Fatalf("changed dwell reused a frozen descriptor: %v", err)
	}
}

func TestChromePageDwellExcludesOnlyIdleTime(t *testing.T) {
	for _, scenario := range []string{"success", "snapshot_failure", "cancelled", "wall_deadline"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, stop := context.WithCancel(t.Context())
				defer stop()
				var outer context.Context = parent
				var idle waitbudget.RunIdleClock
				total, endTotal := idle.WithTimeout(outer, time.Minute)
				defer endTotal()
				outer = total
				if scenario == "wall_deadline" {
					wall, end := context.WithTimeout(parent, time.Minute)
					defer end()
					outer = wall
				}
				coordinator, end := tools.WithInteractiveAwareTimeout(outer, time.Minute)
				defer end()
				worker, endWorker := tools.WithInteractiveAwareTimeout(coordinator, 30*time.Second)
				defer endWorker()
				start := time.Now()
				snapshotError := errors.New("snapshot failed")
				captureAt := time.Time{}
				snapshot := func(context.Context) (string, error) {
					captureAt = time.Now()
					time.Sleep(10 * time.Second) // Actual browser work still consumes budget.
					if scenario == "snapshot_failure" {
						return "", snapshotError
					}
					if scenario == "cancelled" {
						time.AfterFunc(time.Minute, stop)
					}
					return "captured source", nil
				}
				text, err := snapshotChromeWithDwell(worker, snapshot, &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300})
				switch scenario {
				case "snapshot_failure":
					if !errors.Is(err, snapshotError) || time.Since(start) != 10*time.Second {
						t.Fatalf("failed navigation dwelled: %v", err)
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation lost: %v", err)
					}
				case "wall_deadline":
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Minute {
						t.Fatalf("wall deadline moved: %v", err)
					}
				case "success":
					if captureAt != start {
						t.Fatal("source captured after the dwell")
					}
					if !strings.HasPrefix(text, "captured source") {
						t.Fatal("pre-dwell capture was replaced")
					}
					if idle.Elapsed() != 300*time.Second || total.Err() != nil {
						t.Fatal("page dwell consumed total duration budget")
					}
					if err != nil || !strings.Contains(text, "300 seconds") || time.Since(start) != 310*time.Second {
						t.Fatalf("premature navigation result: %q (%v)", text, err)
					}
					if worker.Err() != nil || coordinator.Err() != nil {
						t.Fatal("dwell consumed worker/coordinator timeout")
					}
					time.Sleep(20 * time.Second)
					synctest.Wait()
					if !errors.Is(worker.Err(), context.DeadlineExceeded) || coordinator.Err() != nil {
						t.Fatal("remaining work budget was not restored exactly")
					}
				}
			})
		})
	}
}

func TestChromePageDwellSharesPauseWithProviderWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := tools.WithInteractiveAwareTimeout(t.Context(), time.Minute)
		defer cancel()
		resume := waitbudget.Pause(ctx)
		time.AfterFunc(2*time.Minute, resume)
		if _, err := (&ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}).wait(ctx); err != nil {
			t.Fatal(err)
		}
		deadline, _ := ctx.Deadline()
		if time.Until(deadline) != time.Minute {
			t.Fatal("overlapping waits were counted more than once")
		}
	})
}

func TestChromePageDwellOutlivesDefaultMCPTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dwell := &ChromePageDwellConfig{MinSeconds: 300, MaxSeconds: 300}
		fake := newRuntimeTestServer(t, nil)
		fake.server.AddTool(mcpgo.NewTool("browser_snapshot"), func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			text, err := snapshotChromeWithDwell(ctx, func(context.Context) (string, error) { time.Sleep(10 * time.Second); return "captured source", nil }, dwell)
			if err != nil {
				return nil, err
			}
			return mcpgo.NewToolResultText(text), nil
		})
		manager := NewMCPToolManager("", "")
		attachRuntimeTestServer(t, manager, "chrome", MCPServerConfig{Type: "chrome-cdp", URL: "http://127.0.0.1:9222", PageDwell: dwell}, fake)
		if !manager.GetTools()[0].pausableTimeout {
			t.Fatal("configured dwell kept a fixed transport timeout")
		}
		start := time.Now()
		text, isError, err := manager.ExecuteTool(t.Context(), "chrome__browser_snapshot", "{}")
		if err != nil || isError || !strings.Contains(text, "300 seconds") || time.Since(start) != 310*time.Second {
			t.Fatalf("dwell failed at MCP boundary: %q (%v)", text, err)
		}
	})
}
