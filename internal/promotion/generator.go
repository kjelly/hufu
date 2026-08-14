package promotion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type DraftRequest struct {
	SourceID     string  `json:"source_id"`
	Kind         string  `json:"kind"`
	Content      string  `json:"content"`
	AgentID      string  `json:"agent_id,omitempty"`
	AllowedTypes []Type  `json:"allowed_types"`
	Metrics      Metrics `json:"metrics"`
}
type DraftResult struct {
	Type      Type     `json:"type"`
	AgentID   string   `json:"agent_id,omitempty"`
	SkillName string   `json:"skill_name,omitempty"`
	Draft     string   `json:"draft"`
	Steps     []string `json:"steps,omitempty"`
}
type DraftGenerator interface {
	Generate(context.Context, DraftRequest) (DraftResult, error)
}
type TextGenerator interface {
	GenerateText(context.Context, string) (string, error)
}
type JSONDraftGenerator struct{ Generator TextGenerator }

func (g JSONDraftGenerator) Generate(ctx context.Context, req DraftRequest) (DraftResult, error) {
	if g.Generator == nil {
		return DraftResult{}, fmt.Errorf("semantic draft generator is not configured")
	}
	b, _ := json.Marshal(req)
	prompt := `Create one reviewable Hufu LTM promotion draft. Return ONLY one JSON object with exactly these fields: type, agent_id, skill_name, draft, steps. Type must be one of allowed_types. For skill, draft must be a complete SKILL.md with YAML frontmatter and at least two concrete verifiable steps. For policies, draft is Markdown body only and must not contain YAML frontmatter. Do not invent evidence or include credentials. Input: ` + string(b)
	raw, err := g.Generator.GenerateText(ctx, prompt)
	if err != nil {
		return DraftResult{}, err
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		return DraftResult{}, fmt.Errorf("generator returned fenced JSON")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var out DraftResult
	if err = dec.Decode(&out); err != nil {
		return out, fmt.Errorf("invalid generator JSON: %w", err)
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return out, fmt.Errorf("generator returned trailing JSON")
	}
	return out, nil
}
