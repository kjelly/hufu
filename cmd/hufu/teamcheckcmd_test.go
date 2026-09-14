package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

func TestTeamCheckStaticIsReadOnlyAndMarksOnlineSkipped(t *testing.T) {
	searchRoot, teamName := writeTeamCheckFixture(t, "")
	before := snapshotFiles(t, searchRoot)
	command := newTeamCheckCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{teamName, "--agent-team-search-path", searchRoot, "--output", "json"})
	err := command.Execute()
	if exit, ok := err.(interface{ ProcessExitCode() int }); ok && exit.ProcessExitCode() == 2 {
		t.Fatalf("team check operation failed: %v\n%s", err, output.String())
	}
	var document TeamCheckDocument
	if decodeErr := json.Unmarshal(output.Bytes(), &document); decodeErr != nil {
		t.Fatalf("decode JSON: %v\n%s", decodeErr, output.String())
	}
	if document.Kind != "team_check" || document.StaticReadiness != "passed" || document.OnlineReadiness != "not_checked" || document.LearningReadiness != "not_applicable" {
		t.Fatalf("document = %#v", document)
	}
	foundSkipped := false
	for _, check := range document.Checks {
		if check.ID == "online.provider_models" && check.Status == "skipped" && !check.Required {
			foundSkipped = true
		}
	}
	if !foundSkipped {
		t.Fatalf("online skip missing: %#v", document.Checks)
	}
	if after := snapshotFiles(t, searchRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("static team check modified files: before=%v after=%v", before, after)
	}
}

func TestTeamCheckOnlineIsExplicitAndBoundedToModelList(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/v1/models" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	t.Cleanup(server.Close)
	searchRoot, teamName := writeTeamCheckFixture(t, server.URL+"/v1")
	command := newTeamCheckCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{teamName, "--agent-team-search-path", searchRoot, "--online", "--output", "json"})
	_ = command.Execute()
	if requests != 1 {
		t.Fatalf("provider model-list requests = %d, want 1", requests)
	}
	var document TeamCheckDocument
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.OnlineReadiness != "passed" {
		t.Fatalf("online readiness = %q; checks=%#v", document.OnlineReadiness, document.Checks)
	}
}

func TestTeamCheckOutputAliasConflictReturnsJSONUsageDocument(t *testing.T) {
	command := newTeamCheckCommand()
	var output bytes.Buffer
	command.SetOut(&output)
	command.SetArgs([]string{"fixture", "--json", "--output", "text"})
	err := command.Execute()
	if exit, ok := err.(interface{ ProcessExitCode() int }); !ok || exit.ProcessExitCode() != 2 {
		t.Fatalf("error = %v", err)
	}
	var document TeamCheckDocument
	if decodeErr := json.Unmarshal(output.Bytes(), &document); decodeErr != nil || document.Error == nil {
		t.Fatalf("usage JSON = %q, decode=%v", output.String(), decodeErr)
	}
}

func TestOnlineTeamCheckDeadlineIsClassifiedWithoutInference(t *testing.T) {
	session := &team.TeamSession{Config: agent.TeamConfig{
		WorkerModel: "ollama/fixture-model", CoordinatorModel: "ollama/fixture-model",
		ProviderURL: "http://127.0.0.1:1/v1",
	}}
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	items := onlineTeamCheckItems(ctx, session, &config.Config{}, team.RoleModels{}, nil)
	foundUnknown := false
	for _, item := range items {
		if item.ID == "online.provider.ollama" && item.Status == "unknown" && item.Required && item.ReasonCode == "provider_unreachable" {
			foundUnknown = true
		}
	}
	if !foundUnknown || readinessFor(items, "online") != "failed" {
		t.Fatalf("deadline classification = %#v", items)
	}
}

func writeTeamCheckFixture(t *testing.T, providerURL string) (string, string) {
	t.Helper()
	root := t.TempDir()
	name := "check-team"
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: " + name + "\nworker-model: ollama/fixture-model\ncoordinator-model: ollama/fixture-model\n"
	if providerURL != "" {
		manifest += "provider-url: " + providerURL + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: developer\nrole: worker\ntools: view,grep,glob,ls\n---\nHelp safely.\n"
	if err := os.WriteFile(filepath.Join(dir, "developer.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}
	coordinator := "---\nname: coordinator\nrole: coordinator\ntools: view\n---\nCoordinate safely.\n"
	if err := os.WriteFile(filepath.Join(dir, "coordinator.md"), []byte(coordinator), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, name
}

func snapshotFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return result
}
