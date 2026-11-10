package team

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

func TestHistoricalTypedProjectionPreservesCompleteJSON(t *testing.T) {
	source := []byte(`{"summary":"analysis","future_detail":{"count":9007199254740993,"text":"完整未知欄位","source":"runtime","runtime_outputs":{"api_key":"history-nested-credential-8d1ca2"}},"api_key":"historical-projection-credential-6d9bf1"}`)
	original := bytes.Clone(source)
	projected, digest, err := ProjectHistoricalTaskResult(source)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, original) || digest != historicalResultDigest(original) || digest == historicalResultDigest(projected) {
		t.Fatal("source bytes, canonical payload hash and redacted projection were conflated")
	}
	if !bytes.Contains(projected, []byte("9007199254740993")) || !bytes.Contains(projected, []byte("完整未知欄位")) || bytes.Contains(projected, []byte("historical-projection-credential-6d9bf1")) || bytes.Contains(projected, []byte("history-nested-credential-8d1ca2")) {
		t.Fatalf("projection dropped content, rounded a number or leaked a credential: %s", projected)
	}
	for _, data := range [][]byte{
		[]byte(`[]`), []byte(`null`), []byte(`{"summary":"a","summary":"b"}`),
		[]byte(`{"future":{"x":1,"x":2}}`), []byte(`{} {}`), []byte("{\"summary\":\"\xff\"}"),
	} {
		if view, hash, err := ProjectHistoricalTaskResult(data); err == nil || view != nil || hash != "" {
			t.Errorf("invalid typed payload returned a partial projection: %q, %q, %v", view, hash, err)
		}
	}
}

func TestHistoricalTypedProjectionPreservesRuntimeReceipts(t *testing.T) {
	const collision = "history-runtime-output-collision-7461fca2"
	outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"receipt": map[string]any{
		"observation": collision, "count": jsonv1.Number("9007199254740993"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	result := TaskResult{TaskID: "1", Agent: "worker", Attempt: 1, Status: TaskResultStatusSuccess,
		Summary: collision, Source: "runtime", RuntimeOutputs: outputs, RuntimeOutputsHash: digest}
	data, err := jsonv1.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	utils.RedactSecrets("api_token: " + collision)
	projected, sourceHash, err := ProjectHistoricalTaskResult(data)
	if err != nil {
		t.Fatal(err)
	}
	var header historicalResultHeader
	if err := json.Unmarshal(projected, &header); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(header.Summary, collision) || sourceHash != historicalResultDigest(data) {
		t.Fatal("presentation did not redact prose independently of its canonical payload hash")
	}
	if err := validateHistoricalResultHashes(header); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(header.RuntimeOutputs, []byte(collision)) || !bytes.Contains(header.RuntimeOutputs, []byte("9007199254740993")) || header.RuntimeOutputsHash != digest {
		t.Fatal("history projection rewrote receipt-bound runtime outputs")
	}
}
