package catalog

import (
	"fmt"

	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/decisionrt/backend"
	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
)

// GeneratorFactory returns the language-model generator for a decision entry
// with backend: sidecar. An empty model asks for the caller's default model;
// the generator's ModelID is the model recorded in every receipt.
type GeneratorFactory func(model string) (sidecarbackend.Generator, error)

// ServiceOption configures New.
type ServiceOption func(*options)

type options struct {
	generator      GeneratorFactory
	validationOnly bool
}

// WithSidecarGenerator supplies the generators for backend: sidecar entries.
// The team runtime owns provider selection, credentials and request
// admission, so the catalog never builds a model client itself.
func WithSidecarGenerator(factory GeneratorFactory) ServiceOption {
	return func(o *options) { o.generator = factory }
}

// ValidationOnly checks configuration without binding sidecar entries to a
// model. The returned service is for validation and cannot decide for them.
func ValidationOnly() ServiceOption {
	return func(o *options) { o.validationOnly = true }
}

// validateSidecarEntry rejects settings that do not apply to a language-model
// backend. It reports no confidence, so any confidence policy would make every
// call abstain.
func validateSidecarEntry(entry Entry) error {
	if entry.Endpoint != "" || entry.APIKeyEnv != "" {
		return fmt.Errorf("endpoint and api-key-env apply to backend systemone; backend sidecar uses the team's model providers")
	}
	if entry.MinConfidence != nil || entry.RequireCalibrated {
		return fmt.Errorf("min-confidence and require-calibrated need a backend that reports confidence; backend sidecar does not")
	}
	return nil
}

// primary builds an entry's primary backend. For backend sidecar it resolves
// the generator and records its model on the entry, so the catalog hash and
// receipt validation name the model that actually decides.
func (o options) primary(entry *Entry, apiKey string) (decisionrt.Backend, error) {
	if entry.Backend != "sidecar" {
		return backend.New(backend.Config{Name: entry.Backend, Endpoint: entry.Endpoint, Model: entry.Model, APIKey: apiKey})
	}
	if o.generator == nil {
		if o.validationOnly {
			// Bind a placeholder so the runtime still validates the timeout
			// and policy; this service never decides.
			return backend.New(backend.Config{Name: "rule"})
		}
		return nil, fmt.Errorf("backend sidecar needs the team's model runtime")
	}
	generator, err := o.generator(entry.Model)
	if err != nil {
		return nil, err
	}
	entry.Model = generator.ModelID()
	return backend.New(backend.Config{Name: "sidecar", Generator: generator})
}
