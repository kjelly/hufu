package team

import (
	"strings"
)

// ResumeInstruction opens the prompt `hufu resume` sends. It asks the team to
// continue rather than stating what was requested.
const ResumeInstruction = "Resume the existing session from its durable checkpoint."

// maxOriginalRequestRunes bounds the request copied into a worker prompt.
const maxOriginalRequestRunes = 16000

// originalRequestFor returns the request this run is working on when the team
// lists agentName in delegation.share-request-with. A resumed run's own prompt
// only asks to resume, so the request is then the latest earlier user message.
func (c *Coordinator) originalRequestFor(agentName string) string {
	if c == nil || c.session == nil {
		return ""
	}
	shared := false
	for _, name := range c.session.Config.Delegation.ShareRequestWith {
		if strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(agentName)) {
			shared = true
			break
		}
	}
	if !shared {
		return ""
	}
	request := strings.TrimSpace(c.initialPrompt)
	if !strings.HasPrefix(request, ResumeInstruction) {
		return request
	}
	request = ""
	c.viewSessionData(func(sd *SessionData) {
		for i := len(sd.Entries) - 1; i >= 0; i-- {
			entry := sd.Entries[i]
			if entry.Role == "user" && !strings.HasPrefix(strings.TrimSpace(entry.Content), ResumeInstruction) {
				request = strings.TrimSpace(entry.Content)
				return
			}
		}
	})
	return request
}

// originalRequestContextItem renders the run's request as reference context
// for a worker. The task the coordinator wrote stays authoritative; the
// request lets the worker notice when that task left out or contradicted
// something the user explicitly asked for, and say so.
func originalRequestContextItem(request string) ContextItem {
	if runes := []rune(request); len(runes) > maxOriginalRequestRunes {
		request = string(runes[:maxOriginalRequestRunes]) + "\n\n[request truncated]"
	}
	content := "## Original Request (reference)\n\n" +
		"The coordinator scoped your task from this request. Your task's goal and constraints define what you must do; " +
		"use the request to resolve ambiguity in them. If your task leaves out or contradicts something the request " +
		"explicitly asks for within your task's area, report it in your result instead of silently following either one.\n\n" +
		request
	return ContextItem{
		ID: "original_request", Kind: "original_request", Content: content, Source: "user",
		Priority: PriorityHardConstraints, Required: true, Authority: ContextAuthorityHistorical,
		DedupKey: hashContentKey(request),
	}
}
