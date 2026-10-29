package team

import (
	"context"
	"encoding/json/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestHistoricalResultManifestComposesQueryAndSelectedViews(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	compiled, _ := historicalContextManifest(t, c, item, input)
	result, source := dependencyViewRenderFixture()
	view, err := buildDependencyResultView(&DependencyResultViewSpec{Mode: "selected", Fields: []string{"summary"}}, []TaskResult{result}, []DependencyResultViewSource{source})
	if err != nil {
		t.Fatal(err)
	}
	compiled.IncludedItems = append(compiled.IncludedItems, ContextItem{ID: "dependency_results", Kind: "dependency_result",
		Content: view.content, Required: true, DependencyResultView: view.attribution.Clone()})
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, item.Agent, time.Now(), c.session.Config.MemoryLearning)
	if err != nil || manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		t.Fatalf("historical/selected composition = %+v, %v", manifest, err)
	}
	before, _ := json.Marshal(manifest)
	plan, sources, row := querySelectionFixture()
	selection, err := SelectContextQuerySources(t.Context(), &querySelectionRepo{rows: []contextstore.SearchResult{row}}, plan, sources, stableQueryRevision)
	if err != nil {
		t.Fatal(err)
	}
	composed, err := WithContextQueryAttribution(manifest, selection.Attribution)
	if err != nil || composed.SchemaVersion != HistoricalResultManifestSchemaVersion || !supportedDeliveryContextManifest(composed) {
		t.Fatalf("historical/query composition = %+v, %v", composed, err)
	}
	after, _ := json.Marshal(manifest)
	if string(before) != string(after) {
		t.Fatal("query composition mutated the historical manifest")
	}
	if err := validateDependencyResultViewManifest(composed); err != nil {
		t.Fatalf("selected delivery could not validate composed attribution: %v", err)
	}
}

func TestHistoricalResultManifestRejectsUnprovenAttribution(t *testing.T) {
	for _, failure := range []string{"missing", "duplicate", "omitted", "compressed", "bytes", "tokens", "payload", "recipient", "event", "task", "policy", "legacy"} {
		t.Run(failure, func(t *testing.T) {
			c, item, input := historicalContextFixture(t)
			_, manifest := historicalContextManifest(t, c, item, input)
			for i := range manifest.Items {
				entry := &manifest.Items[i]
				if entry.HistoricalResult == nil {
					continue
				}
				switch failure {
				case "missing":
					manifest.Items = append(manifest.Items[:i], manifest.Items[i+1:]...)
				case "duplicate":
					manifest.Items = append(manifest.Items, *entry)
				case "omitted":
					entry.Included = false
				case "compressed":
					entry.Compressed = true
				case "bytes":
					entry.HistoricalResult.Source.PresentationBytes = 0
				case "tokens":
					entry.Tokens = 0
				case "payload":
					entry.HistoricalResult.Source.PayloadSHA256 = "invalid"
				case "recipient":
					entry.HistoricalResult.Binding.Recipient = "observer"
				case "event":
					entry.HistoricalResult.Binding.Grant.EventHash = "invalid"
				case "task":
					entry.HistoricalResult.Binding.TaskID = "different"
				case "policy":
					entry.HistoricalResult.PolicyFingerprint = "invalid"
				case "legacy":
					manifest.SchemaVersion = ContextManifestSchemaVersion
				}
				break
			}
			if manifest.SchemaVersion == HistoricalResultManifestSchemaVersion {
				manifest.Fingerprint = historicalResultManifestFingerprint(manifest)
			} else {
				manifest.Fingerprint = contextManifestFingerprint(manifest)
			}
			if err := ValidateContextQueryManifest(manifest); err == nil {
				t.Fatal("forged or incomplete historical attribution was accepted")
			}
		})
	}
}

func TestHistoricalResultContextUsesConsumerForkValidity(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	rows, err := c.HistoricalTaskReader().Load(t.Context(), HistoricalTaskScope{Workspace: c.session.Workspace, Team: "test", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := tree.CreateBranch("consumer-fork", "", c.EventStore())
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(c.session.Workspace, tree); err != nil {
		t.Fatal(err)
	}
	c.recoveryBranch = branch.ID
	invalid := false
	c.SetHistoricalTaskReader(historicalTaskReaderFunc(func(_ context.Context, scope HistoricalTaskScope) ([]HistoricalTaskResult, error) {
		if scope.Branch != branch.ID {
			t.Fatalf("reconstruction checked ancestor validity: %+v", scope)
		}
		cloned := append([]HistoricalTaskResult(nil), rows...)
		if invalid {
			cloned[0].Validity = "invalidated"
		}
		return cloned, nil
	}))
	compiled, manifest := historicalContextManifest(t, c, item, input)
	if !strings.Contains(compiled.Prompt, "complete deliverable") || manifest.Items == nil {
		t.Fatal("visible ancestor grant did not reconstruct in the consumer fork")
	}
	if !reflect.DeepEqual(item.HistoricalResultBinding.Grant, manifestHistoricalBinding(t, manifest).Grant) {
		t.Fatal("fork rewrote its ancestor-owned grant reference")
	}
	invalid = true
	if _, err := c.compileWorkerContext(t.Context(), input); err == nil {
		t.Fatal("source invalidated only on the consumer fork remained deliverable")
	}
}

func manifestHistoricalBinding(t *testing.T, manifest ContextInjectionManifest) *HistoricalResultTaskBinding {
	t.Helper()
	for _, entry := range manifest.Items {
		if entry.HistoricalResult != nil {
			return entry.HistoricalResult.Binding
		}
	}
	t.Fatal("historical binding is missing")
	return nil
}

func TestHistoricalResultContextDoesNotAutomaticallyReachAuxiliaryModels(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	input.Request = c.newAuxiliaryContextRequest(item.ID, 1, item.Agent, PhaseExecute, "judge", "judge the candidate result")
	input.Goal = input.Request.Goal
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(compiled.Prompt, "complete deliverable") {
		t.Fatal("auxiliary model received a worker's historical source body")
	}
	for _, entry := range compiled.IncludedItems {
		if entry.HistoricalResult != nil {
			t.Fatal("auxiliary model received historical grant attribution")
		}
	}
}
