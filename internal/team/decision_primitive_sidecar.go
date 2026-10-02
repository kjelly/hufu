package team

import (
	"context"
	"fmt"
	"sync"

	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/sidecar"
)

// decisionPrimitivePurpose is the auxiliary invocation purpose of a decision
// made on backend sidecar.
const decisionPrimitivePurpose = "decision_primitive"

// decisionSidecarResolver supplies the language-model generators for
// decision-primitives entries with backend: sidecar. The catalog is frozen
// before the coordinator exists, so each generator resolves its sidecar on
// first use, through the same admitted provider path as the coordinator's own
// sidecars, and its usage counts toward the run.
type decisionSidecarResolver struct {
	defaultModel string

	mu       sync.Mutex
	c        *Coordinator
	sidecars map[string]*sidecar.Sidecar
}

func (r *decisionSidecarResolver) generator(model string) (sidecarbackend.Generator, error) {
	if model == "" {
		model = r.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("backend sidecar needs a model: set model or the team's sidecar-model")
	}
	return &lazyDecisionGenerator{resolver: r, model: model}, nil
}

func (r *decisionSidecarResolver) bind(c *Coordinator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.c = c
}

func (r *decisionSidecarResolver) sidecarFor(model string) (*sidecar.Sidecar, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.c == nil {
		return nil, fmt.Errorf("decision sidecar is not bound to a run")
	}
	if model == r.c.sidecarModel {
		if s := r.c.Sidecar(); s != nil {
			return s, nil
		}
		return nil, fmt.Errorf("sidecar model %q is unavailable", model)
	}
	if s, ok := r.sidecars[model]; ok {
		return s, nil
	}
	s, err := r.c.newModelSidecar(model)
	if err != nil {
		return nil, err
	}
	if r.sidecars == nil {
		r.sidecars = make(map[string]*sidecar.Sidecar)
	}
	r.sidecars[model] = s
	return s, nil
}

// lazyDecisionGenerator names its model up front, for the catalog hash and
// receipts, and binds the sidecar only when a decision is made.
type lazyDecisionGenerator struct {
	resolver *decisionSidecarResolver
	model    string
}

func (g *lazyDecisionGenerator) ModelID() string { return g.model }

func (g *lazyDecisionGenerator) Execute(ctx context.Context, prompt string) (string, error) {
	s, err := g.resolver.sidecarFor(g.model)
	if err != nil {
		return "", err
	}
	return s.Execute(sidecar.WithPurpose(ctx, decisionPrimitivePurpose), prompt)
}

// newModelSidecar builds a sidecar for model through the execution policy's
// provider boundary, like Sidecar does for the configured sidecar model.
func (c *Coordinator) newModelSidecar(model string) (*sidecar.Sidecar, error) {
	if !c.providerBoundaryReady() {
		return nil, fmt.Errorf("provider boundary is not ready for model %q", model)
	}
	ctx, invocation, err := c.resolveProviderBoundInvocationContextSnapshot(c.providerInvocationContext(), model, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve decision model %q: %w", model, err)
	}
	gatedBackend, target, err := c.gatedAgentBackendForModel(model)
	if err != nil {
		return nil, fmt.Errorf("resolve decision model %q backend: %w", model, err)
	}
	provider, err := gatedBackend.AgentProvider(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("build decision model %q provider: %w", model, err)
	}
	s, err := sidecar.NewSidecarWithAdmissionContext(ctx, provider, model, c.providerAdmission(), invocation.AdmissionContext)
	if err != nil {
		return nil, fmt.Errorf("build decision model %q sidecar: %w", model, err)
	}
	return c.attachSidecarUsageObserver(s), nil
}
