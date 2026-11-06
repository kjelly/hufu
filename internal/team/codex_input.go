package team

import (
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"strings"
)

// codexUserInput serializes only the two variants verified by the pinned schema.
// MarshalJSON keeps text and localImage fields mutually exclusive.
type codexUserInput struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
}

func (input codexUserInput) MarshalJSON() ([]byte, error) {
	switch input.Type {
	case "text":
		if input.Path != "" {
			return nil, fmt.Errorf("codex text input cannot contain a path")
		}
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{input.Type, input.Text})
	case "localImage":
		if input.Text != "" || !filepath.IsAbs(input.Path) || strings.ContainsRune(input.Path, 0) {
			return nil, fmt.Errorf("codex localImage requires an absolute owned path")
		}
		return json.Marshal(struct {
			Type string `json:"type"`
			Path string `json:"path"`
		}{input.Type, input.Path})
	default:
		return nil, fmt.Errorf("unsupported Codex input type %q", input.Type)
	}
}
