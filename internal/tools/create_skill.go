package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"charm.land/fantasy"
)

type createSkillArgs struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

func NewCreateSkillTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "create_skill"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "create_skill",
			Description: "Create a new agent skill dynamically. Use this when you encounter a repetitive task or a missing tool. The content should be a Markdown file with instructions and scripts.",
			Parameters: map[string]any{
				"name": map[string]any{
					"type":        "string",
					"description": "Name of the skill (e.g., 'data-analyzer', 'custom-bash-tool')",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "A short summary of what this skill does.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The Markdown content of the SKILL.md file. You MUST include YAML frontmatter with `name:` and `description:` at the top.",
				},
			},
			Required: []string{"name", "description", "content"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var args createSkillArgs
			if err := parseArgs(call.Input, &args); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid input JSON: %v", err)), nil
			}

			if args.Name == "" || args.Content == "" {
				return fantasy.NewTextErrorResponse("name and content are required"), nil
			}

			if cfg.WorkDir == "" {
				return fantasy.NewTextErrorResponse("workspace not configured"), nil
			}

			skillsDir := filepath.Join(cfg.WorkDir, "skills", args.Name)
			if err := os.MkdirAll(skillsDir, 0o755); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to create skill directory: %v", err)), nil
			}

			skillPath := filepath.Join(skillsDir, "SKILL.md")
			if err := os.WriteFile(skillPath, []byte(args.Content), 0o644); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to write skill file: %v", err)), nil
			}

			return fantasy.NewTextResponse(fmt.Sprintf("Successfully created skill '%s' at %s.", args.Name, skillPath)), nil
		},
	}
}
