// The diagnostic pilot owns its synthetic domain; core never interprets it.
package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/belief"
	"github.com/kjelly/hufu/internal/team"
)

//go:embed fixtures/*.json *.schema.json model.json policy.json adapter.json provenance.json fixture.go evaluate.go
var resources embed.FS

const (
	fixtureCapability = "diagnostic_fixture"
	maxBundleBytes    = 64 << 10
)

func embeddedDigest(name string) (string, error) {
	data, err := resources.ReadFile(name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type vector struct {
	Input    int64 `json:"input"`
	Expected int64 `json:"expected"`
	Actual   int64 `json:"actual"`
}

type environment struct {
	Required string `json:"required_version"`
	Observed string `json:"observed_version"`
}

type fixture struct {
	SchemaVersion int         `json:"schema_version"`
	ID            string      `json:"fixture_id"`
	Vectors       []vector    `json:"source_vectors"`
	Environment   environment `json:"environment"`
	FailureRef    string      `json:"failed_test_receipt_ref"`
	Scope         string      `json:"scope"`
}

type probeInput struct {
	FixtureRef   string `json:"fixture_ref"`
	ExperimentID string `json:"experiment_id"`
}

type measurement struct {
	FixtureRef   string `json:"fixture_ref"`
	ExperimentID string `json:"experiment_id"`
	Outcome      string `json:"outcome"`
	Digest       string `json:"measurement_digest"`
}

// fixtureProvider has no filesystem, network, shell or host-environment input.
// The registry owns decoded copies of bounded, maintainer-authored bundles.
type fixtureProvider struct {
	bundles map[string]fixture
}

var _ team.ActionProvider = (*fixtureProvider)(nil)

func requiredObject(data []byte, keys ...string) error {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("invalid fixture object")
	}
	for _, key := range keys {
		if value, ok := fields[key]; !ok || string(value) == "null" {
			return fmt.Errorf("required field %s is missing or null", key)
		}
	}
	return nil
}

func decodeFixture(data []byte) (fixture, error) {
	var f fixture
	if len(data) > maxBundleBytes {
		return f, fmt.Errorf("fixture exceeds size limit")
	}
	if err := json.Unmarshal(data, &f, json.RejectUnknownMembers(true)); err != nil {
		return f, fmt.Errorf("invalid fixture JSON")
	}
	if err := requiredObject(data, "schema_version", "fixture_id", "source_vectors", "environment", "failed_test_receipt_ref", "scope"); err != nil {
		return f, err
	}
	var raw struct {
		Vectors     []jsontext.Value `json:"source_vectors"`
		Environment jsontext.Value   `json:"environment"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return f, err
	}
	for _, value := range raw.Vectors {
		if err := requiredObject(value, "input", "expected", "actual"); err != nil {
			return f, err
		}
	}
	if err := requiredObject(raw.Environment, "required_version", "observed_version"); err != nil {
		return f, err
	}
	if f.SchemaVersion != 1 || f.Scope != "single_fault_fixture_v1" || len(f.ID) < 1 || len(f.ID) > 128 || strings.ContainsFunc(f.ID, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' }) || len(f.Vectors) < 1 || len(f.Vectors) > 8 || !belief.ValidDiagnosticDigest(f.FailureRef) {
		return f, fmt.Errorf("fixture out_of_scope or invalid identity")
	}
	for _, v := range f.Vectors {
		for _, n := range []int64{v.Input, v.Expected, v.Actual} {
			if n < -1_000_000 || n > 1_000_000 {
				return f, fmt.Errorf("fixture integer exceeds bound")
			}
		}
	}
	for _, version := range []string{f.Environment.Required, f.Environment.Observed} {
		if len(version) == 0 || len(version) > 32 || strings.ContainsFunc(version, func(r rune) bool { return r < 33 || r > 126 }) {
			return f, fmt.Errorf("invalid fixture version")
		}
	}
	return f, nil
}

func contentDigest(domain string, value any) (string, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(domain+"\x00"), data...))
	return hex.EncodeToString(sum[:]), nil
}

func newFixtureProvider(bundles ...[]byte) (*fixtureProvider, error) {
	p := &fixtureProvider{bundles: map[string]fixture{}}
	for _, data := range bundles {
		f, err := decodeFixture(data)
		if err != nil {
			return nil, err
		}
		ref, err := contentDigest("diagnostic-fixture-v1", f)
		if err != nil {
			return nil, err
		}
		p.bundles[ref] = f
	}
	if len(p.bundles) == 0 || len(p.bundles) > 4 {
		return nil, fmt.Errorf("pilot requires one to four registered bundles")
	}
	return p, nil
}

func (p *fixtureProvider) input(action team.Action) (probeInput, error) {
	var input probeInput
	if action.Capability != fixtureCapability || !slices.Contains([]string{"check_source", "check_environment"}, action.Type) || len(action.InputBindings) != 0 || len(action.Payload) > belief.MaxDiagnosticEvidenceBytes {
		return input, fmt.Errorf("unsupported fixture action")
	}
	if err := json.Unmarshal([]byte(action.Payload), &input, json.RejectUnknownMembers(true)); err != nil {
		return input, fmt.Errorf("invalid fixture action arguments")
	}
	if input.ExperimentID != action.Type || !belief.ValidDiagnosticDigest(input.FixtureRef) {
		return input, fmt.Errorf("fixture identity or experiment mismatch")
	}
	if _, ok := p.bundles[input.FixtureRef]; !ok {
		return input, fmt.Errorf("fixture is not registered")
	}
	return input, nil
}

func (p *fixtureProvider) Validate(action team.Action) error {
	_, err := p.input(action)
	return err
}

func (p *fixtureProvider) Execute(ctx context.Context, action team.Action) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	input, err := p.input(action)
	if err != nil {
		return nil, err
	}
	m, err := measure(p.bundles[input.FixtureRef], input)
	if err != nil {
		return nil, err
	}
	return team.ActionResult{Outputs: map[string]any{"measurement": m}}, nil
}

func measure(f fixture, input probeInput) (measurement, error) {
	m := measurement{FixtureRef: input.FixtureRef, ExperimentID: input.ExperimentID, Outcome: "pass"}
	var checked any
	switch input.ExperimentID {
	case "check_source":
		checked = f.Vectors
		if slices.ContainsFunc(f.Vectors, func(v vector) bool { return v.Actual != v.Expected }) {
			m.Outcome = "fail"
		}
	case "check_environment":
		checked = f.Environment
		if f.Environment.Required != f.Environment.Observed {
			m.Outcome = "fail"
		}
	default:
		return m, fmt.Errorf("unknown fixture experiment")
	}
	var err error
	m.Digest, err = contentDigest("diagnostic-measurement-v1", struct {
		Experiment string `json:"experiment_id"`
		Data       any    `json:"checked_data"`
		Outcome    string `json:"outcome"`
	}{input.ExperimentID, checked, m.Outcome})
	return m, err
}

// verifyMeasurement recomputes the objective comparison; a provider's outcome
// or digest is never accepted merely because it looks well formed.
func verifyMeasurement(f fixture, input probeInput, got measurement) error {
	want, err := measure(f, input)
	if err != nil || got != want {
		return fmt.Errorf("fixture objective verification failed")
	}
	return nil
}
