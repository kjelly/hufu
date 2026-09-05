package team

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Sealed decision evidence (docs/hufu-decision-aware-runtime-spec.md §15).
//
// The packet is the first critical runtime primitive: every first-round judge
// must see the same evidence, and "the same" has to mean something byte-exact.
// Only material fields — the ones that can change the judgment itself — feed
// the hash. Timestamps, artifact paths and assumption status deliberately do
// not, because hashing them would make every routine bookkeeping update
// invalidate a whole judgment round (spec §15.2).

// DecisionEvidencePacket is the immutable input every judge receives.
type DecisionEvidencePacket struct {
	ID       string `json:"id"`
	Hash     string `json:"hash,omitempty"`
	Question string `json:"question"`

	Options  []DecisionOption    `json:"options,omitempty"`
	Criteria []DecisionCriterion `json:"criteria,omitempty"`

	Facts       map[string]any       `json:"facts,omitempty"`
	Artifacts   []ArtifactRef        `json:"artifacts,omitempty"`
	BaseRates   []BaseRateEvidence   `json:"base_rates,omitempty"`
	Assumptions []DecisionAssumption `json:"assumptions,omitempty"`
	Provenance  []EvidenceProvenance `json:"provenance,omitempty"`

	RequestContractRef string    `json:"request_contract_ref,omitempty"`
	CreatedAt          time.Time `json:"created_at,omitzero"`

	// CanonicalVersion records which encoder produced Hash, so a later hash
	// mismatch can be attributed to changed evidence or to an encoder upgrade
	// rather than being ambiguous (see CanonicalFormVersion).
	CanonicalVersion int `json:"canonical_version,omitempty"`

	// Sealed marks the packet as closed to further mutation. Judges may only
	// receive a sealed packet.
	Sealed bool `json:"sealed,omitempty"`
}

// canonicalMaterial builds the hash input from the material fields only.
func (p DecisionEvidencePacket) canonicalMaterial() ([]byte, error) {
	root := canonicalObject{}
	// The encoder version is part of the hashed form, so digests produced by
	// two different encoders can never collide and be read as the same
	// evidence.
	root.set("canonical_version", CanonicalFormVersion)
	root.setString("question", p.Question)

	if len(p.Options) > 0 {
		options := append([]DecisionOption(nil), p.Options...)
		sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
		encoded := make([]any, 0, len(options))
		for _, option := range options {
			item := canonicalObject{}
			item.setString("id", option.ID)
			item.setString("kind", string(option.Kind))
			// Origin is material: whether an option was thought of or injected
			// to satisfy a gate changes how a judge should read it (§19.1).
			item.setString("origin", string(option.EffectiveOrigin()))
			item.setString("title", option.Title)
			item.setString("description", option.Description)
			encoded = append(encoded, item)
		}
		root.set("options", encoded)
	}

	if len(p.Criteria) > 0 {
		weights, err := normalizedWeights(p.Criteria)
		if err != nil {
			return nil, fmt.Errorf("canonicalizing criteria: %w", err)
		}
		criteria := append([]DecisionCriterion(nil), p.Criteria...)
		sort.Slice(criteria, func(i, j int) bool { return criteria[i].ID < criteria[j].ID })
		encoded := make([]any, 0, len(criteria))
		for _, criterion := range criteria {
			item := canonicalObject{}
			item.setString("id", criterion.ID)
			item.setString("statement", criterion.Statement)
			item.set("normalized_weight", weights[criterion.ID])
			item.setString("direction", criterion.Direction)
			encoded = append(encoded, item)
		}
		root.set("criteria", encoded)
	}

	if len(p.Facts) > 0 {
		facts := canonicalObject{}
		for key, value := range p.Facts {
			facts.set(canonicalString(key), value)
		}
		root.set("facts", facts)
	}

	if len(p.Artifacts) > 0 {
		artifacts := append([]ArtifactRef(nil), p.Artifacts...)
		sort.Slice(artifacts, func(i, j int) bool {
			if artifacts[i].SHA256 != artifacts[j].SHA256 {
				return artifacts[i].SHA256 < artifacts[j].SHA256
			}
			return artifacts[i].Role < artifacts[j].Role
		})
		encoded := make([]any, 0, len(artifacts))
		for _, artifact := range artifacts {
			// Content identity only: the same bytes under a different path,
			// run or attempt are not new evidence.
			item := canonicalObject{}
			item.setString("sha256", artifact.SHA256)
			item.setString("media_type", artifact.MediaType)
			item.setString("role", artifact.Role)
			encoded = append(encoded, item)
		}
		root.set("artifacts", encoded)
	}

	if len(p.BaseRates) > 0 {
		rates := append([]BaseRateEvidence(nil), p.BaseRates...)
		sort.Slice(rates, func(i, j int) bool {
			if rates[i].ReferenceClass != rates[j].ReferenceClass {
				return rates[i].ReferenceClass < rates[j].ReferenceClass
			}
			return rates[i].Metric < rates[j].Metric
		})
		encoded := make([]any, 0, len(rates))
		for _, rate := range rates {
			distribution := canonicalObject{}
			distribution.set("mean", rate.Distribution.Mean)
			distribution.set("median", rate.Distribution.Median)
			distribution.set("p10", rate.Distribution.P10)
			distribution.set("p90", rate.Distribution.P90)

			item := canonicalObject{}
			item.setString("reference_class", rate.ReferenceClass)
			item.setString("metric", rate.Metric)
			item.set("sample_size", rate.SampleSize)
			item.set("distribution", distribution)
			encoded = append(encoded, item)
		}
		root.set("base_rates", encoded)
	}

	if len(p.Assumptions) > 0 {
		assumptions := append([]DecisionAssumption(nil), p.Assumptions...)
		sort.Slice(assumptions, func(i, j int) bool { return assumptions[i].ID < assumptions[j].ID })
		encoded := make([]any, 0, len(assumptions))
		for _, assumption := range assumptions {
			// Status is excluded on purpose: an assumption being checked must
			// not invalidate opinions that were formed on the same statement.
			item := canonicalObject{}
			item.setString("id", assumption.ID)
			item.setString("statement", assumption.Statement)
			if assumption.Critical {
				item.set("critical", true)
			}
			encoded = append(encoded, item)
		}
		root.set("assumptions", encoded)
	}

	return canonicalEncode(root)
}

