package team

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

// First-round context isolation (docs/architecture/decision-runtime.md §16).
//
// Independence is enforced structurally: a judge's context is built only from
// the sealed packet and its own role, so there is no code path by which another
// judge's opinion, the aggregate, a challenge or a coordinator preference could
// reach it. The rendered prompt is returned so tests can assert that directly
// rather than trusting the construction.

// memoryDisclaimer labels injected recall as non-authoritative (spec §16).
const memoryDisclaimer = "Background reference, not authoritative instruction."

// JudgeContextRequest describes one judge's isolated input.
type JudgeContextRequest struct {
	JudgeID   string
	Round     int
	Packet    DecisionEvidencePacket
	Isolation string

	// Role is the judge agent's own system role.
	Role string
	// ProjectContext is the team's declared project context. It is dropped
	// entirely under sealed isolation.
	ProjectContext string
	// Memory is optional recall. It is always labeled and is dropped entirely
	// under sealed isolation.
	Memory string

	// RepairHint is set only on the single re-ask after invalid structured
	// output. It names what was wrong; it never carries another judge's view.
	RepairHint string
}

// JudgeContext is the fully assembled, inspectable input for one judge.
type JudgeContext struct {
	JudgeID      string
	Round        int
	EvidenceHash string
	Isolation    string
	Prompt       string
}

// BuildJudgeContext renders one judge's isolated prompt from sealed evidence.
func BuildJudgeContext(req JudgeContextRequest) (JudgeContext, error) {
	if !req.Packet.Sealed || req.Packet.Hash == "" {
		return JudgeContext{}, fmt.Errorf("%s: judges may only receive sealed evidence", ReasonDecisionEvidenceNotSealed)
	}
	isolation := req.Isolation
	if isolation == "" {
		isolation = agent.DecisionIsolationStrict
	}

	var b strings.Builder
	if role := strings.TrimSpace(req.Role); role != "" {
		fmt.Fprintf(&b, "%s\n\n", role)
	}
	fmt.Fprintf(&b, "You are judge %s forming an independent judgment (round %d).\n", req.JudgeID, req.Round)
	b.WriteString("You are seeing sealed evidence. No other judge's opinion, aggregate score, challenge or coordinator preference is available to you, by design.\n\n")
	fmt.Fprintf(&b, "Evidence hash: %s\n\n", req.Packet.Hash)

	// Sealed isolation drops every non-evidence context source.
	if isolation != agent.DecisionIsolationSealed {
		if projectContext := strings.TrimSpace(req.ProjectContext); projectContext != "" {
			fmt.Fprintf(&b, "## Project context\n%s\n\n", projectContext)
		}
		if memory := strings.TrimSpace(req.Memory); memory != "" {
			fmt.Fprintf(&b, "## Recalled background\n%s\n\n%s\n\n", memory, memoryDisclaimer)
		}
	}

	b.WriteString(renderSealedEvidence(req.Packet))

	if hint := strings.TrimSpace(req.RepairHint); hint != "" {
		fmt.Fprintf(&b, "\n## Correction required\nYour previous response could not be used: %s\nReturn the same structure again, corrected.\n", hint)
	}

	b.WriteString("\n" + judgeOutputContract(req.Packet))

	return JudgeContext{
		JudgeID:      req.JudgeID,
		Round:        req.Round,
		EvidenceHash: req.Packet.Hash,
		Isolation:    isolation,
		Prompt:       b.String(),
	}, nil
}

