//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/manifoldco/promptui"
)

// askUserDeadlineCtx freezes the underlying context's deadline while
// ask_user is active. Once the user answers (IsAskUserActive becomes
// false), the original deadline is restored. This prevents the user's
// response time from counting against the agent's LLM timeout.
type askUserDeadlineCtx struct {
	base context.Context
}

func (c *askUserDeadlineCtx) Deadline() (time.Time, bool) {
	if IsAskUserActive() {
		return time.Time{}, false
	}
	return c.base.Deadline()
}

func (c *askUserDeadlineCtx) Done() <-chan struct{} {
	if IsAskUserActive() {
		return nil
	}
	return c.base.Done()
}

func (c *askUserDeadlineCtx) Err() error {
	if IsAskUserActive() {
		return nil
	}
	return c.base.Err()
}

func (c *askUserDeadlineCtx) Value(key any) any {
	return c.base.Value(key)
}

// AskUserAwareDeadline wraps ctx so that the wrapped context's
// Deadline(), Done(), and Err() report no deadline / no error while
// ask_user is active. When ask_user is not active, the underlying
// context's values are returned unchanged.
//
// If ctx has no deadline, the original context is returned unwrapped.
func AskUserAwareDeadline(ctx context.Context) context.Context {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		return ctx
	}
	return &askUserDeadlineCtx{base: ctx}
}

type askUserArgs struct {
	Question string      `json:"question"`
	Type     string      `json:"type"`
	Options  []askOption `json:"options,omitempty"`
	AllowAny bool        `json:"allow_any,omitempty"`
}

type askOption struct {
	Label string `json:"label"`
	Value string `json:"value,omitempty"`
}

type askResponseType struct {
	Answers []string `json:"answers"`
	Free    string   `json:"free_text,omitempty"`
}

func NewAskUserTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	_ = cfg
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "ask_user",
			Description: "Ask the user a question and wait for their response. Supports multiple choice, free text, or mixed (choices + free text). Use this when you need clarification, confirmation, or input from the user.",
			Parameters: map[string]any{
				"question": map[string]any{
					"type":        "string",
					"description": "The question to ask the user",
				},
				"type": map[string]any{
					"type":        "string",
					"description": "Question type: 'single_choice' (pick one), 'multiple_choice' (pick multiple), 'free_text' (type answer), or 'mixed' (pick from options or type free text)",
				},
				"options": map[string]any{
					"type":        "array",
					"description": "Available choices for single_choice, multiple_choice, or mixed type",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"label": map[string]any{
								"type":        "string",
								"description": "Display label for this option",
							},
							"value": map[string]any{
								"type":        "string",
								"description": "Value to return if selected (defaults to label if empty)",
							},
						},
						"required": []string{"label"},
					},
				},
				"allow_any": map[string]any{
					"type":        "boolean",
					"description": "For single_choice/multiple_choice: also allow free-text input as an answer",
				},
			},
			Required: []string{"question"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeAskUser(ctx, call)
		},
	}
}

// unattendedAskUserResponse produces a safe, non-blocking answer when no human
// is available. For choice questions it picks the first option as a documented
// default; for free-text it returns an error so the agent proceeds on its own
// judgement rather than waiting. Either way it fires a needs-human notification.
func unattendedAskUserResponse(args askUserArgs, questionType string) (fantasy.ToolResponse, error) {
	NotifyNeedsHuman(args.Question)
	fmt.Fprintf(os.Stderr, "\n%s no human available; auto-answering: %s\n", boldFmt("─── Ask User (unattended) ───"), args.Question)

	if (questionType == "single_choice" || questionType == "multiple_choice" || questionType == "mixed") && len(args.Options) > 0 {
		first := args.Options[0]
		val := first.Value
		if val == "" {
			val = first.Label
		}
		resp := askResponseType{Answers: []string{val}}
		data, err := json.Marshal(resp)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal unattended response: %v", err)), nil
		}
		return fantasy.NewTextResponse(string(data)), nil
	}

	return fantasy.NewTextErrorResponse("ask_user unavailable: running unattended with no human to answer. Proceed using your best judgement and reasonable defaults; do not ask again."), nil
}

