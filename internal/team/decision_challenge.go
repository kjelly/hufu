package team

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Challenge and premortem stages
// (docs/hufu-decision-aware-runtime-spec.md §23, §24).
//
// Challenge runs only after deterministic aggregation, sees anonymized
// opinions, and cannot mutate the sealed evidence. Judge identity is withheld
// by default so the challenger attacks the argument rather than its author.

// ChallengeRequest is one challenger dispatch.
type ChallengeRequest struct {
	DecisionID   string
	ChallengerID string
	Packet       DecisionEvidencePacket
	Aggregate    DecisionAggregate
	Prompt       string
}

// ChallengeRunner executes one challenger.
type ChallengeRunner interface {
	RunChallenge(ctx context.Context, req ChallengeRequest) (DecisionChallenge, error)
}

// PremortemRequest is one premortem dispatch. It runs before evidence is
// sealed, so it sees the candidate options rather than an aggregate.
type PremortemRequest struct {
	DecisionID string
	Question   string
	Options    []DecisionOption
	Prompt     string
}

// PremortemRunner executes the premortem stage.
type PremortemRunner interface {
	RunPremortem(ctx context.Context, req PremortemRequest) (PremortemResult, error)
}

// anonymizeOpinions replaces judge IDs with stable positional aliases. The
// mapping stays in the event log; it never reaches the challenger (spec §23).
func anonymizeOpinions(opinions []DecisionOpinion) ([]DecisionOpinion, map[string]string) {
	valid := make([]DecisionOpinion, 0, len(opinions))
	for _, opinion := range opinions {
		if opinion.Valid {
			valid = append(valid, opinion)
		}
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].JudgeID < valid[j].JudgeID })

	aliases := make(map[string]string, len(valid))
	anonymized := make([]DecisionOpinion, 0, len(valid))
	for i, opinion := range valid {
		alias := fmt.Sprintf("judge-%c", 'a'+rune(i%26))
		aliases[opinion.JudgeID] = alias
		copied := opinion
		copied.JudgeID = alias
		copied.ID = ""
		anonymized = append(anonymized, copied)
	}
	return anonymized, aliases
}

// BuildChallengePrompt renders the challenger's input from sealed evidence, the
// deterministic aggregate and anonymized opinions.
func BuildChallengePrompt(packet DecisionEvidencePacket, aggregate DecisionAggregate, opinions []DecisionOpinion) (string, map[string]string) {
	anonymized, aliases := anonymizeOpinions(opinions)

	var b strings.Builder
	b.WriteString("You are challenging a candidate decision. Attack the strongest version of it.\n")
	b.WriteString("You are seeing anonymized judgments on purpose: argue with the reasoning, not with who made it.\n")
	b.WriteString("You cannot change the sealed evidence.\n\n")
	fmt.Fprintf(&b, "Evidence hash: %s\n\n", packet.Hash)
	b.WriteString(renderSealedEvidence(packet))

	b.WriteString("## Aggregate (computed by the runtime, not by a model)\n")
	for _, id := range packet.OptionIDs() {
		mean, ok := aggregate.MeanScores[id]
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "- %s: mean %.4f, median %.4f, min %.4f, max %.4f, stddev %.4f\n",
			id, mean, aggregate.MedianScores[id], aggregate.MinScores[id],
			aggregate.MaxScores[id], aggregate.StdDev[id])
	}
	fmt.Fprintf(&b, "Leading option: %s\n\n", aggregate.PreferredOption)

	b.WriteString("## Anonymized judgments\n")
	for _, opinion := range anonymized {
		fmt.Fprintf(&b, "- %s prefers %s (p=%.4f)\n", opinion.JudgeID, opinion.PreferredOption, opinion.SuccessProbability)
		for _, note := range opinion.DisconfirmingEvidence {
			fmt.Fprintf(&b, "  disconfirming: %s\n", strings.TrimSpace(note))
		}
		for _, note := range opinion.MissingInformation {
			fmt.Fprintf(&b, "  missing: %s\n", strings.TrimSpace(note))
		}
	}
	b.WriteString("\n")

	b.WriteString("## Required response\nReturn a single JSON object, no prose around it:\n")
	b.WriteString("{\n")
	b.WriteString(`  "target_option": "...",` + "\n")
	b.WriteString(`  "strongest_countercase": "...",` + "\n")
	b.WriteString(`  "fragile_assumptions": ["..."],` + "\n")
	b.WriteString(`  "missing_evidence": ["..."],` + "\n")
	b.WriteString(`  "falsification_tests": ["..."],` + "\n")
	b.WriteString(`  "severity": 0.0` + "\n")
	b.WriteString("}\n")
	b.WriteString("severity is 0-1, where 1 means the leading option should not be taken.\n")
	return b.String(), aliases
}

