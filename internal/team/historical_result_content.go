package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/utils"
)

// ProjectHistoricalTaskResult returns a detached, complete redacted JSON view
// and the SHA-256 of the original typed payload bytes. Unknown fields remain
// in the view. Canonical top-level runtime outputs retain their receipt-bound
// values; similarly named fields inside untrusted facts cannot bypass redaction.
func ProjectHistoricalTaskResult(data []byte) (jsonv1.RawMessage, string, error) {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return nil, "", fmt.Errorf("historical typed payload must be a valid JSON object")
	}
	if _, err := decodeUniqueJSON(data); err != nil {
		return nil, "", fmt.Errorf("decode historical typed payload: %w", err)
	}
	var source string
	if raw, ok := object["source"]; ok {
		if err := json.Unmarshal(raw, &source); err != nil {
			return nil, "", fmt.Errorf("decode historical result origin: %w", err)
		}
	}
	var preserved jsontext.Value
	if source == "runtime" && len(object["runtime_outputs"]) != 0 {
		preserved = bytes.Clone(object["runtime_outputs"])
		delete(object, "runtime_outputs")
	}
	var preservedMedia jsontext.Value
	if raw := object["runtime_media"]; len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var result struct {
			TaskID       string                  `json:"task_id"`
			Attempt      int                     `json:"attempt"`
			RuntimeMedia *RuntimeMediaProjection `json:"runtime_media"`
		}
		if err := json.Unmarshal(data, &result); err != nil || result.RuntimeMedia == nil || result.RuntimeMedia.Validate() != nil ||
			result.TaskID != result.RuntimeMedia.TaskID || result.Attempt != result.RuntimeMedia.Attempt {
			return nil, "", fmt.Errorf("historical runtime media projection is invalid")
		}
		preservedMedia = bytes.Clone(raw)
		delete(object, "runtime_media")
	}
	masked, err := json.Marshal(object, json.Deterministic(true), jsontext.PreserveRawStrings(true))
	if err != nil {
		return nil, "", err
	}
	redacted, err := utils.RedactJSON(masked)
	if err != nil {
		return nil, "", fmt.Errorf("project historical typed payload: %w", err)
	}
	var projectedObject map[string]jsontext.Value
	if err := json.Unmarshal(redacted, &projectedObject); err != nil || projectedObject == nil {
		return nil, "", fmt.Errorf("redacted historical typed payload must remain an object")
	}
	if preserved != nil {
		projectedObject["runtime_outputs"] = preserved
	}
	if preservedMedia != nil {
		projectedObject["runtime_media"] = preservedMedia
	}
	projected, err := json.Marshal(projectedObject, json.Deterministic(true), jsontext.PreserveRawStrings(true))
	if err != nil {
		return nil, "", err
	}
	return jsonv1.RawMessage(projected), historicalResultDigest(data), nil
}

func historicalResultDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
