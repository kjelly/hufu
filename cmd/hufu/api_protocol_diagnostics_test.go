package main

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/config"
)

func TestAPIProtocolDiagnosticsSelectionAndPrivacy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "unexpected probe", 500) }))
	t.Cleanup(server.Close)
	for _, tc := range []struct{ raw, requested, effective, source string }{
		{"", "legacy", "chat-completions", "legacy"},
		{" CHAT-COMPLETIONS ", "chat-completions", "chat-completions", "explicit"},
		{" ReSpOnSeS ", "responses", "responses", "explicit"},
	} {
		t.Run(tc.requested, func(t *testing.T) {
			const secret = "protocol-diagnostic-credential"
			cfg := &config.Config{Backends: map[string]config.BackendConfig{"remote": {Kind: "llm", Type: "openai-compatible", BaseURL: server.URL + "/private/v1", ProviderAPIKey: secret, APIProtocol: tc.raw}}}
			before := cfg.Backends["remote"]
			report := doctorReport{SchemaVersion: 1}
			collectDoctorAPIProtocols(&report, cfg)
			var found bool
			for _, check := range report.Checks {
				if check.Subject != "remote" {
					continue
				}
				p := check.APIProtocol
				if check.Status != "pass" || p == nil || p.Requested != tc.requested || p.Effective != tc.effective || p.Source != tc.source || p.BindingVersion != 1 {
					t.Fatalf("protocol diagnostic=%+v", check)
				}
				found = true
			}
			if !found {
				t.Fatal("named provider protocol was not reported")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var rendered bytes.Buffer
			if err := renderDoctorReportText(&rendered, report); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded)+rendered.String(), secret) || strings.Contains(string(encoded)+rendered.String(), server.URL) {
				t.Fatal("protocol diagnostics exposed a credential or endpoint")
			}
			if !reflect.DeepEqual(before, cfg.Backends["remote"]) {
				t.Fatal("diagnostics mutated configuration")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatal("protocol inspection contacted a provider")
	}
}

func TestAPIProtocolDiagnosticsRejectInvalidConfigurationWithoutProbe(t *testing.T) {
	for _, backend := range []config.BackendConfig{
		{Kind: "llm", Type: "openai-compatible", APIProtocol: "auto"},
		{Kind: "agent", Type: "codex-app-server", APIProtocol: "responses"},
	} {
		report := doctorReport{SchemaVersion: 1}
		collectDoctorAPIProtocols(&report, &config.Config{Backends: map[string]config.BackendConfig{"remote": backend}})
		if len(report.Checks) != 1 || report.Checks[0].Status != "fail" || report.Checks[0].APIProtocol != nil {
			t.Fatal("invalid API protocol was presented as selected")
		}
	}
}

func TestTeamCheckAPIProtocolOfflineReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("HUFU_STATE_HOME", filepath.Join(home, "state"))
	t.Chdir(t.TempDir())
	oldOpts := opts
	opts = runOptions{}
	t.Cleanup(func() { opts = oldOpts })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); http.Error(w, "unexpected probe", 500) }))
	t.Cleanup(server.Close)
	search, name := writeTeamCheckFixture(t, server.URL+"/v1")
	path := filepath.Join(search, name, "team.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(fmt.Sprintf("providers:\n  ollama:\n    provider-url: %s/v1\n    provider-api-key: diagnostic-test-credential\n    api-protocol: responses\n", server.URL))...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotFiles(t, search)
	command := newTeamCheckCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{name, "--agent-team-search-path", search, "--output", "json"})
	if err := command.Execute(); err != nil {
		t.Fatalf("team check: %v\n%s", err, output.String())
	}
	var result TeamCheckDocument
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, check := range result.Checks {
		if check.ID != "static.api_protocol.ollama" {
			continue
		}
		p := check.APIProtocol
		if p == nil || p.Requested != "responses" || p.Effective != "responses" || p.Source != "explicit" || p.BindingVersion != 1 || check.Status != "passed" {
			t.Fatalf("protocol check=%+v", check)
		}
		found = true
	}
	if !found || result.StaticReadiness != "passed" || result.OnlineReadiness != "not_checked" || requests.Load() != 0 {
		t.Fatal("offline protocol diagnostics were missing or contacted a provider")
	}
	if strings.Contains(output.String(), "diagnostic-test-credential") || strings.Contains(output.String(), server.URL) {
		t.Fatal("team check exposed provider configuration")
	}
	if !reflect.DeepEqual(before, snapshotFiles(t, search)) {
		t.Fatal("team check mutated team files")
	}
}
