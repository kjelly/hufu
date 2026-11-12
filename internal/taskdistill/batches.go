package taskdistill

import (
	"context"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const DefaultMaxModelCalls = 256
const MaxModelCalls = 4096

// PrepareBatches preflights the complete extraction before provider admission.
// Fragments are opaque data, with stable runtime-owned source identities. No
// evidence is truncated and no prose is interpreted by deterministic code.
func PrepareBatches(sources []Source, policy string, maxCalls int) ([][]Source, error) {
	if maxCalls < 1 || maxCalls > MaxModelCalls {
		return nil, fmt.Errorf("%w: model call limit must be between 1 and %d", ErrInvalid, MaxModelCalls)
	}
	if len(sources) == 0 || len(sources) > 100 {
		return nil, fmt.Errorf("%w: select between 1 and 100 sources", ErrInvalid)
	}
	seen := map[string]bool{}
	total := len(policy)
	for _, source := range sources {
		if source.ID == "" || seen[source.ID] || !utf8.ValidString(source.Content) {
			return nil, fmt.Errorf("%w: invalid source identity or encoding", ErrInvalid)
		}
		seen[source.ID] = true
		total += len(source.Content) + len(source.ID)
	}
	if total > MaxSourceBytes || len(policy) > MaxInputBytes/4 {
		return nil, fmt.Errorf("%w: extraction exceeds %d bytes or policy exceeds %d bytes; source retained", ErrInvalid, MaxSourceBytes, MaxInputBytes/4)
	}
	if inputSize(sources, policy) <= MaxInputBytes {
		return [][]Source{sources}, nil
	}
	var batches [][]Source
	var batch []Source
	for index, source := range sources {
		remaining := source.Content
		for part := 0; ; part++ {
			// JSON escaping can expand each input byte sixfold. Determine the
			// actual encoded fit instead of assuming characters equal bytes.
			n := min(len(remaining), MaxInputBytes/2)
			fragment := Source{ID: fmt.Sprintf("fragment_%d_%d", index, part), Protected: source.Protected}
			for {
				for n > 0 && n < len(remaining) && !utf8.RuneStart(remaining[n]) {
					n--
				}
				fragment.Content = remaining[:n]
				if inputSize([]Source{fragment}, policy) <= MaxInputBytes-1024 {
					break
				}
				n /= 2
				if n == 0 {
					return nil, fmt.Errorf("%w: policy leaves no fragment capacity", ErrInvalid)
				}
			}
			candidate := append(slices.Clone(batch), fragment)
			if len(candidate) > 100 || inputSize(candidate, policy) > MaxInputBytes-1024 {
				batches = append(batches, batch)
				batch = nil
			}
			batch = append(batch, fragment)
			remaining = remaining[n:]
			if remaining == "" {
				break
			}
		}
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	// Reserve at least one call for merging selected facts.
	if len(batches) >= maxCalls {
		return nil, fmt.Errorf("%w: extraction needs %d calls plus knowledge merging, exceeding the %d-call budget; use --max-model-calls to explicitly increase the budget (maximum %d); source retained", ErrInvalid, len(batches), maxCalls, MaxModelCalls)
	}
	return batches, nil
}

func inputSize(sources []Source, policy string) int {
	raw, _ := json.Marshal(struct {
		Sources []Source `json:"sources"`
		Policy  string   `json:"policy"`
	}{sources, policy})
	return len(raw)
}

// GenerateBatched keeps model calls bounded while retaining decisions and
// provenance for the original sources. Any retained fragment retains its whole
// source. A failure at any stage returns no applicable proposal.
func GenerateBatched(ctx context.Context, generator TextGenerator, sources []Source, policy string, maxCalls int) (Draft, error) {
	batches, err := PrepareBatches(sources, policy, maxCalls)
	if err != nil {
		return Draft{}, err
	}
	if len(batches) == 1 && slices.EqualFunc(batches[0], sources, func(a, b Source) bool { return a == b }) {
		return newDraftRunner(generator, maxCalls).generate(ctx, sources, policy, 0)
	}
	runner := newDraftRunner(generator, maxCalls)
	owners := map[string]string{}
	for index := range sources {
		prefix := fmt.Sprintf("fragment_%d_", index)
		for _, batch := range batches {
			for _, fragment := range batch {
				if strings.HasPrefix(fragment.ID, prefix) {
					owners[fragment.ID] = sources[index].ID
				}
			}
		}
	}
	result := Draft{Decisions: []Decision{}, Knowledge: []Knowledge{}}
	actions := map[string]string{}
	fragmentPolicy := extractionPolicy(sources, batches, policy)
	// Account for this fixed guidance in the preflight, before the first call.
	for _, batch := range batches {
		if inputSize(batch, fragmentPolicy) > MaxInputBytes {
			return Draft{}, fmt.Errorf("%w: fragment guidance exceeds call budget", ErrInvalid)
		}
	}
	for i, batch := range batches {
		draft, err := runner.generate(ctx, batch, fragmentPolicy, 0)
		if err != nil {
			return Draft{}, fmt.Errorf("extract batch %d/%d: %w", i+1, len(batches), err)
		}
		for _, decision := range draft.Decisions {
			id := owners[decision.SourceID]
			if actions[id] == "" || decision.Action == "retain" || actions[id] == "discard" {
				actions[id] = decision.Action
			}
		}
		for _, knowledge := range draft.Knowledge {
			for i, id := range knowledge.SourceIDs {
				knowledge.SourceIDs[i] = owners[id]
			}
			result.Knowledge = append(result.Knowledge, knowledge)
		}
	}
	for _, source := range sources {
		result.Decisions = append(result.Decisions, Decision{source.ID, actions[source.ID], "Aggregated fragment decisions; any retained fragment retains the complete source. See model call evidence in the draft workspace."})
	}
	for i := range result.Knowledge {
		result.Knowledge[i].SourceIDs = slices.DeleteFunc(result.Knowledge[i].SourceIDs, func(id string) bool { return actions[id] != "distill" })
	}
	result.Knowledge = slices.DeleteFunc(result.Knowledge, func(k Knowledge) bool { return len(k.SourceIDs) == 0 })
	result.Knowledge = mergeExactKnowledge(result.Knowledge)
	result.Knowledge, err = consolidateKnowledge(ctx, runner, result.Knowledge, policy)
	if err != nil {
		return Draft{}, err
	}
	if err = Validate(result, sources); err != nil {
		return Draft{}, err
	}
	return result, nil
}

func mergeExactKnowledge(items []Knowledge) []Knowledge {
	result := []Knowledge{}
	indices := map[string]int{}
	for _, item := range items {
		index, exists := indices[item.Text]
		if exists {
			result[index].SourceIDs = append(result[index].SourceIDs, item.SourceIDs...)
		} else {
			indices[item.Text] = len(result)
			item.SourceIDs = slices.Clone(item.SourceIDs)
			result = append(result, item)
		}
	}
	for i := range result {
		slices.Sort(result[i].SourceIDs)
		result[i].SourceIDs = slices.Compact(result[i].SourceIDs)
	}
	return result
}

// Every selected fact must survive this pass: retain copies it verbatim,
// distill requires schema-validated coverage, and discard is rejected. Only
// the model judges semantic equivalence and preserves conflicting claims.
func consolidateKnowledge(ctx context.Context, runner *draftRunner, items []Knowledge, policy string) ([]Knowledge, error) {
	runner.splitContent = false
	defer func() { runner.splitContent = true }()
	if len(items) < 2 {
		return items, nil
	}
	mergePolicy := "All sources are already selected durable facts. Merge duplicates while preserving applicability, failures and conflicts. You MUST NOT discard any source: use distill for a covered fact, or retain to keep its exact text. Operator policy: " + policy
	var batches [][]Source
	var batch []Source
	owners := map[string]Knowledge{}
	for index, item := range items {
		source := Source{ID: fmt.Sprintf("fact_%d", index), Content: item.Text}
		if inputSize([]Source{source}, mergePolicy) > MaxInputBytes {
			return nil, fmt.Errorf("%w: fact exceeds merge budget", ErrInvalid)
		}
		candidate := append(slices.Clone(batch), source)
		if len(candidate) > 100 || inputSize(candidate, mergePolicy) > MaxInputBytes {
			batches = append(batches, batch)
			batch = nil
		}
		batch = append(batch, source)
		owners[source.ID] = item
	}
	batches = append(batches, batch)
	result := []Knowledge{}
	for i, batch := range batches {
		draft, err := runner.generate(ctx, batch, mergePolicy, 0)
		if err != nil {
			return nil, fmt.Errorf("merge batch %d/%d: %w", i+1, len(batches), err)
		}
		for _, decision := range draft.Decisions {
			switch decision.Action {
			case "discard":
				return nil, fmt.Errorf("%w: merge discarded a selected fact", ErrInvalid)
			case "retain":
				result = append(result, owners[decision.SourceID])
			}
		}
		for _, knowledge := range draft.Knowledge {
			var ids []string
			for _, id := range knowledge.SourceIDs {
				ids = append(ids, owners[id].SourceIDs...)
			}
			result = append(result, Knowledge{knowledge.Text, ids})
		}
	}
	return mergeExactKnowledge(result), nil
}