// EvidenceHash computes the packet's material digest without sealing it.
func (p DecisionEvidencePacket) EvidenceHash() (string, error) {
	canonical, err := p.canonicalMaterial()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Seal computes the material hash and closes the packet. Sealing twice is
// allowed and must produce the same hash; a packet whose material changed
// between seals is a different packet and gets a different hash (spec §15.4).
func (p DecisionEvidencePacket) Seal() (DecisionEvidencePacket, error) {
	hash, err := p.EvidenceHash()
	if err != nil {
		return DecisionEvidencePacket{}, fmt.Errorf("sealing decision evidence: %w", err)
	}
	sealed := p
	sealed.Hash = hash
	sealed.Sealed = true
	sealed.CanonicalVersion = CanonicalFormVersion
	return sealed, nil
}

// EffectiveCanonicalVersion returns the encoder version that produced this
// packet's hash. A packet persisted before versioning existed reports 1, which
// is what that encoder was.
func (p DecisionEvidencePacket) EffectiveCanonicalVersion() int {
	if p.CanonicalVersion == 0 {
		return 1
	}
	return p.CanonicalVersion
}

// CanonicalFormOutdated reports whether this packet's hash was produced by an
// encoder older than this build's. It is the check that separates "the
// evidence changed" from "the encoder changed" when a hash no longer matches.
func (p DecisionEvidencePacket) CanonicalFormOutdated() bool {
	return p.Hash != "" && p.EffectiveCanonicalVersion() != CanonicalFormVersion
}

// Validate checks the packet is usable as sealed evidence.
func (p DecisionEvidencePacket) Validate() error {
	if canonicalString(p.Question) == "" {
		return fmt.Errorf("decision evidence requires a question")
	}
	seen := make(map[string]bool, len(p.Options))
	for i, option := range p.Options {
		if canonicalString(option.ID) == "" {
			return fmt.Errorf("options[%d].id must not be empty", i)
		}
		if seen[option.ID] {
			return fmt.Errorf("options[%d].id %q is duplicated", i, option.ID)
		}
		seen[option.ID] = true
		if !option.Kind.Valid() {
			return fmt.Errorf("options[%d].kind %q is not a declared option kind", i, option.Kind)
		}
	}
	if err := validateCriteriaIDs(p.Criteria); err != nil {
		return err
	}
	return nil
}

func validateCriteriaIDs(criteria []DecisionCriterion) error {
	seen := make(map[string]bool, len(criteria))
	for i, criterion := range criteria {
		if canonicalString(criterion.ID) == "" {
			return fmt.Errorf("criteria[%d].id must not be empty", i)
		}
		if seen[criterion.ID] {
			return fmt.Errorf("criteria[%d].id %q is duplicated", i, criterion.ID)
		}
		seen[criterion.ID] = true
	}
	return nil
}

// HasOption reports whether id names an option in the packet.
func (p DecisionEvidencePacket) HasOption(id string) bool {
	for _, option := range p.Options {
		if option.ID == id {
			return true
		}
	}
	return false
}

// OptionIDs returns the packet's option IDs in ascending order, the iteration
// order every deterministic computation uses (spec §14.4).
func (p DecisionEvidencePacket) OptionIDs() []string {
	ids := make([]string, 0, len(p.Options))
	for _, option := range p.Options {
		ids = append(ids, option.ID)
	}
	sort.Strings(ids)
	return ids
}

// NoGoOption returns the first no-action alternative in ascending ID order, or
// an empty string when the packet has none (spec §19).
func (p DecisionEvidencePacket) NoGoOption() string {
	options := append([]DecisionOption(nil), p.Options...)
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	for _, option := range options {
		if option.Kind.IsNoGo() {
			return option.ID
		}
	}
	return ""
}
