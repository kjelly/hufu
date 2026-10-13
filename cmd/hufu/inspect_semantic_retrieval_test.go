package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
)

func buildSemanticRetrievalInspectFixture(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	var traces strings.Builder
	for i := range 45 {
		fmt.Fprintf(&traces, `{"trace_id":"t%d","run_id":"run-1","task_id":"task-%d","attempt":1,"phase":"execute","agent":"researcher","source":"worker_private","route":"semantic","route_share":0.9,"mode":"active","fallback_reason":"none","latency_millis":%d,"embed_millis":%d}`+"\n", i, i, 100+i, 80+i)
	}
	traces.WriteString(`{"trace_id":"x","run_id":"run-1","task_id":"task-0","attempt":1,"source":"shared_persistent","route":"lexical","mode":"off","fallback_reason":""}` + "\n")
	traces.WriteString(`{"trace_id":"y","run_id":"run-1","task_id":"task-1","attempt":1,"source":"shared_persistent","route":"semantic","mode":"active","fallback_reason":"embedding_warming"}` + "\n")
	status := `{"run_id":"run-1","recorded_at":"2026-10-13T10:00:00Z","status":{"mode":"auto","state":"ready","model":"embeddinggemma:latest","generation_id":"gen-1","queries":46,"semantic_results":45,"routed_semantic":46,"routed_lexical":1,"warmups":1,"refreshes":2}}` + "\n"
	for name, content := range map[string]string{"semantic-retrieval-traces.jsonl": traces.String(), "semantic-runtime-status.jsonl": status} {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return workspace
}

func TestInspectSemanticRetrievalCommand(t *testing.T) {
	workspace := buildSemanticRetrievalInspectFixture(t)
	output, err := runInspectControlDecisions(t, "--workspace", workspace, "--format", "json", "semantic-retrieval", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Kind inspectpkg.Kind                  `json:"kind"`
		Data inspectpkg.SemanticRetrievalData `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode: %v\n%s", err, output)
	}
	if envelope.Kind != inspectpkg.KindSemanticRetrieval || envelope.Data.Summary.Traces != 47 || len(envelope.Data.Summary.Tasks) != 47 ||
		envelope.Data.Runtime == nil || envelope.Data.Runtime.Status.RoutedLexical != 1 {
		t.Fatalf("envelope = %+v", envelope)
	}

	text, err := runInspectControlDecisions(t, "--workspace", workspace, "semantic-retrieval", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Semantic retrieval (run run-1)",
		"Runtime at close of run run-1 (2026-10-13 10:00:00Z): mode auto, state ready, model embeddinggemma:latest, generation gen-1",
		"routed semantic 46 / lexical 1",
		"Traces: 47 across 1 run(s)",
		"worker_private",
		"Fallback reasons: embedding_warming=1",
		"7 earlier task row(s) not shown",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output lacks %q:\n%s", want, text)
		}
	}
	if _, err := runInspectControlDecisions(t, "--workspace", workspace, "semantic-retrieval", "run-missing"); err == nil {
		t.Fatal("unknown run did not fail")
	}
}

func TestWriteSemanticRetrievalRuntimeReport(t *testing.T) {
	var off strings.Builder
	writeSemanticRetrievalRuntimeReport(&off, nil)
	if off.Len() != 0 {
		t.Fatalf("off wrote %q", off.String())
	}
	data := &team.SemanticRetrievalReportData{
		Status:  team.SemanticRuntimeStatus{Mode: "auto", State: "ready", Model: "embeddinggemma:latest", Queries: 3, SemanticResults: 2, RoutedSemantic: 3, RoutedLexical: 4},
		Summary: team.SummarizeSemanticRetrievalTraces(nil),
	}
	data.Summary.Routes = []team.SemanticRouteSummary{{Source: "shared_persistent", Route: "semantic", Mode: "active", Requests: 3, Served: 2, Fallbacks: 1, P50LatencyMS: 90, P95LatencyMS: 200}}
	data.Summary.Fallbacks = map[string]int{"embedding_busy": 1}
	var b strings.Builder
	writeSemanticRetrievalRuntimeReport(&b, data)
	for _, want := range []string{
		"## Semantic Retrieval Runtime",
		"Mode `auto`, state `ready`, model `embeddinggemma:latest`",
		"Auto routed 3 request(s) semantic and 4 lexical.",
		"| shared_persistent | semantic | active | 3 | 2 | 1 | 0 | 90 / 200 |",
		"Fallback reasons: embedding_busy=1.",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("report lacks %q:\n%s", want, b.String())
		}
	}
}
