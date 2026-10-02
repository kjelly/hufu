package control_test

import (
	"context"
	"strings"
	"testing"

	sidecarbackend "github.com/kjelly/hufu/internal/decisionrt/backend/sidecar"
	"github.com/kjelly/hufu/internal/decisionrt/catalog"
	"github.com/kjelly/hufu/internal/decisionrt/control"
)

// tokenGenerator is a language-model generator that always names token.
type tokenGenerator struct{ token string }

func (g tokenGenerator) Execute(context.Context, string) (string, error) {
	return `{"token":"` + g.token + `"}`, nil
}

func (g tokenGenerator) ModelID() string { return "ollama/minimax-m3:cloud" }

func sidecarFactory(token string) catalog.GeneratorFactory {
	return func(model string) (sidecarbackend.Generator, error) {
		if model != "" {
			return nil, nil
		}
		return tokenGenerator{token: token}, nil
	}
}

func TestSidecarBackendDecidesWithoutConfidence(t *testing.T) {
	config := control.Config{Backend: control.BackendSidecar, Model: "nimble", Endpoint: "http://rog:11434/v1/systemone", Mode: control.ModeActive}
	if _, err := control.New(config, identity); err == nil || !strings.Contains(err.Error(), "model runtime") {
		t.Fatalf("New without a generator = %v, want a missing model runtime error", err)
	}
	service, err := control.New(config, identity, control.WithSidecarGenerator(sidecarFactory("A1")))
	if err != nil {
		t.Fatal(err)
	}
	if service.Backend() != control.BackendSidecar || service.MinConfidence(control.PathReviewer) != 0 {
		t.Fatalf("backend %q threshold %v, want sidecar without a threshold", service.Backend(), service.MinConfidence(control.PathReviewer))
	}
	request, ok := control.PathReviewerRequest("cat /etc/os-release", "/etc/os-release")
	if !ok {
		t.Fatal("path request not applicable")
	}
	outcome := service.Decide(t.Context(), control.PathReviewer, request)
	if outcome.Status != control.StatusDecided || outcome.Value != "true" || !outcome.Accepted || outcome.Confidence != 0 || outcome.Model != "ollama/minimax-m3:cloud" {
		t.Fatalf("outcome = %+v, want an accepted decision from the sidecar model", outcome)
	}
	systemone := newSystemOneServer(t, 200, noulBody("0.9"))
	direct, err := control.New(control.Config{Endpoint: systemone.endpoint(), Model: "nimble", Mode: control.ModeActive}, identity)
	if err != nil {
		t.Fatal(err)
	}
	if service.Hash() == "" || service.Hash() == direct.Hash() {
		t.Fatalf("sidecar hash %q must pin the backend apart from systemone %q", service.Hash(), direct.Hash())
	}
}

func TestBackendMergesAndValidates(t *testing.T) {
	merged := control.Merge(control.Config{Backend: control.BackendSystemOne, Model: "nimble"}, control.Config{Backend: control.BackendSidecar})
	if merged.Backend != control.BackendSidecar || merged.Model != "nimble" {
		t.Fatalf("merged = %+v, want the team's backend over the global one", merged)
	}
	if err := (control.Config{Backend: "nimble"}).Validate(); err == nil {
		t.Fatal("an unknown backend was accepted")
	}
}