func executeAskUser(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args askUserArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if args.Question == "" {
		return fantasy.NewTextErrorResponse("question is required"), nil
	}

	questionType := args.Type
	if questionType == "" {
		if len(args.Options) > 0 {
			questionType = "single_choice"
		} else {
			questionType = "free_text"
		}
	}

	if (questionType == "single_choice" || questionType == "multiple_choice" || questionType == "mixed") && len(args.Options) == 0 && !args.AllowAny {
		questionType = "free_text"
	}

	// TUI mode: use the native dialog — no terminal release or stdin needed.
	tuiOpts := make([]AskUserTUIOption, len(args.Options))
	for i, o := range args.Options {
		tuiOpts[i] = AskUserTUIOption{Label: o.Label, Value: o.Value}
	}
	if jsonResp, ok := TryAskUserTUI(ctx, args.Question, questionType, tuiOpts, args.AllowAny); ok {
		return fantasy.NewTextResponse(jsonResp), nil
	}

	// TUI mode returned false — check if context was cancelled.
	if err := ctx.Err(); err != nil {
		return fantasy.NewTextErrorResponse("ask_user cancelled: " + err.Error()), nil
	}

	// Unattended / non-interactive: there is no human to read from stdin.
	// Returning here is critical — reading stdin would block forever (and the
	// ask_user-aware deadline means the agent timeout would not rescue it).
	// Instead return a safe default and fire a needs-human notification so an
	// operator can follow up out-of-band.
	if IsUnattended(ctx) || !IsInteractiveEnvironment() {
		return unattendedAskUserResponse(args, questionType)
	}

	// CLI mode: read from stdin.
	StdinMu.Lock()
	defer StdinMu.Unlock()

	SetAskUserActive(true)
	defer SetAskUserActive(false)

	NotifyAskUserStart()

	fmt.Fprintf(os.Stderr, "\n%s\n", boldFmt("─── Ask User ───"))
	fmt.Fprintf(os.Stderr, "%s\n", args.Question)

	switch questionType {
	case "single_choice":
		return handleSingleChoice(args)
	case "multiple_choice":
		return handleMultipleChoice(args)
	case "free_text":
		return handleFreeText(args)
	case "mixed":
		return handleMixed(args)
	default:
		if len(args.Options) > 0 {
			return handleSingleChoice(args)
		}
		return handleFreeText(args)
	}
}

func handleSingleChoice(args askUserArgs) (fantasy.ToolResponse, error) {
	var items []string
	for _, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		items = append(items, label)
	}

	if args.AllowAny {
		items = append(items, "[Type your own answer]")
	}

	prompt := promptui.Select{
		Label: "Select Option",
		Items: items,
		Size:  10,
	}

	index, _, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	if args.AllowAny && index == len(args.Options) {
		promptText := promptui.Prompt{
			Label: "Your answer",
		}
		freeInput, err := promptText.Run()
		if err != nil {
			if err == promptui.ErrInterrupt {
				return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
			}
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		resp := askResponseType{Free: strings.TrimSpace(freeInput)}
		data, err := json.Marshal(resp)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
		}
		return fantasy.NewTextResponse(string(data)), nil
	}

	selected := args.Options[index]
	value := selected.Value
	if value == "" {
		value = selected.Label
	}

	resp := askResponseType{Answers: []string{value}}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMultipleChoice(args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}

	prompt := promptui.Prompt{
		Label: "Your choices (comma-separated, e.g. 1,3)",
	}
	input, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return fantasy.NewTextErrorResponse("no choices provided"), nil
	}

	var answers []string
	parts := strings.Split(input, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		choice, err := strconv.Atoi(p)
		if err != nil || choice < 1 || choice > len(args.Options) {
			if args.AllowAny {
				answers = append(answers, p)
				continue
			}
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid choice: %q", p)), nil
		}
		selected := args.Options[choice-1]
		value := selected.Value
		if value == "" {
			value = selected.Label
		}
		answers = append(answers, value)
	}

	resp := askResponseType{Answers: answers}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleFreeText(args askUserArgs) (fantasy.ToolResponse, error) {
	prompt := promptui.Prompt{
		Label: "Your answer",
	}
	input, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	resp := askResponseType{Free: strings.TrimSpace(input)}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func handleMixed(args askUserArgs) (fantasy.ToolResponse, error) {
	for i, opt := range args.Options {
		label := opt.Label
		if opt.Value != "" && opt.Value != opt.Label {
			label = fmt.Sprintf("%s (%s)", opt.Label, opt.Value)
		}
		fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt(fmt.Sprintf("[%d]", i+1)), label)
	}
	fmt.Fprintf(os.Stderr, "  %s %s\n", cyanFmt("[0]"), "Type your own answer")

	prompt := promptui.Prompt{
		Label: "Your choice(s) or free text",
	}
	input, err := prompt.Run()
	if err != nil {
		if err == promptui.ErrInterrupt {
			return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	input = strings.TrimSpace(input)
	if input == "" {
		return fantasy.NewTextErrorResponse("no input provided"), nil
	}

	var answers []string
	var freeText string

	parts := strings.Split(input, ",")
	hasNumber := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if choice, err := strconv.Atoi(p); err == nil && choice >= 0 && choice <= len(args.Options) {
			hasNumber = true
			if choice == 0 {
				promptText := promptui.Prompt{
					Label: "Your answer",
				}
				freeInput, err := promptText.Run()
				if err != nil {
					if err == promptui.ErrInterrupt {
						return fantasy.NewTextErrorResponse("ask_user cancelled by user"), nil
					}
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				freeText = strings.TrimSpace(freeInput)
			} else {
				selected := args.Options[choice-1]
				value := selected.Value
				if value == "" {
					value = selected.Label
				}
				answers = append(answers, value)
			}
		}
	}

	if !hasNumber {
		freeText = input
	}

	resp := askResponseType{Answers: answers, Free: freeText}
	data, err := json.Marshal(resp)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to marshal response: %v", err)), nil
	}
	return fantasy.NewTextResponse(string(data)), nil
}

func boldFmt(s string) string {
	return fmt.Sprintf("\033[1m%s\033[0m", s)
}

func cyanFmt(s string) string {
	return fmt.Sprintf("\033[36m%s\033[0m", s)
}
