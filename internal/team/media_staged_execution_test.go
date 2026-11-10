package team

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/providerintrospection"
	"github.com/kjelly/hufu/internal/providerproxy"
)

func TestStagedMediaExecutionUsesCASAfterAdmissionReleasesBuffers(t *testing.T) {
	requests := make(chan []mediaWorkerWireMessage, 2)
	var committed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model    string                   `json:"model"`
			Messages []mediaWorkerWireMessage `json:"messages"`
		}
		if !committed.Load() || r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.UnmarshalRead(io.LimitReader(r.Body, 1<<20), &request) != nil || request.Model != "vision" {
			http.Error(w, "transport preceded canonical input admission", http.StatusBadRequest)
			return
		}
		select {
		case requests <- request.Messages:
		default:
			http.Error(w, "unexpected extra invocation", http.StatusBadRequest)
			return
		}
		writeCoordinatorMediaToolStream(t, w, request.Model, "submit_result", map[string]any{"status": "success", "summary": "Staged images inspected.", "details": "Original bytes preserved."})
	}))
	t.Cleanup(server.Close)
	factory := func(ctx context.Context, _ string, cfg providerproxy.Config) (providerproxy.Boundary, error) {
		return providerproxy.StartInProcess(ctx, cfg)
	}
	c := newMediaPolicyCoordinatorWithTransport(t, server.URL+"/v1", factory, providerintrospection.CapabilityYes, func(session *TeamSession) {
		configureNativeMediaCost(t, session)
		session.MediaPolicy.AllowedTargets[0].ImageTokenUpperBound = 4096
		session.Config.Acceptance = "true"
		session.Agents["worker"].Tools = "submit_result"
		session.Agents["worker"].ResultTransport = agent.ResultTransportTool
	})
	var second bytes.Buffer
	if err := png.Encode(&second, image.NewRGBA(image.Rect(0, 0, 3, 2))); err != nil {
		t.Fatal(err)
	}
	var staged []*media.StagedImage
	var refs []media.Ref
	for index, data := range [][]byte{mediaStorePNG(t), second.Bytes()} {
		path := filepath.Join(c.projectDir, []string{"first.png", "second.png"}[index])
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		input, err := c.StageImageInput(t.Context(), path, refs)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(input.Close)
		staged, refs = append(staged, input), append(refs, input.Ref())
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	var notices []MediaTurnAdmission
	selected := []*media.StagedImage{staged[1], staged[0], staged[1]}
	result, err := c.RunDirectAgentWithMedia(t.Context(), "worker", "Inspect original staged images.", MediaTurnRequest{
		TurnID: "staged-turn", StagedImages: selected,
		OnAdmission: func(notice MediaTurnAdmission) {
			notices = append(notices, notice)
			for _, input := range selected {
				input.Close()
			}
			committed.Store(notice.State == MediaTurnCommitted)
		},
	})
	if err != nil || result == nil || len(requests) != 1 || len(notices) != 1 || notices[0].State != MediaTurnCommitted || notices[0].TurnID != "staged-turn" || notices[0].ImageCount != 3 || notices[0].SnapshotID == "" {
		t.Fatalf("staged native execution: calls=%d notices=%+v err=%v", len(requests), notices, err)
	}
	items := c.TaskTracker().TodoList().Items()
	if len(items) != 1 || items[0].Status != TaskDone || c.LastRunResult() == nil || !c.LastRunResult().GoalSatisfied {
		t.Fatal("staged input lost its canonical task or whole-run acceptance")
	}
	assertMediaWorkerWireForPrompt(t, nativeRepairImageTurns(t, <-requests), items[0].MediaBinding, "Inspect original staged images.")
	for _, input := range staged {
		requireMediaCode(t, input.WithBytes(t.Context(), func([]byte, media.Ref) error { return nil }), media.InputMissing)
	}
	events, err := c.EventJournal().ReadEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyNativeMediaLineage(t.Context(), c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
	assertNativeMediaCost(t, events, 1)
}
