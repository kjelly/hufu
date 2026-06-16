package team

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

// saveSkillTool lets any agent encode a reusable workflow as a SKILL.md file
// and make it immediately available via load_skill in the same session.
type saveSkillTool struct {
	coordinator *Coordinator
	pOpts       fantasy.ProviderOptions
}

func (t *saveSkillTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name: "save_skill",
		Description: "Save a reusable skill as a SKILL.md file so it can be loaded by future agents or the coordinator. " +
			"Use this when you have solved a non-trivial problem and want to encode the solution as a repeatable workflow. " +
			"The skill is written to the team's local skills/ directory (e.g., .agent-teams/my-team/skills/<name>/SKILL.md) and is immediately available via load_skill.",
		Parameters: map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "Unique skill name in kebab-case (e.g. 'git-rebase', 'deploy-docker'). Used as directory name and lookup key.",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "One-paragraph description of what the skill does and when to use it.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full Markdown body of the skill: step-by-step workflow, examples, rules, and any gotchas learned.",
			},
			"as_draft": map[string]any{
				"type":        "boolean",
				"description": "Save as a draft in skills/drafts/ instead of skills/. Default: false.",
				"default":     false,
			},
		},
		Required: []string{"name", "description", "content"},
	}
}

func (t *saveSkillTool) ProviderOptions() fantasy.ProviderOptions        { return t.pOpts }
func (t *saveSkillTool) SetProviderOptions(opts fantasy.ProviderOptions) { t.pOpts = opts }

func (t *saveSkillTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Content     string `json:"content"`
		AsDraft     bool   `json:"as_draft"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Name == "" {
		return fantasy.NewTextErrorResponse("name is required"), nil
	}
	if args.Description == "" {
		return fantasy.NewTextErrorResponse("description is required"), nil
	}
	if args.Content == "" {
		return fantasy.NewTextErrorResponse("content is required"), nil
	}

	path, err := t.coordinator.saveAndReloadSkill(args.Name, args.Description, args.Content, args.AsDraft)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to save skill: %v", err)), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf(
		"Skill %q saved to %s and is now available via load_skill.", args.Name, path,
	)), nil
}
