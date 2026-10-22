package evalharness

import (
	"fmt"

	"github.com/kjelly/hufu/internal/scripted"
)

const evalModelDriverName = "eval-harness-model"

// Keep evaluator findings and legacy entry points while sharing transport.
type scriptedProvider struct{ *scripted.Provider }

func newScriptedProvider(fixture *ProviderFixture) *scriptedProvider {
	return &scriptedProvider{scripted.NewProvider(evalModelDriverName, fixture.Steps)}
}

func (p *scriptedProvider) nextStep(body []byte) (ProviderStep, error) {
	return p.NextStep(body)
}

func (p *scriptedProvider) scriptFindings() []EvalFinding {
	var findings []EvalFinding
	for _, issue := range p.Issues() {
		switch issue.Reason {
		case "forbidden_consumed":
			findings = append(findings, EvalFinding{Dimension: "provider-fixture", Expected: "forbidden step never consumed", Actual: fmt.Sprintf("a request contained %q", issue.Step.Match.Contains)})
		case "unused_step":
			findings = append(findings, EvalFinding{Dimension: "provider-fixture", Expected: "every scripted step consumed", Actual: fmt.Sprintf("unused step: %+v", issue.Step)})
		}
	}
	return findings
}