// ValidateChallenge checks a challenger's structured output against the sealed
// evidence. A challenger that names an option outside the packet is attacking
// something that was never on the table.
func ValidateChallenge(challenge *DecisionChallenge, packet DecisionEvidencePacket) error {
	if challenge == nil {
		return fmt.Errorf("nil challenge")
	}
	if challenge.TargetOption != "" && !packet.HasOption(challenge.TargetOption) {
		return fmt.Errorf("target_option %q is not an option in the sealed evidence", challenge.TargetOption)
	}
	if !validUnit(challenge.Severity) {
		return fmt.Errorf("severity %v is outside [0, 1] or not finite", challenge.Severity)
	}
	challenge.Severity = roundDecision(challenge.Severity)
	challenge.EvidenceHash = packet.Hash
	return nil
}

// BuildPremortemPrompt renders the premortem input.
func BuildPremortemPrompt(question string, options []DecisionOption) string {
	var b strings.Builder
	b.WriteString("Assume this decision was taken and it failed. Explain how.\n")
	b.WriteString("You are discovering risk, not rejecting an option: name failure modes, their early warning signals, and what would mitigate them.\n\n")
	fmt.Fprintf(&b, "## Question\n%s\n\n", strings.TrimSpace(question))

	ordered := append([]DecisionOption(nil), options...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	b.WriteString("## Options under consideration\n")
	for _, option := range ordered {
		fmt.Fprintf(&b, "- %s (%s): %s\n", option.ID, option.Kind, strings.TrimSpace(option.Title))
	}
	b.WriteString("\n")

	b.WriteString("## Required response\nReturn a single JSON object, no prose around it:\n")
	b.WriteString("{\n")
	b.WriteString(`  "assumed_outcome": "failure",` + "\n")
	b.WriteString(`  "failure_modes": [{` + "\n")
	b.WriteString(`    "id": "F1", "description": "...",` + "\n")
	b.WriteString(`    "likelihood": 0.0, "impact": 0.0,` + "\n")
	b.WriteString(`    "early_warning_signals": ["..."], "mitigations": ["..."]` + "\n")
	b.WriteString("  }]\n}\n")
	b.WriteString("likelihood and impact are 0-1.\n")
	return b.String()
}

// ValidatePremortem checks the premortem's structured output.
func ValidatePremortem(result *PremortemResult) error {
	if result == nil {
		return fmt.Errorf("nil premortem result")
	}
	if len(result.FailureModes) == 0 {
		return fmt.Errorf("premortem produced no failure modes")
	}
	seen := make(map[string]bool, len(result.FailureModes))
	for i := range result.FailureModes {
		mode := &result.FailureModes[i]
		if strings.TrimSpace(mode.ID) == "" {
			mode.ID = fmt.Sprintf("F%d", i+1)
		}
		if seen[mode.ID] {
			return fmt.Errorf("failure_modes[%d] repeats id %q", i, mode.ID)
		}
		seen[mode.ID] = true
		if strings.TrimSpace(mode.Description) == "" {
			return fmt.Errorf("failure_modes[%d] has no description", i)
		}
		if !validUnit(mode.Likelihood) || !validUnit(mode.Impact) {
			return fmt.Errorf("failure_modes[%d] likelihood/impact must be in [0, 1]", i)
		}
		mode.Likelihood = roundDecision(mode.Likelihood)
		mode.Impact = roundDecision(mode.Impact)
	}
	sort.Slice(result.FailureModes, func(i, j int) bool { return result.FailureModes[i].ID < result.FailureModes[j].ID })
	if strings.TrimSpace(result.AssumedOutcome) == "" {
		result.AssumedOutcome = "failure"
	}
	return nil
}

// PremortemFalsifications collects the early warning signals a premortem found,
// which are exactly the conditions that would tell us the plan is going wrong.
func PremortemFalsifications(result *PremortemResult, challenges []DecisionChallenge) []string {
	seen := map[string]bool{}
	var out []string
	add := func(values []string) {
		for _, value := range values {
			trimmed := strings.TrimSpace(value)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	if result != nil {
		for _, mode := range result.FailureModes {
			add(mode.EarlyWarningSignals)
		}
	}
	for _, challenge := range challenges {
		add(challenge.FalsificationTests)
	}
	sort.Strings(out)
	return out
}
