package team

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

// A deterministic judge for end-to-end decision tests (plan Stage 7.1).
//
// Every decision stage runs on the judge sidecar, so a fixture that wants to
// exercise the whole chain needs one provider that answers all of them. This
// one dispatches on the marker each stage's prompt opens with and returns a
// fixed payload, which makes the run reproducible: the same fixture always
// produces the same aggregate, the same sealed hash, and the same record.
//
// The judge deliberately never varies its answer by call order except where a
// test asks it to. Non-determinism here would be indistinguishable from a
// runtime that aggregates inconsistently.

type fakeJudgeStage string

const (
	stageOptions      fakeJudgeStage = "options"
	stageJudge        fakeJudgeStage = "judge"
	stageChallenge    fakeJudgeStage = "challenge"
	stagePremortem    fakeJudgeStage = "premortem"
	stageRevision     fakeJudgeStage = "revision"
	stageReference    fakeJudgeStage = "reference"
	stageFinalization fakeJudgeStage = "finalization"
	stageUnknown      fakeJudgeStage = "unknown"
)

// classifyDecisionStagePrompt maps a stage prompt to the stage that built it.
// The markers are the opening lines the prompt builders write, so a change to
// a prompt's purpose fails these tests rather than silently answering the
// wrong stage.
func classifyDecisionStagePrompt(prompt string) fakeJudgeStage {
	switch {
	case strings.Contains(prompt, "ReferenceEvidenceDraft"):
		return stageReference
	case strings.Contains(prompt, "forming an independent judgment"):
		return stageJudge
	case strings.Contains(prompt, "revising your own judgment once"):
		return stageRevision
	case strings.Contains(prompt, "You are challenging a candidate decision"):
		return stageChallenge
	case strings.Contains(prompt, "Assume this decision was taken and it failed"):
		return stagePremortem
	case strings.Contains(prompt, "Propose the distinct courses of action"):
		return stageOptions
	case strings.Contains(prompt, "sealed_evidence"):
		return stageFinalization
	default:
		return stageUnknown
	}
}

// fakeJudge answers every decision stage deterministically.
type fakeJudge struct {
	mu      sync.Mutex
	calls   map[fakeJudgeStage]int
	prompts map[fakeJudgeStage][]string

	// preferred is the option every judge scores highest. Tests that want a
	// specific winner set it before the run.
	preferred string
	// dispersion widens the per-judge scores so a profile's challenge
	// threshold is crossed. Zero keeps every judge identical.
	dispersion float64
	// judgeSeq counts judge calls so dispersion can vary them.
	judgeSeq int
}

func newFakeJudge(preferred string) *fakeJudge {
	return &fakeJudge{
		calls:     map[fakeJudgeStage]int{},
		prompts:   map[fakeJudgeStage][]string{},
		preferred: preferred,
	}
}

func (f *fakeJudge) count(stage fakeJudgeStage) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[stage]
}

func (f *fakeJudge) promptsFor(stage fakeJudgeStage) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts[stage]...)
}

// ServeHTTP answers an OpenAI-compatible chat completion.
func (f *fakeJudge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Model    string `json:"model"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, fmt.Sprintf("decode judge request: %v", err), http.StatusBadRequest)
		return
	}
	var prompt strings.Builder
	for _, message := range request.Messages {
		prompt.WriteString(decodeMessageContent(message.Content))
		prompt.WriteString("\n")
	}

	stage := classifyDecisionStagePrompt(prompt.String())
	f.mu.Lock()
	f.calls[stage]++
	f.prompts[stage] = append(f.prompts[stage], prompt.String())
	seq := 0
	if stage == stageJudge {
		seq = f.judgeSeq
		f.judgeSeq++
	}
	preferred, dispersion := f.preferred, f.dispersion
	f.mu.Unlock()

	body := fakeJudgeStageResponse(stage, preferred, dispersion, seq, parsePromptOptionIDs(prompt.String()))
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w,
		`{"id":"fake-judge","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`,
		request.Model, body)
}

