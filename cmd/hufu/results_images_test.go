package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

func TestResultsImagesRetainSelectedResultScope(t *testing.T) {
	workspace, ids := resultsTestFixture(t)
	deps := resultsTestDeps(t)
	var selected team.ResultImageScope
	listCalls, exports := 0, 0
	deps.images = func(_ context.Context, gotWorkspace string, scope team.ResultImageScope) ([]team.ResultImage, error) {
		listCalls++
		if gotWorkspace != workspace || scope.RunID != "run-review" || scope.TaskID != "1" || scope.BranchID != "main" || scope.EventID == "" || scope.EventHash == "" {
			t.Fatalf("image query lost selected historical result: workspace=%s scope=%+v", gotWorkspace, scope)
		}
		selected = scope
		return []team.ResultImage{{Image: media.Ref{ID: "image-fixture", MediaType: "image/png", ByteSize: 123, Width: 2, Height: 3}, Occurrences: 2}}, nil
	}
	deps.exportImage = func(_ context.Context, gotWorkspace string, scope team.ResultImageScope, id, destination string) error {
		exports++
		if gotWorkspace != workspace || scope != selected || id != "image-fixture" || destination == "" {
			t.Fatal("export widened or lost its selected image scope")
		}
		return nil
	}
	args := []string{"images", ids[0], "--workspace", workspace, "--team", "review", "--output", "json"}
	_, output, err := executeResultsTest(t, deps, args...)
	if err != nil {
		t.Fatal(err)
	}
	var envelope resultImagesEnvelope
	if err := json.Unmarshal([]byte(output), &envelope); err != nil || envelope.ResultID != ids[0] || len(envelope.Images) != 1 || envelope.Images[0].Occurrences != 2 || exports != 0 {
		t.Fatal("metadata query exported content or lost image metadata", err)
	}
	_, _, err = executeResultsTest(t, deps, append(args, "--artifact", "image-fixture", "--export", filepath.Join(t.TempDir(), "image.png"))...)
	if err != nil || exports != 1 {
		t.Fatal("scoped export was not dispatched once", err)
	}
	before := listCalls
	if _, _, err := executeResultsTest(t, deps, "images", ids[0], "--workspace", workspace, "--team", "other"); err == nil || listCalls != before {
		t.Fatal("wrong team scope reached the image reader")
	}
	if _, _, err := executeResultsTest(t, deps, append(args, "--artifact", "unrelated", "--export", "denied.png")...); err == nil || exports != 1 {
		t.Fatal("unrelated image reached export")
	}
	if _, _, err := executeResultsTest(t, deps, append(args, "--export", "denied.png")...); err == nil || exports != 1 {
		t.Fatal("export without exact artifact ID was accepted")
	}
	deps.exportImage = func(context.Context, string, team.ResultImageScope, string, string) error {
		return errors.New("fixture export failed")
	}
	_, output, err = executeResultsTest(t, deps, append(args, "--artifact", "image-fixture", "--export", "failed.png")...)
	if err == nil || output != "" {
		t.Fatal("failed export advertised success")
	}
}
