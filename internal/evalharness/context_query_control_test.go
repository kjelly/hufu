package evalharness

import (
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestContextQueryPairedNoHintsPreservesProviderDeliveryAndCalls(t *testing.T) {
	suite, c, steps := pairedQueryFixture(t, "constraints", "")
	c.ContextItems = []ContextItemFixture{{ID: "positive", Content: "inspect router positive convention"}}
	c.Expect.ProviderCallCount = new(3)
	c.Expect.ContextDelivery[0].Include = []string{"positive"}
	c.Expect.ContextDelivery[0].Relevant = []string{"positive"}
	var arms []ContextRequestMetrics
	for _, profile := range []string{"legacy", "candidate"} {
		c.QueryProfileOverride = profile
		provider := newScriptedProvider(&ProviderFixture{Steps: steps})
		result, err := runCaseWithHandler(t.Context(), suite, c, provider, provider.scriptFindings)
		if err != nil || !result.Passed {
			t.Fatalf("%s err=%v findings=%+v", profile, err, result.Findings)
		}
		arms = append(arms, result.Metrics.ContextDelivery.Requests[0])
	}
	if !reflect.DeepEqual(arms[0].DeliveredIDs, arms[1].DeliveredIDs) || !reflect.DeepEqual(arms[0].RetrievalCalls, arms[1].RetrievalCalls) || arms[0].IrrelevantOptionalTokens != arms[1].IrrelevantOptionalTokens {
		t.Fatal("no-hints candidate changed delivery, query calls or irrelevant cost")
	}
	if arms[1].QueryAttribution == nil || arms[1].QueryAttribution.Mode != team.QueryModeLegacy {
		t.Fatal("no-hints fallback was declared active")
	}
	if arms[0].DeliveryRecall == nil || arms[1].DeliveryRecall == nil || *arms[0].DeliveryRecall != 1 || *arms[1].DeliveryRecall != 1 {
		t.Fatal("positive control regressed")
	}
}

func TestContextQueryEvaluationProfileRejectsUnsupportedModes(t *testing.T) {
	for _, mode := range []string{"active", "shadow", "Candidate", "unknown"} {
		if _, err := team.WithContextQueryEvaluationProfile(t.Context(), mode); err == nil {
			t.Fatalf("accepted unsupported profile %q", mode)
		}
	}
}
