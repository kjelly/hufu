package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var ErrIncompleteOutput = errors.New("incomplete distillation output")
var ErrCallBudget = errors.New("distillation model call budget exhausted")

func incompleteDraftJSON(err error) bool {
	issue, ok := errors.AsType[*OutputViolation](err)
	return ok && issue.Code == "invalid_json" && errors.Is(issue, io.ErrUnexpectedEOF)
}

// Checkpoint contains either a validated complete draft or a deterministic
// split hint. Partial model responses are never accepted as checkpoints.
type Checkpoint struct {
	Split bool   `json:"split"`
	Draft []byte `json:"draft"`
}

// CheckpointStore is bound by the caller to the frozen source, destination,
// model, execution policy and generation profile, before it is supplied here.
type CheckpointStore interface {
	LoadDraftCheckpoint(context.Context, string) (Checkpoint, bool, error)
	SaveDraftCheckpoint(context.Context, string, Checkpoint) error
}

type draftRunner struct {
	generator    TextGenerator
	cache        CheckpointStore
	remaining    int
	splitContent bool
}

func newDraftRunner(generator TextGenerator, calls int) *draftRunner {
	cache, _ := generator.(CheckpointStore)
	return &draftRunner{generator: generator, cache: cache, remaining: calls, splitContent: true}
}

func (r *draftRunner) generate(ctx context.Context, sources []Source, policy string, depth int) (Draft, error) {
	if err := ctx.Err(); err != nil {
		return Draft{}, err
	}
	if r.generator == nil {
		return Draft{}, fmt.Errorf("%w: model unavailable", ErrInvalid)
	}
	prompt, err := draftPrompt(sources, policy)
	if err != nil {
		return Draft{}, err
	}
	legacy := Digest(struct {
		Prompt       string
		SplitContent bool
	}{prompt, r.splitContent})
	key := legacy
	if !r.splitContent {
		key = Digest([]string{legacy, "preserve_selected_facts_v1"})
	}
	if draft, found, err := r.cached(ctx, key, legacy, sources, policy, depth); err != nil || found {
		return draft, err
	}
	modelPrompt, err := requestPrompt(prompt, sources, "", !r.splitContent)
	if err != nil {
		return Draft{}, err
	}
	draft, err := r.call(ctx, modelPrompt, sources)
	if errors.Is(err, errModelContract) && !incompleteDraftJSON(err) {
		draft, err = r.correctDraft(ctx, prompt, sources, err)
	}
	if err == nil {
		return r.save(ctx, key, draft)
	}
	if errors.Is(err, ErrTransport) {
		return Draft{}, err
	}
	if !errors.Is(err, ErrIncompleteOutput) && !incompleteDraftJSON(err) {
		return Draft{}, err
	}
	if _, _, ok := r.splitSources(sources, depth); !ok {
		return Draft{}, fmt.Errorf("%w: bounded source splitting exhausted: %w", ErrIncompleteOutput, err)
	}
	if r.cache != nil {
		if saveErr := r.cache.SaveDraftCheckpoint(ctx, key, Checkpoint{Split: true}); saveErr != nil {
			return Draft{}, saveErr
		}
	}
	return r.split(ctx, sources, policy, depth, key)
}

func (r *draftRunner) cached(ctx context.Context, key, legacy string, sources []Source, policy string, depth int) (Draft, bool, error) {
	if r.cache == nil {
		return Draft{}, false, nil
	}
	keys := []string{key}
	if legacy != key {
		keys = append(keys, legacy)
	}
	for _, candidate := range keys {
		checkpoint, found, err := r.cache.LoadDraftCheckpoint(ctx, candidate)
		if err != nil {
			return Draft{}, false, err
		}
		if !found {
			continue
		}
		if checkpoint.Split {
			if len(checkpoint.Draft) != 0 {
				return Draft{}, false, fmt.Errorf("%w: split checkpoint contains a draft", ErrInvalid)
			}
			draft, err := r.split(ctx, sources, policy, depth, key)
			return draft, true, err
		}
		draft, err := decodeDraft(string(checkpoint.Draft), sources)
		if err == nil {
			err = r.validateStage(draft)
		}
		if issue, ok := errors.AsType[*OutputViolation](err); ok && candidate != key && issue.Code == "merge_discard" {
			// Old runners cached candidates before checking this merge rule.
			// Keep the rejected legacy record immutable; use the new stage key.
			continue
		}
		return draft, true, err
	}
	return Draft{}, false, nil
}

