package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

func (s *Sidecar) ChooseAskUserResponse(ctx context.Context, question, qtype string, opts []tools.AskUserTUIOption, allowAny bool) (tools.AskUserResponse, error) {
	if s == nil || s.agent == nil {
		return tools.AskUserResponse{}, fmt.Errorf("sidecar not initialized")
	}
	if len(opts) == 0 {
		return tools.AskUserResponse{}, fmt.Errorf("no options provided")
	}

	var list strings.Builder
	for i, opt := range opts {
		value := strings.TrimSpace(opt.Value)
		if value == "" {
			value = strings.TrimSpace(opt.Label)
		}
		fmt.Fprintf(&list, "%d. %s", i+1, opt.Label)
		if value != opt.Label {
			fmt.Fprintf(&list, " (value: %s)", value)
		}
		list.WriteByte('\n')
	}

	prompt := fmt.Sprintf(`You are choosing the best answer for an ask_user prompt in unattended mode.
Pick the safest and most appropriate answer from the options.
Use the exact option value when present; otherwise use the label.

Return ONLY JSON in this exact shape:
{"answers":["..."],"free_text":""}

Question type: %s
Allow free text: %t

Options:
%s
User question: %s`, qtype, allowAny, list.String(), question)

	// An unusable reply gets one more attempt that names the problem. A
	// provider failure is returned as is.
	attemptPrompt := prompt
	var lastErr error
	for range 2 {
		result, err := s.generate(ctx, attemptPrompt, ClassifierProfile)
		if err != nil {
			return tools.AskUserResponse{}, err
		}
		normalized, parseErr := parseAskUserSelection(result, opts, qtype, allowAny)
		if parseErr == nil {
			return normalized, nil
		}
		lastErr = parseErr
		attemptPrompt = prompt + fmt.Sprintf("\n\nYour previous reply %s:\n%s\n\nReply again with JSON only, and write each answer exactly as the option is written, without its list number.", parseErr.problem, strings.TrimSpace(result))
	}
	return tools.AskUserResponse{}, lastErr
}

// askUserSelectionError is an unusable selector reply. Its message quotes the
// reply so a rejected answer can be diagnosed from the log.
type askUserSelectionError struct {
	reply   string
	problem string
}

func (e *askUserSelectionError) Error() string {
	reply := e.reply
	if len(reply) > 200 {
		reply = reply[:200] + "..."
	}
	return fmt.Sprintf("ask_user selection response %q %s", reply, e.problem)
}

// parseAskUserSelection decodes one selector reply.
func parseAskUserSelection(result string, opts []tools.AskUserTUIOption, qtype string, allowAny bool) (tools.AskUserResponse, *askUserSelectionError) {
	result = strings.TrimSpace(result)
	if result == "" {
		return tools.AskUserResponse{}, &askUserSelectionError{problem: "was empty"}
	}
	if extracted := jsonCodeBlockRe.FindStringSubmatch(result); len(extracted) >= 2 {
		result = strings.TrimSpace(extracted[1])
	}
	var resp tools.AskUserResponse
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return tools.AskUserResponse{}, &askUserSelectionError{reply: result, problem: "is not valid JSON"}
	}
	normalized, ok := normalizeAskUserSelection(resp, opts, qtype, allowAny)
	if !ok {
		return tools.AskUserResponse{}, &askUserSelectionError{reply: result, problem: "does not name one of the options"}
	}
	return normalized, nil
}

// askUserNumberedAnswer matches an answer that repeats the option's list
// number, such as "1. List the files" or "2) Count only".
var askUserNumberedAnswer = regexp.MustCompile(`^(\d+)\s*[.)]\s*(.+)$`)

func normalizeAskUserSelection(resp tools.AskUserResponse, opts []tools.AskUserTUIOption, qtype string, allowAny bool) (tools.AskUserResponse, bool) {
	if len(opts) == 0 {
		if strings.TrimSpace(resp.Free) == "" {
			return tools.AskUserResponse{}, false
		}
		return tools.AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
	}

	lookup := make(map[string]string, len(opts)*2)
	for idx, opt := range opts {
		val := strings.TrimSpace(opt.Value)
		if val == "" {
			val = strings.TrimSpace(opt.Label)
		}
		lookup[strings.ToLower(val)] = val
		lookup[strings.ToLower(strings.TrimSpace(opt.Label))] = val
		lookup[fmt.Sprintf("%d", idx+1)] = val
	}

	var answers []string
	for _, ans := range resp.Answers {
		trimmed := strings.TrimSpace(ans)
		if trimmed == "" {
			continue
		}
		if normalized, ok := lookup[strings.ToLower(trimmed)]; ok {
			answers = append(answers, normalized)
			continue
		}
		if idx, err := strconv.Atoi(trimmed); err == nil && idx >= 1 && idx <= len(opts) {
			opt := opts[idx-1]
			val := strings.TrimSpace(opt.Value)
			if val == "" {
				val = strings.TrimSpace(opt.Label)
			}
			answers = append(answers, val)
			continue
		}
		// The prompt lists options as "N. label" and models often echo that
		// line. Accept it only when the number and the text name the same
		// option.
		if match := askUserNumberedAnswer.FindStringSubmatch(trimmed); match != nil {
			idx, _ := strconv.Atoi(match[1])
			named, known := lookup[strings.ToLower(strings.TrimSpace(match[2]))]
			if known && idx >= 1 && idx <= len(opts) && named == lookup[strconv.Itoa(idx)] {
				answers = append(answers, named)
				continue
			}
		}
		if allowAny {
			answers = append(answers, trimmed)
			continue
		}
		return tools.AskUserResponse{}, false
	}

	if len(answers) == 0 {
		if allowAny && strings.TrimSpace(resp.Free) != "" {
			return tools.AskUserResponse{Free: strings.TrimSpace(resp.Free)}, true
		}
		return tools.AskUserResponse{}, false
	}

	if qtype == "single_choice" && len(answers) > 1 {
		answers = answers[:1]
	}

	return tools.AskUserResponse{Answers: answers, Free: strings.TrimSpace(resp.Free)}, true
}
