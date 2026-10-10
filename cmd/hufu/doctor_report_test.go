package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func doctorCheckByID(t *testing.T, report doctorReport, id string) doctorCheck {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing doctor check %s: %#v", id, report.Checks)
	return doctorCheck{}
}

func collectWorkspaceReport(t *testing.T, workspace string) doctorReport {
	t.Helper()
	report := doctorReport{SchemaVersion: 1, Checks: []doctorCheck{}}
	collectDoctorWorkspace(t.Context(), &report, workspace)
	report.finish()
	return report
}

func appendDoctorTaskEvent(t *testing.T, store *team.EventStore, id string, branch string) {
	t.Helper()
	store.SetBranchID(branch)
	item := team.TodoItem{ID: id, Agent: "worker", Desc: "external task", Status: team.TaskBlocked,
		SideEffect: team.SideEffectExternalWrite, RecoveryState: team.RecoveryStateUnknown}
	payload, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(team.RunEvent{ID: "event-" + id, Type: "task_created", TaskID: id, Actor: "worker", Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorReportStatusSortingAndTextParity(t *testing.T) {
	tests := []struct {
		name   string
		checks []doctorCheck
		want   string
	}{
		{"ready", []doctorCheck{{ID: "provider.reachable", Status: "pass", Message: "provider reachable"}}, "ready"},
		{"warning", []doctorCheck{{ID: "teams.discovery", Status: "warning", Message: "no teams"}}, "degraded"},
		{"unknown", []doctorCheck{{ID: "recovery.unresolved", Status: "unknown", Message: "checkpoint absent"}}, "degraded"},
		{"provider failure", []doctorCheck{{ID: "provider.reachable", Status: "fail", Message: "provider unavailable"}}, "failed"},
		{"team contract failure", []doctorCheck{{ID: "teams.contract", Subject: "team-a", Status: "fail", Message: "contract invalid"}}, "failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := doctorReport{SchemaVersion: 1, Checks: tt.checks}
			report.finish()
			if report.Status != tt.want {
				t.Fatalf("status = %q, want %q", report.Status, tt.want)
			}
			var text bytes.Buffer
			if err := renderDoctorReportText(&text, report); err != nil {
				t.Fatal(err)
			}
			for _, check := range report.Checks {
				if !strings.Contains(text.String(), check.ID) || !strings.Contains(text.String(), check.Message) {
					t.Fatalf("text did not render JSON finding %#v: %s", check, text.String())
				}
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var decoded doctorReport
			if err := json.Unmarshal(data, &decoded); err != nil || decoded.Status != report.Status {
				t.Fatalf("JSON report = %#v, err=%v", decoded, err)
			}
		})
	}
	report := doctorReport{Checks: []doctorCheck{{ID: "z", Subject: "b", Status: "pass"}, {ID: "a", Status: "pass"}, {ID: "z", Subject: "a", Status: "pass"}}}
	report.finish()
	if report.Checks[0].ID != "a" || report.Checks[1].Subject != "a" || report.Checks[2].Subject != "b" {
		t.Fatalf("checks are not sorted: %#v", report.Checks)
	}
}

func TestDoctorWorkspaceEmptyHistoryAndProbeSafety(t *testing.T) {
	workspace := t.TempDir()
	legacyProbe := filepath.Join(workspace, ".hufu-doctor-probe")
	if err := os.WriteFile(legacyProbe, []byte("owner data"), 0o600); err != nil {
		t.Fatal(err)
	}
	report := collectWorkspaceReport(t, workspace)
	if report.Status != "ready" || doctorCheckByID(t, report, "events.integrity").Status != "pass" {
		t.Fatalf("empty workspace report = %#v", report)
	}
	if check := doctorCheckByID(t, report, "recovery.unresolved"); check.Message != "no prior session" || check.Count != nil {
		t.Fatalf("empty workspace recovery = %#v", check)
	}
	data, err := os.ReadFile(legacyProbe)
	if err != nil || string(data) != "owner data" {
		t.Fatalf("legacy probe changed: data=%q err=%v", data, err)
	}
	if matches, err := filepath.Glob(filepath.Join(workspace, ".hufu-doctor-probe-*")); err != nil || len(matches) != 0 {
		t.Fatalf("temporary doctor probes remain: %v, err=%v", matches, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "logs", "event_store.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("doctor created event log: %v", err)
	}
}

func TestDoctorWorkspaceExistingEmptyCheckpointReportsZero(t *testing.T) {
	workspace := t.TempDir()
	if err := team.SaveSession(workspace, team.NewSession()); err != nil {
		t.Fatal(err)
	}
	check := doctorCheckByID(t, collectWorkspaceReport(t, workspace), "recovery.unresolved")
	if check.Status != "pass" || check.Count == nil || *check.Count != 0 {
		t.Fatalf("existing empty checkpoint = %#v", check)
	}
}

func TestDoctorProbeErrorPreservesExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(path, []byte("owner data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkWritable(path); err == nil {
		t.Fatal("file path should not be treated as writable workspace")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "owner data" {
		t.Fatalf("existing file changed: data=%q err=%v", data, err)
	}
}

func TestDoctorWorkspaceHistoryCheckpointAndIntegrity(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewEventStore(workspace, "run-doctor", "session-doctor")
	if err != nil {
		t.Fatal(err)
	}
	appendDoctorTaskEvent(t, store, "main-task", "main")
	appendDoctorTaskEvent(t, store, "sibling-task", "experiment")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree := team.NewSessionTree()
	tree.Branches["experiment"] = &team.SessionBranch{ID: "experiment", Name: "experiment", ParentID: "main"}
	if err := team.SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	withoutCheckpoint := collectWorkspaceReport(t, workspace)
	if check := doctorCheckByID(t, withoutCheckpoint, "recovery.unresolved"); check.Status != "unknown" || check.Count != nil {
		t.Fatalf("missing checkpoint = %#v", check)
	}
	session := team.NewSession()
	if err := team.SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	report := collectWorkspaceReport(t, workspace)
	if check := doctorCheckByID(t, report, "recovery.unresolved"); check.Status != "warning" || check.Count == nil || *check.Count != 1 {
		t.Fatalf("active-branch recovery = %#v", check)
	}
	path := filepath.Join(workspace, "logs", "event_store.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := bytes.Replace(before, []byte(`"type":"task_created"`), []byte(`"type":"task_started"`), 1)
	if bytes.Equal(before, corrupt) {
		t.Fatal("test fixture did not change event type")
	}
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	broken := collectWorkspaceReport(t, workspace)
	if doctorCheckByID(t, broken, "events.integrity").Status != "fail" || doctorCheckByID(t, broken, "recovery.unresolved").Status != "unknown" {
		t.Fatalf("corrupt history report = %#v", broken)
	}
	if check := doctorCheckByID(t, broken, "events.integrity"); strings.Contains(check.Message, "main-task") {
		t.Fatalf("event payload leaked: %#v", check)
	}
}

func TestDoctorWorkspaceRejectsMalformedCheckpointAndBranch(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := doctorCheckByID(t, collectWorkspaceReport(t, workspace), "recovery.unresolved"); check.Status != "fail" {
		t.Fatalf("malformed checkpoint = %#v", check)
	}
	if err := os.WriteFile(filepath.Join(workspace, "session.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "session_tree.json"), []byte(`{"active_branch":"lost","branches":{"main":{"id":"main"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if check := doctorCheckByID(t, collectWorkspaceReport(t, workspace), "recovery.unresolved"); check.Status != "fail" {
		t.Fatalf("invalid branch = %#v", check)
	}
}

func TestDoctorJSONFailureIsOneObjectWithoutSecrets(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.workspace = t.TempDir()
	opts.agentTeamSearchPath = t.TempDir()
	opts.providerURL = "http://user:super-secret@127.0.0.1:1/v1"
	if err := doctorCmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = doctorCmd.Flags().Set("json", "false") }()
	var stdout, stderr bytes.Buffer
	doctorCmd.SetOut(&stdout)
	doctorCmd.SetErr(&stderr)
	doctorCmd.SetContext(t.Context())
	defer doctorCmd.SetOut(nil)
	defer doctorCmd.SetErr(nil)
	defer doctorCmd.SetContext(nil)
	if err := runDoctorReport(doctorCmd, nil); err == nil {
		t.Fatal("provider failure should retain nonzero exit")
	}
	var report doctorReport
	decoder := json.NewDecoder(&stdout)
	if err := decoder.Decode(&report); err != nil {
		t.Fatalf("invalid JSON: %v; stdout=%q", err, stdout.String())
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		t.Fatalf("JSON stdout has trailing content: %v, %q", err, stdout.String())
	}
	if report.Status != "failed" || doctorCheckByID(t, report, "provider.reachable").Status != "fail" {
		t.Fatalf("failed report = %#v", report)
	}
	if check := doctorCheckByID(t, report, "provider.reachable"); check.ReasonCode == "" {
		t.Fatalf("provider failure has no safe diagnostic: %#v", check)
	}
	if strings.Contains(stdout.String(), "super-secret") || strings.Contains(stderr.String(), "super-secret") {
		t.Fatal("doctor leaked provider credentials")
	}
}

func TestDoctorProviderTransportDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		code  string
	}{
		{"cancelled", context.Canceled, "request_cancelled"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"network timeout", &net.DNSError{Err: "secret", Name: "secret.invalid", IsTimeout: true}, "timeout"},
		{"dns", &net.DNSError{Err: "secret", Name: "secret.invalid"}, "dns_failed"},
		{"refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, "connection_refused"},
		{"tls", &tls.CertificateVerificationError{Err: errors.New("secret certificate")}, "tls_failed"},
		{"other", errors.New("secret transport detail"), "transport_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrapped := &url.Error{Op: "Get", URL: "https://user:secret@example.com/private?token=secret", Err: test.cause}
			failure := doctorProviderTransportError(wrapped)
			if failure.Code != test.code || failure.Message == "" || strings.Contains(failure.Error(), "secret") || strings.Contains(failure.Error(), "example.com") {
				t.Fatalf("unsafe or incorrect diagnostic: %#v", failure)
			}
			if !errors.Is(failure, test.cause) {
				t.Fatal("safe diagnostic lost its original error cause")
			}
		})
	}
}

func TestDoctorProviderResponseDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		body     string
		code     string
		wantHTTP int
	}{
		{"unauthorized", http.StatusUnauthorized, "secret response body", "http_error", http.StatusUnauthorized},
		{"unavailable", http.StatusServiceUnavailable, "secret response body", "http_error", http.StatusServiceUnavailable},
		{"invalid model list", http.StatusOK, `{"data":"secret"}`, "invalid_model_list", 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()
			previous := opts
			defer func() { opts = previous }()
			opts.workspace, opts.agentTeamSearchPath, opts.providerURL = t.TempDir(), t.TempDir(), server.URL+"/private?token=secret"
			report := collectDoctorReport(t.Context())
			check := doctorCheckByID(t, report, "provider.reachable")
			if check.Status != "fail" || check.ReasonCode != test.code || check.HTTPStatus != test.wantHTTP {
				t.Fatalf("provider diagnostic = %#v", check)
			}
			if test.wantHTTP != 0 && !strings.Contains(check.Message, fmt.Sprint(test.wantHTTP)) {
				t.Fatalf("HTTP diagnostic omits status: %#v", check)
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var text bytes.Buffer
			if err := renderDoctorReportText(&text, report); err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{string(encoded), text.String()} {
				if strings.Contains(output, "secret") || strings.Contains(output, server.URL) || !strings.Contains(output, check.Message) {
					t.Fatalf("unsafe or missing provider diagnostic: %s", output)
				}
			}
		})
	}
	_, err := fetchModelsContext(t.Context(), "http://user:secret@invalid/%zz", "secret")
	failure, ok := errors.AsType[*doctorProviderError](err)
	if !ok || failure.Code != "invalid_request" || strings.Contains(failure.Error(), "secret") {
		t.Fatalf("invalid request diagnostic = %v", err)
	}
}

func TestDoctorModelStateDistinguishesDeferredConfiguration(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts = runOptions{}
	report := doctorReport{}
	collectDoctorModels(&report, &config.Config{}, nil)
	for _, check := range report.Checks {
		if check.ID == "models.resolved" && (check.ModelState != "deferred" || check.Status != "pass") {
			t.Fatalf("unset model is not deferred: %#v", check)
		}
	}
	if len(report.Checks) == 0 {
		t.Fatal("no role model checks were emitted")
	}
	report.finish()
	if report.Status != "ready" {
		t.Fatalf("deferred configuration degraded the report: %#v", report)
	}
	report = doctorReport{}
	collectDoctorModels(&report, &config.Config{Model: "ollama/available", SidecarModel: "ollama/available"}, []string{"available"})
	for _, check := range report.Checks {
		if check.ID == "models.resolved" && (check.ModelState != "configured" || check.Status != "pass") {
			t.Fatalf("configured model state = %#v", check)
		}
	}
}

func TestDoctorJSONWarningKeepsZeroExit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[]}`))
	}))
	defer server.Close()
	previous := opts
	defer func() { opts = previous }()
	opts.workspace = t.TempDir()
	opts.agentTeamSearchPath = t.TempDir()
	opts.providerURL = server.URL
	if err := doctorCmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = doctorCmd.Flags().Set("json", "false") }()
	var stdout, stderr bytes.Buffer
	doctorCmd.SetOut(&stdout)
	doctorCmd.SetErr(&stderr)
	doctorCmd.SetContext(t.Context())
	defer doctorCmd.SetOut(nil)
	defer doctorCmd.SetErr(nil)
	defer doctorCmd.SetContext(nil)
	if err := runDoctorReport(doctorCmd, nil); err != nil {
		t.Fatalf("warning must keep exit zero: %v", err)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Status != "degraded" {
		t.Fatalf("warning report = %#v err=%v stdout=%q", report, err, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON mode emitted diagnostics to stderr: %q", stderr.String())
	}
}

func TestDoctorTextDiagnosticsUseStderr(t *testing.T) {
	previous := opts
	defer func() { opts = previous }()
	opts.workspace = t.TempDir()
	opts.agentTeamSearchPath = t.TempDir()
	opts.providerURL = "http://127.0.0.1:1/v1"
	if err := doctorCmd.Flags().Set("json", "false"); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	doctorCmd.SetOut(&stdout)
	doctorCmd.SetErr(&stderr)
	doctorCmd.SetContext(t.Context())
	defer doctorCmd.SetOut(nil)
	defer doctorCmd.SetErr(nil)
	defer doctorCmd.SetContext(nil)
	if err := runDoctorReport(doctorCmd, nil); err == nil {
		t.Fatal("provider failure should retain nonzero exit")
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "hufu doctor") || !strings.Contains(stderr.String(), "provider.reachable") {
		t.Fatalf("text streams: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestDoctorContractFailureIsCollected(t *testing.T) {
	searchPath := t.TempDir()
	teamDir := filepath.Join(searchPath, "broken-team")
	if err := os.Mkdir(teamDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: [invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := opts
	defer func() { opts = previous }()
	opts.agentTeamSearchPath = searchPath
	report := doctorReport{SchemaVersion: 1, Checks: []doctorCheck{}}
	collectDoctorTeams(&report, &config.Config{})
	report.finish()
	if report.Status != "failed" || doctorCheckByID(t, report, "teams.contract").Status != "fail" {
		t.Fatalf("team contract report = %#v", report)
	}
}
