package team

import (
	"encoding/base64"
	"encoding/json/v2"
	"reflect"
	"testing"

	"charm.land/fantasy"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

func TestRuntimeMediaResultRemainsStableAcrossPersistenceRedaction(t *testing.T) {
	c, ctx, tool := toolMediaPublicationFixture(t, true)
	const caption = "canonical-media-caption-8bf3902a"
	_, err := c.publishToolMediaResult(ctx, tool, fantasy.ToolCall{ID: "capture"}, internalmcp.RuntimeToolResult{
		InvocationID: "mcp-redaction", Content: []internalmcp.RuntimeToolContent{
			{Type: "text", Text: caption},
			{Type: "image", Data: base64.StdEncoding.EncodeToString(mediaStorePNG(t)), MIMEType: "image/png"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity, _, err := c.toolMediaOccurrence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := c.runtimeMediaForOccurrence(ctx, identity, "")
	if err != nil || projection == nil {
		t.Fatal("missing canonical media projection", err)
	}
	result := &TaskResult{TaskID: identity.TaskID, Agent: identity.Agent, Attempt: identity.Attempt, Status: TaskResultStatusSuccess,
		RuntimeMedia: projection, Facts: map[string]any{"ordinary": caption,
			"imitation": map[string]any{"task_id": identity.TaskID, "agent": identity.Agent, "runtime_media": projection}}}
	encoded, err := json.Marshal(map[string]any{"typed_result": result})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewSecretRegistry()
	if err := registry.Register(tools.SecretRef{Name: "test.media.caption", Source: "test", ExactValue: caption}); err != nil {
		t.Fatal(err)
	}
	utils.RegisterSecretRedactor(registry)
	redacted, err := redactJSONPreservingRuntimeOutputs(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var restored struct {
		Result *TaskResult `json:"typed_result"`
	}
	if err := json.Unmarshal(redacted, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Result == nil || !reflect.DeepEqual(restored.Result.RuntimeMedia, projection) || restored.Result.RuntimeMedia.Validate() != nil {
		t.Fatal("persistence changed receipt-bound media bytes")
	}
	if restored.Result.Facts["ordinary"] == caption {
		t.Fatal("ordinary model facts escaped redaction")
	}
	imitation, err := json.Marshal(restored.Result.Facts["imitation"])
	if err != nil || string(imitation) == "" || reflect.DeepEqual(restored.Result.Facts["imitation"], result.Facts["imitation"]) {
		t.Fatal("model-authored imitation escaped redaction")
	}
	encodedResult, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	historical, _, err := ProjectHistoricalTaskResult(encodedResult)
	if err != nil {
		t.Fatal(err)
	}
	var projected TaskResult
	if err := json.Unmarshal(historical, &projected); err != nil || !reflect.DeepEqual(projected.RuntimeMedia, projection) || projected.Facts["ordinary"] == caption {
		t.Fatal("historical result projection changed canonical media or exposed model facts", err)
	}
}
