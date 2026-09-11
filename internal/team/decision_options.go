package team

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Option proposal (docs/architecture/decision-runtime.md §19.1).
//
// Hand-writing three or more options per decision task is usable but tedious.
// The proposal stage produces candidates from the task's goal instead.
//
// The risk it introduces is precise: if the judgment under scrutiny also
// decides what counts as an alternative, the no-go gate checks nothing — a
// proposer that never proposes "do nothing" passes it every time. The answer
// here is not to trust the proposer and block when it forgets, which only
// produces retry loops. The runtime instead injects the required alternatives
// deterministically, so "do nothing" really is on the table, really is scored
// by every judge, and really can win. Every option then carries its origin, so
// nobody later mistakes an injected option for one somebody thought of.

// maxProposalTitleRunes bounds a proposed option's title so one verbose
// proposal cannot dominate every judge's context.
const maxProposalTitleRunes = 120

// OptionProposalRequest is one proposal dispatch.
type OptionProposalRequest struct {
	DecisionID string
	Question   string
	MaxOptions int
	Prompt     string
}

// OptionProposer produces candidate options for a decision.
type OptionProposer interface {
	ProposeOptions(ctx context.Context, req OptionProposalRequest) ([]DecisionOption, error)
}

// BuildOptionProposalPrompt renders the proposer's input. The proposer sees
// the question and nothing else: no preference, no judge, no earlier decision.
func BuildOptionProposalPrompt(question string, maxOptions int) string {
	var b strings.Builder
	b.WriteString("Propose the distinct courses of action worth weighing for the decision below.\n")
	b.WriteString("You are not choosing between them and you are not ranking them. Independent judges will score whatever you propose.\n")
	b.WriteString("Propose genuinely different approaches, not restatements of one approach.\n\n")
	fmt.Fprintf(&b, "## Decision\n%s\n\n", strings.TrimSpace(question))

	b.WriteString("## Option kinds\n")
	b.WriteString("- execute: do the work as framed\n")
	b.WriteString("- reduce_scope: do a smaller version\n")
	b.WriteString("- negotiate: change the terms with whoever set them\n")
	b.WriteString("- request_information: gather what is missing before committing\n")
	b.WriteString("- defer: do nothing for now\n")
	b.WriteString("- abandon: do not do this at all\n")
	b.WriteString("- custom: anything the kinds above do not cover\n\n")

	b.WriteString("## Required response\nReturn a single JSON object, no prose around it:\n")
	b.WriteString("{\n")
	b.WriteString(`  "options": [{"id": "short-slug", "kind": "execute", "title": "...", "description": "..."}]` + "\n")
	b.WriteString("}\n")
	fmt.Fprintf(&b, "Propose at most %d options. ids must be short, lowercase, unique slugs.\n", maxOptions)
	return b.String()
}

// NormalizeProposedOptions validates and canonicalizes a proposer's output.
// Malformed entries are dropped rather than repaired: an option nobody can
// identify is worse than one fewer option.
func NormalizeProposedOptions(proposed []DecisionOption, maxOptions int) []DecisionOption {
	seen := make(map[string]bool, len(proposed))
	out := make([]DecisionOption, 0, len(proposed))
	for _, option := range proposed {
		id := slugifyOptionID(option.ID)
		if id == "" || seen[id] {
			continue
		}
		kind := option.Kind
		if !kind.Valid() {
			kind = OptionCustom
		}
		title := strings.TrimSpace(option.Title)
		if runes := []rune(title); len(runes) > maxProposalTitleRunes {
			title = string(runes[:maxProposalTitleRunes])
		}
		seen[id] = true
		out = append(out, DecisionOption{
			ID:          id,
			Kind:        kind,
			Origin:      OptionOriginProposed,
			Title:       title,
			Description: strings.TrimSpace(option.Description),
		})
		if maxOptions > 0 && len(out) >= maxOptions {
			break
		}
	}
	return out
}

// slugifyOptionID reduces a proposed ID to a stable lowercase slug. Option IDs
// are sort keys for every deterministic computation, so they cannot carry
// whitespace or case that would reorder the aggregate.
func slugifyOptionID(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(raw)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	return slug
}