// decodeMessageContent accepts both the string and the multi-part array forms
// an OpenAI-compatible client may send.
func decodeMessageContent(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, part := range parts {
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return string(raw)
}

// parsePromptOptionIDs reads the option IDs the prompt actually presented.
// A judge must score every sealed option, so the fake reads them rather than
// assuming a fixture's option set — which also makes it fail loudly if the
// runtime ever stops rendering them.
func parsePromptOptionIDs(prompt string) []string {
	_, rest, found := strings.Cut(prompt, "## Options\n")
	if !found {
		return nil
	}
	var ids []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), " (")
		if !ok {
			continue
		}
		ids = append(ids, strings.TrimSpace(id))
	}
	return ids
}

// scoreEveryOption renders one score per sealed option. The preferred option
// scores highest; the rest are flat, so the aggregate has one unambiguous
// winner and no tie-break to depend on.
func scoreEveryOption(optionIDs []string, preferred string, top float64) string {
	if len(optionIDs) == 0 {
		optionIDs = []string{preferred}
	}
	parts := make([]string, 0, len(optionIDs))
	for _, id := range optionIDs {
		score := 4.0
		if id == preferred {
			score = top
		}
		parts = append(parts, fmt.Sprintf(`{"option_id":%q,"criteria":{"impact":%.4f},"overall":%.4f}`, id, score, score))
	}
	return strings.Join(parts, ",")
}

func fakeJudgeStageResponse(stage fakeJudgeStage, preferred string, dispersion float64, seq int, optionIDs []string) string {
	switch stage {
	case stageOptions:
		return fmt.Sprintf(`{"options":[
{"id":%q,"kind":"execute","title":"Do the work as framed","description":"Execute the change directly."},
{"id":"reduce","kind":"reduce_scope","title":"Do a smaller version","description":"Ship a narrower change first."}]}`,
			preferred)
	case stageJudge:
		// Judge scores are offset by call order only when a test asks for
		// dispersion, so the aggregate is otherwise byte-identical every run.
		offset := dispersion * float64(seq)
		return fmt.Sprintf(`{"option_scores":[%s],
"preferred_option":%q,"success_probability":0.7,"confidence":0.8,
"key_assumptions":["the target service accepts the change"],
"disconfirming_evidence":["the last two attempts timed out"],
"missing_information":[]}`,
			scoreEveryOption(optionIDs, preferred, 8.0-offset), preferred)
	case stageChallenge:
		return fmt.Sprintf(`{"target_option":%q,"strongest_countercase":"the rollback path is untested",
"fragile_assumptions":["the target service accepts the change"],
"missing_evidence":["a rollback rehearsal"],
"falsification_tests":["run the compensating operation against a copy"],
"severity":0.4}`, preferred)
	case stagePremortem:
		return `{"assumed_outcome":"the change was applied and the service degraded",
"failure_modes":[{"id":"fm-1","description":"the bridge is created but never attached",
"likelihood":0.3,"impact":0.7,"early_warning_signals":["probe reports partial"],
"mitigations":["reconcile before retry"]}]}`
	case stageRevision:
		return fmt.Sprintf(`{"revised_scores":[%s],
"revised_probability":0.7,"changed":false,"reason":"the challenge did not move the evidence"}`,
			scoreEveryOption(optionIDs, preferred, 8.0))
	case stageReference:
		return `{"schema_version":1,"entries":[{"reference_class":"infra change rollouts",
"metric":"success rate","sample_size":42,
"distribution":{"mean":0.62,"median":0.65,"p10":0.30,"p90":0.85},
"limitations":["single operator sample"],
"source":{"id":"src-1","source_id":"src-1","source_type":"internal_report",
"name":"rollout history","title":"Rollout history 2026","publisher":"platform team",
"citation":"platform/rollouts-2026","description":"internal rollout outcomes",
"url":"https://example.invalid/rollouts","uri":"https://example.invalid/rollouts",
"locator":"table-3","declared_parent_source_ids":[]}}]}`
	case stageFinalization:
		return fmt.Sprintf(`{"option_id":%q,"reason":"the aggregate and the challenge agree"}`, preferred)
	default:
		return `{}`
	}
}
