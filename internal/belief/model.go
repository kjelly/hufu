// Package belief computes bounded, replayable finite Bayesian state.
// It has no dependency on runtime execution, persistence, providers, or clocks.
package belief

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
)

const (
	SchemaVersion              = 1
	ReducerVersion             = "finite_bayes_v1"
	Scale               uint64 = 1_000_000_000
	MaxModelBytes              = 1 << 20
	MaxHypotheses              = 32
	MaxExperiments             = 32
	MaxOutcomes                = 16
	MaxObservations            = 256
	MaxIntermediateBits        = 16_384
)

type ErrorCode string

const (
	InvalidModel       ErrorCode = "invalid_model"
	InvalidObservation ErrorCode = "invalid_observation"
	InvalidEpoch       ErrorCode = "invalid_epoch"
	IdentityConflict   ErrorCode = "identity_conflict"
	UnsupportedVersion ErrorCode = "unsupported_version"
	ResourceLimit      ErrorCode = "resource_limit"
)

// Error carries a stable category and a non-content-bearing structural location.
type Error struct {
	Code  ErrorCode
	Field string
}

func (e *Error) Error() string                   { return fmt.Sprintf("belief: %s (%s)", e.Code, e.Field) }
func invalid(code ErrorCode, field string) error { return &Error{Code: code, Field: field} }

type Model struct {
	SchemaVersion     int          `json:"schema_version"`
	ReducerVersion    string       `json:"reducer_version"`
	ID                string       `json:"id"`
	Version           string       `json:"version"`
	Synthetic         bool         `json:"synthetic"`
	PriorSource       string       `json:"prior_source"`
	IndependenceBasis string       `json:"independence_basis"`
	Applicability     string       `json:"applicability"`
	Hypotheses        []Hypothesis `json:"hypotheses"`
	Experiments       []Experiment `json:"experiments"`
}
type Hypothesis struct {
	ID         string `json:"id"`
	Meaning    string `json:"meaning"`
	PriorUnits uint64 `json:"prior_units"`
}
type Experiment struct {
	ID               string    `json:"id"`
	CorrelationGroup string    `json:"correlation_group"`
	Source           string    `json:"source"`
	Outcomes         []Outcome `json:"outcomes"`
}
type Outcome struct {
	ID              string   `json:"id"`
	LikelihoodUnits []uint64 `json:"likelihood_units"`
}

// DecodeModel is the untrusted JSON boundary; exact integer decoding never uses float64.
func DecodeModel(data []byte) (Model, error) {
	if len(data) > MaxModelBytes {
		return Model{}, invalid(ResourceLimit, "model_bytes")
	}
	var model Model
	if err := json.Unmarshal(data, &model, json.RejectUnknownMembers(true)); err != nil {
		return Model{}, invalid(InvalidModel, "json")
	}
	if err := validateModelWire(data); err != nil {
		return Model{}, err
	}
	if err := ValidateModel(model); err != nil {
		return Model{}, err
	}
	return model, nil
}