// runtimeInjectedOptions are the canonical alternatives the runtime adds when
// a policy requires them and nothing supplied one.
var (
	runtimeDeferOption = DecisionOption{
		ID:     "runtime-defer",
		Kind:   OptionDefer,
		Origin: OptionOriginRuntime,
		Title:  "Do nothing for now",
		Description: "Take no action at this time. The runtime added this option because the " +
			"profile requires a no-action alternative to be on the table when judgment happens.",
	}
	runtimeRequestInfoOption = DecisionOption{
		ID:     "runtime-request-information",
		Kind:   OptionRequestInfo,
		Origin: OptionOriginRuntime,
		Title:  "Gather missing information first",
		Description: "Do not commit yet; find out what is missing. The runtime added this option " +
			"because the profile requires an information-gathering alternative.",
	}
)

// EnsureRequiredAlternatives injects the alternatives the policy requires when
// the option set lacks them. This is what keeps the no-go gate meaningful once
// options can be proposed rather than declared (spec §19.1).
//
// It returns the resulting option set and the IDs it injected, so the caller
// can record that they were not proposed by anyone.
func EnsureRequiredAlternatives(options []DecisionOption, policy AlternativesPolicy) ([]DecisionOption, []string) {
	out := append([]DecisionOption(nil), options...)
	var injected []string

	has := func(match func(DecisionOption) bool) bool {
		for _, option := range out {
			if match(option) {
				return true
			}
		}
		return false
	}

	if policy.RequireNoActionOption && !has(func(o DecisionOption) bool { return o.Kind.IsNoGo() }) {
		out = append(out, runtimeDeferOption)
		injected = append(injected, runtimeDeferOption.ID)
	}
	if policy.RequireInfoOption && !has(func(o DecisionOption) bool { return o.Kind == OptionRequestInfo }) {
		out = append(out, runtimeRequestInfoOption)
		injected = append(injected, runtimeRequestInfoOption.ID)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, injected
}

// proposeOptions runs the proposal stage for a decision, reusing a durable
// proposal on resume. Options are material evidence, so re-proposing would
// produce a different evidence hash and invalidate the whole round (§15.4).
func (e *decisionEngine) proposeOptions(ctx context.Context, req DecisionRequest, policy DecisionPolicy, state decisionState) ([]DecisionOption, error) {
	// A task that declared its own options is authoritative; the proposal
	// stage never second-guesses a written contract. They are still stamped
	// with their provenance so no option in a record has an unknown origin.
	if len(req.Options) > 0 {
		return stampDeclaredOrigin(req.Options), nil
	}
	if !policy.OptionProposal.Enabled {
		return nil, &GateResult{
			Reason: ReasonDecisionMissingAlternative,
			Detail: "the task declares no options and option-proposal is not enabled for this profile",
		}
	}
	if len(state.ProposedOptions) > 0 {
		return state.ProposedOptions, nil
	}
	if e.services.Proposer == nil {
		return nil, &GateResult{
			Reason: ReasonDecisionMissingAlternative,
			Detail: "option-proposal is enabled but no proposer is configured",
		}
	}

	maxOptions := policy.OptionProposal.EffectiveMaxOptions()
	proposed, err := e.services.Proposer.ProposeOptions(ctx, OptionProposalRequest{
		DecisionID: req.DecisionID,
		Question:   req.Question,
		MaxOptions: maxOptions,
		Prompt:     BuildOptionProposalPrompt(req.Question, maxOptions),
	})
	if err != nil {
		return nil, fmt.Errorf("decision %s option proposal: %w", req.DecisionID, err)
	}

	options := NormalizeProposedOptions(proposed, maxOptions)
	options, injected := EnsureRequiredAlternatives(options, policy.Discipline.Alternatives)
	if len(options) == 0 {
		return nil, &GateResult{
			Reason: ReasonDecisionMissingAlternative,
			Detail: "the proposal stage produced no usable options",
		}
	}

	reason := fmt.Sprintf("%d options proposed", len(options)-len(injected))
	if len(injected) > 0 {
		reason += fmt.Sprintf("; runtime injected %s to satisfy the alternatives policy", strings.Join(injected, ", "))
	}
	if err := appendDecisionEvent(ctx, e.services.Journal, EventDecisionOptionsProposed, decisionEvent{
		DecisionID: req.DecisionID, Reason: reason, Options: options,
	}); err != nil {
		return nil, err
	}
	return options, nil
}

// stampDeclaredOrigin marks contract-declared options as such.
func stampDeclaredOrigin(options []DecisionOption) []DecisionOption {
	out := make([]DecisionOption, 0, len(options))
	for _, option := range options {
		if option.Origin == "" {
			option.Origin = OptionOriginDeclared
		}
		out = append(out, option)
	}
	return out
}
