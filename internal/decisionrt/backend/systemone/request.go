package systemone

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/kjelly/hufu/internal/decisionrt"
)

const (
	questionKey         = "decision"
	nativeChoice        = "choice"
	nativeNoul          = "noul"
	maximumRequestBytes = 64 << 10
)

var (
	errTooFewCandidates = errors.New("systemone requires at least two candidates")
	errRequestTooLarge  = errors.New("systemone request exceeds 64 KiB")
)

type nativeRequest struct {
	Model     string                    `json:"model"`
	State     map[string]any            `json:"state"`
	Questions map[string]nativeQuestion `json:"questions"`
}

type nativeQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// candidate maps one opaque native key back to its typed value. The slice
// order is the declared order of the decision domain.
type candidate struct {
	key       string
	value     decisionrt.Value
	canonical string
	label     string
}

func encodeRequest(model string, request decisionrt.Request) ([]byte, []candidate, error) {
	if err := request.Validate(); err != nil {
		return nil, nil, err
	}
	state, err := decisionrt.CanonicalContextValues(request.Context)
	if err != nil {
		return nil, nil, err
	}
	question := nativeQuestion{Type: nativeChoice, Instructions: request.Spec.Question}
	candidates, err := nativeCandidates(request.Spec)
	if err != nil {
		return nil, nil, err
	}
	if request.Spec.Kind == decisionrt.KindBoolean {
		question.Type = nativeNoul
	} else {
		// Keys are zero-padded, so the sorted map encoding keeps declared order.
		question.Criteria = make(map[string]string, len(candidates))
		for _, item := range candidates {
			question.Criteria[item.key] = item.label
		}
	}
	encoded, err := json.Marshal(nativeRequest{
		Model: model, State: state, Questions: map[string]nativeQuestion{questionKey: question},
	})
	if err != nil {
		return nil, nil, backendFailure(fmt.Errorf("encoding systemone request: %w", err))
	}
	if len(encoded) > maximumRequestBytes {
		return nil, nil, backendFailure(errRequestTooLarge)
	}
	return encoded, candidates, nil
}

func nativeCandidates(spec decisionrt.Spec) ([]candidate, error) {
	switch spec.Kind {
	case decisionrt.KindChoice:
		candidates := make([]candidate, 0, len(spec.Options))
		for index, option := range spec.Options {
			label := option.ID
			if option.Description != "" {
				label += ": " + option.Description
			}
			candidates = append(candidates, candidate{
				key: opaqueKey('o', index), value: decisionrt.Value{Choice: option.ID}, canonical: option.ID, label: label,
			})
		}
		return candidates, nil
	case decisionrt.KindBoolean:
		return []candidate{
			{value: decisionrt.Value{Boolean: new(false)}, canonical: "false"},
			{value: decisionrt.Value{Boolean: new(true)}, canonical: "true"},
		}, nil
	case decisionrt.KindIntegerRange:
		width := uint64(spec.Range.Max) - uint64(spec.Range.Min)
		if width == 0 {
			return nil, backendFailure(errTooFewCandidates)
		}
		candidates := make([]candidate, 0, width+1)
		for offset := range width + 1 {
			value := spec.Range.Min + int64(offset)
			canonical := strconv.FormatInt(value, 10)
			candidates = append(candidates, candidate{
				key: opaqueKey('i', int(offset)), value: decisionrt.Value{Integer: new(value)}, canonical: canonical, label: canonical,
			})
		}
		return candidates, nil
	default:
		return nil, &decisionrt.RuntimeError{Kind: decisionrt.ErrorInvalidRequest, Backend: backendName, Err: errors.New("unknown decision kind")}
	}
}

func opaqueKey(prefix byte, index int) string {
	return fmt.Sprintf("%c%02d", prefix, index)
}
