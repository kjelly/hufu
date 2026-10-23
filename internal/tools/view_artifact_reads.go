package tools

import (
	"encoding/json/v2"

	"charm.land/fantasy"
)

type viewReadMetadata struct {
	ArtifactReads []string `json:"artifact_reads"`
	ArtifactOnly  bool     `json:"artifact_only,omitzero"`
}

func artifactViewResponse(content, ref string) fantasy.ToolResponse {
	response := fantasy.NewTextResponse(content)
	setViewArtifactReads(&response, []string{ref}, true)
	return response
}

func setViewArtifactReads(response *fantasy.ToolResponse, refs []string, artifactOnly bool) {
	if len(refs) == 0 {
		return
	}
	raw, err := json.Marshal(viewReadMetadata{ArtifactReads: refs, ArtifactOnly: artifactOnly})
	if err == nil {
		response.Metadata = string(raw)
	}
}

// ViewArtifactReads reports only artifact-only successful reads whose output was
// included in this response. Callers must first authenticate the built-in view
// tool; metadata from an arbitrary tool is not authorization evidence.
func ViewArtifactReads(response fantasy.ToolResponse) []string {
	if response.IsError || response.Metadata == "" {
		return nil
	}
	var metadata viewReadMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata, json.RejectUnknownMembers(true)) != nil {
		return nil
	}
	if !metadata.ArtifactOnly {
		return nil
	}
	return metadata.ArtifactReads
}