func ValidateModel(model Model) error {
	if model.SchemaVersion != SchemaVersion || model.ReducerVersion != ReducerVersion {
		return invalid(UnsupportedVersion, "model")
	}
	if !validID(model.ID) || !validID(model.Version) || !description(model.PriorSource) || !description(model.IndependenceBasis) || !description(model.Applicability) {
		return invalid(InvalidModel, "metadata")
	}
	if len(model.Hypotheses) < 2 || len(model.Hypotheses) > MaxHypotheses || len(model.Experiments) < 1 || len(model.Experiments) > MaxExperiments {
		return invalid(ResourceLimit, "model_dimensions")
	}
	ids := map[string]bool{}
	var total uint64
	for _, h := range model.Hypotheses {
		if !validID(h.ID) || ids[h.ID] || !description(h.Meaning) || h.PriorUnits > Scale {
			return invalid(InvalidModel, "hypotheses")
		}
		ids[h.ID] = true
		total += h.PriorUnits
	}
	if total != Scale {
		return invalid(InvalidModel, "prior_sum")
	}
	ids = map[string]bool{}
	for _, experiment := range model.Experiments {
		if !validID(experiment.ID) || ids[experiment.ID] || !validID(experiment.CorrelationGroup) || experiment.Source != "verification" {
			return invalid(InvalidModel, "experiments")
		}
		ids[experiment.ID] = true
		if err := validateOutcomes(experiment.Outcomes, len(model.Hypotheses)); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return invalid(InvalidModel, "encoding")
	}
	if len(encoded) > MaxModelBytes {
		return invalid(ResourceLimit, "model_bytes")
	}
	return nil
}
func validateOutcomes(outcomes []Outcome, states int) error {
	if len(outcomes) < 2 || len(outcomes) > MaxOutcomes {
		return invalid(ResourceLimit, "outcomes")
	}
	sums := make([]uint64, states)
	ids := map[string]bool{}
	for _, outcome := range outcomes {
		if !validID(outcome.ID) || ids[outcome.ID] || len(outcome.LikelihoodUnits) != states {
			return invalid(InvalidModel, "outcomes")
		}
		ids[outcome.ID] = true
		for i, value := range outcome.LikelihoodUnits {
			if value > Scale {
				return invalid(InvalidModel, "likelihood_units")
			}
			sums[i] += value
		}
	}
	for _, sum := range sums {
		if sum != Scale {
			return invalid(InvalidModel, "likelihood_sum")
		}
	}
	return nil
}
func validID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return false
	}
	return true
}
func description(value string) bool { return len(value) > 0 && len(value) <= 4096 }
func digest(domain string, value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

// ModelDigest binds all declared semantics, including likelihood vector order.
func ModelDigest(model Model) (string, error) {
	if err := ValidateModel(model); err != nil {
		return "", err
	}
	return digest("hufu-belief-model-v1", model)
}

func wireObject(data []byte, keys ...string) (map[string]jsontext.Value, error) {
	var object map[string]jsontext.Value
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, invalid(InvalidModel, "json")
	}
	for _, key := range keys {
		value, ok := object[key]
		if !ok || string(value) == "null" {
			return nil, invalid(InvalidModel, "required_fields")
		}
	}
	return object, nil
}
func validateModelWire(data []byte) error {
	root, err := wireObject(data, "schema_version", "reducer_version", "id", "version", "synthetic", "prior_source", "independence_basis", "applicability", "hypotheses", "experiments")
	if err != nil {
		return err
	}
	var hypotheses, experiments []jsontext.Value
	if err := json.Unmarshal(root["hypotheses"], &hypotheses); err != nil {
		return invalid(InvalidModel, "hypotheses")
	}
	for _, h := range hypotheses {
		if _, err := wireObject(h, "id", "meaning", "prior_units"); err != nil {
			return err
		}
	}
	if err := json.Unmarshal(root["experiments"], &experiments); err != nil {
		return invalid(InvalidModel, "experiments")
	}
	for _, e := range experiments {
		experiment, err := wireObject(e, "id", "correlation_group", "source", "outcomes")
		if err != nil {
			return err
		}
		var outcomes []jsontext.Value
		if err := json.Unmarshal(experiment["outcomes"], &outcomes); err != nil {
			return invalid(InvalidModel, "outcomes")
		}
		for _, o := range outcomes {
			outcome, err := wireObject(o, "id", "likelihood_units")
			if err != nil {
				return err
			}
			var units []jsontext.Value
			if err := json.Unmarshal(outcome["likelihood_units"], &units); err != nil {
				return invalid(InvalidModel, "likelihood_units")
			}
			for _, unit := range units {
				if string(unit) == "null" {
					return invalid(InvalidModel, "likelihood_units")
				}
			}
		}
	}
	return nil
}