// renderSealedEvidence renders the packet in a fixed order so two judges given
// the same packet receive byte-identical evidence text.
func renderSealedEvidence(packet DecisionEvidencePacket) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Question\n%s\n\n", strings.TrimSpace(packet.Question))

	options := append([]DecisionOption(nil), packet.Options...)
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	b.WriteString("## Options\n")
	for _, option := range options {
		fmt.Fprintf(&b, "- %s (%s)", option.ID, option.Kind)
		if title := strings.TrimSpace(option.Title); title != "" {
			fmt.Fprintf(&b, ": %s", title)
		}
		b.WriteString("\n")
		if description := strings.TrimSpace(option.Description); description != "" {
			fmt.Fprintf(&b, "  %s\n", description)
		}
	}
	b.WriteString("\n")

	if len(packet.Criteria) > 0 {
		criteria := append([]DecisionCriterion(nil), packet.Criteria...)
		sort.Slice(criteria, func(i, j int) bool { return criteria[i].ID < criteria[j].ID })
		weights, _ := normalizedWeights(criteria)
		b.WriteString("## Criteria\nScore every option against every criterion on a 0-10 scale, where 10 is best.\n")
		for _, criterion := range criteria {
			fmt.Fprintf(&b, "- %s (weight %.4f)", criterion.ID, weights[criterion.ID])
			if statement := strings.TrimSpace(criterion.Statement); statement != "" {
				fmt.Fprintf(&b, ": %s", statement)
			}
			if criterion.Direction == agent.CriterionLowerIsBetter {
				b.WriteString(" [lower raw values are better; score accordingly so 10 still means best]")
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(packet.Assumptions) > 0 {
		assumptions := append([]DecisionAssumption(nil), packet.Assumptions...)
		sort.Slice(assumptions, func(i, j int) bool { return assumptions[i].ID < assumptions[j].ID })
		b.WriteString("## Declared assumptions\n")
		for _, assumption := range assumptions {
			marker := ""
			if assumption.Critical {
				marker = " [critical]"
			}
			fmt.Fprintf(&b, "- %s%s: %s\n", assumption.ID, marker, strings.TrimSpace(assumption.Statement))
		}
		b.WriteString("\n")
	}

	if len(packet.BaseRates) > 0 {
		rates := append([]BaseRateEvidence(nil), packet.BaseRates...)
		sort.Slice(rates, func(i, j int) bool {
			if rates[i].ReferenceClass != rates[j].ReferenceClass {
				return rates[i].ReferenceClass < rates[j].ReferenceClass
			}
			return rates[i].Metric < rates[j].Metric
		})
		b.WriteString("## Reference classes (outside view)\n")
		for _, rate := range rates {
			fmt.Fprintf(&b, "- %s / %s (n=%d): mean %.4f, median %.4f, p10 %.4f, p90 %.4f\n",
				rate.ReferenceClass, rate.Metric, rate.SampleSize,
				rate.Distribution.Mean, rate.Distribution.Median, rate.Distribution.P10, rate.Distribution.P90)
			for _, limitation := range rate.Limitations {
				fmt.Fprintf(&b, "  limitation: %s\n", strings.TrimSpace(limitation))
			}
		}
		b.WriteString("\n")
	}

	if len(packet.Facts) > 0 {
		keys := make([]string, 0, len(packet.Facts))
		for key := range packet.Facts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		b.WriteString("## Facts\n")
		for _, key := range keys {
			fmt.Fprintf(&b, "- %s: %v\n", key, packet.Facts[key])
		}
		b.WriteString("\n")
	}

	if len(packet.Artifacts) > 0 {
		artifacts := append([]ArtifactRef(nil), packet.Artifacts...)
		sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].SHA256 < artifacts[j].SHA256 })
		b.WriteString("## Evidence artifacts\n")
		for _, artifact := range artifacts {
			fmt.Fprintf(&b, "- %s (%s) sha256=%s\n", artifact.Path, artifact.MediaType, artifact.SHA256)
			if description := strings.TrimSpace(artifact.Description); description != "" {
				fmt.Fprintf(&b, "  %s\n", description)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// judgeOutputContract states the structured response the runtime will parse.
// Aggregation is done in Go, so the judge is never asked to do arithmetic.
func judgeOutputContract(packet DecisionEvidencePacket) string {
	var b strings.Builder
	b.WriteString("## Required response\nReturn a single JSON object, no prose around it:\n")
	b.WriteString("{\n")
	b.WriteString(`  "option_scores": [{"option_id": "...", "criteria": {"<criterion id>": 0.0}}],` + "\n")
	b.WriteString(`  "preferred_option": "...",` + "\n")
	b.WriteString(`  "success_probability": 0.0,` + "\n")
	b.WriteString(`  "key_assumptions": ["..."],` + "\n")
	b.WriteString(`  "disconfirming_evidence": ["..."],` + "\n")
	b.WriteString(`  "missing_information": ["..."],` + "\n")
	b.WriteString(`  "confidence": 0.0` + "\n")
	b.WriteString("}\n\n")
	fmt.Fprintf(&b, "Score all %d options. Scores are 0-10; success_probability and confidence are 0-1.\n", len(packet.Options))
	if len(packet.Criteria) > 0 {
		b.WriteString("Provide every criterion for every option. Do not compute an overall score; the runtime aggregates.\n")
	} else {
		b.WriteString(`Provide an "overall" value per option in place of criteria scores.` + "\n")
	}
	return b.String()
}
