package decisionrt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type canonicalOption struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type canonicalSpec struct {
	ID       string            `json:"id"`
	Version  string            `json:"version"`
	Kind     Kind              `json:"kind"`
	Question string            `json:"question"`
	Options  []canonicalOption `json:"options"`
	Range    *IntegerRange     `json:"range"`
}

type canonicalRequest struct {
	Purpose string                  `json:"purpose"`
	Spec    canonicalSpec           `json:"spec"`
	Context []canonicalContextEntry `json:"context"`
}

func Digest(request Request) (string, error) {
	if err := request.Validate(); err != nil {
		return "", err
	}
	contextEntries, err := canonicalizeContext(request.Context)
	if err != nil {
		return "", err
	}
	options := make([]canonicalOption, 0, len(request.Spec.Options))
	for _, option := range request.Spec.Options {
		options = append(options, canonicalOption(option))
	}
	canonical := canonicalRequest{
		Purpose: request.Purpose,
		Spec: canonicalSpec{
			ID:       request.Spec.ID,
			Version:  request.Spec.Version,
			Kind:     request.Spec.Kind,
			Question: request.Spec.Question,
			Options:  options,
			Range:    request.Spec.Range,
		},
		Context: contextEntries,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", runtimeError(ErrorInvalidRequest, "", err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