func (r *draftRunner) call(ctx context.Context, prompt string, sources []Source) (Draft, error) {
	raw, err := r.generateText(ctx, prompt)
	if err != nil {
		return Draft{}, err
	}
	if len(raw) > MaxOutputBytes {
		return Draft{}, fmt.Errorf("%w: %w: per-call output exceeds %d bytes", ErrInvalid, ErrIncompleteOutput, MaxOutputBytes)
	}
	draft, err := decodeModelDraft(raw, sources)
	if err != nil {
		if issue, ok := errors.AsType[*OutputViolation](err); ok && issue.Code == "schema_violation" {
			issue.rewrite = knowledgeRewriteCandidate(raw, sources)
		}
		return Draft{}, withRejectedCandidate(err, raw)
	}
	if err := r.validateStage(draft); err != nil {
		return Draft{}, withRejectedCandidate(err, raw)
	}
	return draft, nil
}

func (r *draftRunner) validateStage(draft Draft) error {
	if !r.splitContent {
		for i, decision := range draft.Decisions {
			if decision.Action == "discard" {
				return violation("merge_discard", fmt.Sprintf("decisions[%d].action", i), nil)
			}
		}
	}
	return nil
}

func (r *draftRunner) split(ctx context.Context, sources []Source, policy string, depth int, key string) (Draft, error) {
	parts, owners, ok := r.splitSources(sources, depth)
	if !ok {
		return Draft{}, fmt.Errorf("%w: invalid split checkpoint", ErrInvalid)
	}
	var drafts []Draft
	childPolicy := policy
	if len(sources) == 1 {
		childPolicy = "These are partial UTF-8 fragments of one source, possibly split inside JSON or text. Do not infer missing context; retain ambiguous or conflicting fragments. Operator policy: " + policy
	}
	for _, part := range parts {
		draft, err := r.generate(ctx, part, childPolicy, depth+1)
		if err != nil {
			return Draft{}, err
		}
		drafts = append(drafts, draft)
	}
	draft, err := aggregateSplitDrafts(sources, drafts, owners)
	if err != nil {
		return Draft{}, err
	}
	return r.save(ctx, key, draft)
}

func (r *draftRunner) splitSources(sources []Source, depth int) ([][]Source, map[string]string, bool) {
	// A selected fact must remain indivisible during consolidation: child
	// decisions must not hide a discard of part of that fact.
	if !r.splitContent && len(sources) == 1 {
		return nil, nil, false
	}
	return splitDraftSources(sources, depth)
}

func (r *draftRunner) save(ctx context.Context, key string, draft Draft) (Draft, error) {
	if r.cache != nil {
		raw, err := json.Marshal(draft)
		if err != nil {
			return Draft{}, err
		}
		if err = r.cache.SaveDraftCheckpoint(ctx, key, Checkpoint{Draft: raw}); err != nil {
			return Draft{}, err
		}
	}
	return draft, nil
}

func splitDraftSources(sources []Source, depth int) ([][]Source, map[string]string, bool) {
	if depth >= 3 || len(sources) == 0 {
		return nil, nil, false
	}
	owners := map[string]string{}
	if len(sources) > 1 {
		for _, source := range sources {
			owners[source.ID] = source.ID
		}
		half := len(sources) / 2
		return [][]Source{sources[:half], sources[half:]}, owners, true
	}
	source := sources[0]
	if len(source.Content) < 2048 {
		return nil, nil, false
	}
	half := len(source.Content) / 2
	for half > 0 && !utf8.RuneStart(source.Content[half]) {
		half--
	}
	left, right := source, source
	left.ID, right.ID = source.ID+"/0", source.ID+"/1"
	left.Content, right.Content = source.Content[:half], source.Content[half:]
	owners[left.ID], owners[right.ID] = source.ID, source.ID
	return [][]Source{{left}, {right}}, owners, true
}

func aggregateSplitDrafts(sources []Source, drafts []Draft, owners map[string]string) (Draft, error) {
	result := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
	actions := map[string]string{}
	for _, draft := range drafts {
		for _, decision := range draft.Decisions {
			id := owners[decision.SourceID]
			if actions[id] == "" || decision.Action == "retain" || actions[id] == "discard" {
				actions[id] = decision.Action
			}
		}
		for _, knowledge := range draft.Knowledge {
			ids := []string{}
			for _, id := range knowledge.SourceIDs {
				ids = append(ids, owners[id])
			}
			result.Knowledge = append(result.Knowledge, Knowledge{Text: knowledge.Text, SourceIDs: ids})
		}
	}
	for _, source := range sources {
		result.Decisions = append(result.Decisions, Decision{SourceID: source.ID, Action: actions[source.ID], Reason: "Combined complete sub-batches; a retained part retains the entire source."})
	}
	filtered := []Knowledge{}
	for _, knowledge := range result.Knowledge {
		ids := []string{}
		for _, id := range knowledge.SourceIDs {
			if actions[id] == "distill" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			knowledge.SourceIDs = ids
			filtered = append(filtered, knowledge)
		}
	}
	result.Knowledge = mergeExactKnowledge(filtered)
	if err := Validate(result, sources); err != nil {
		return Draft{}, err
	}
	return result, nil
}
