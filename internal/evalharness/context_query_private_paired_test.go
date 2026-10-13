package evalharness

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestContextQueryPairedProviderRecallsPrivatePersistentOnlyForOwner(t *testing.T) {
	suite, c, steps := pairedQueryFixture(t, "constraints", "migration sentinel")
	worker := "---\nname: worker\nrole: worker\ntools: view\nmodel: eval-harness-model\nmemory:\n  mode: persistent\n  max-items: 4\n  max-tokens: 500\n---\nComplete the assigned task and submit_result.\n"
	if err := os.WriteFile(filepath.Join(suite.Team, "worker.md"), []byte(worker), 0o600); err != nil {
		t.Fatal(err)
	}
	c.ContextItems = []ContextItemFixture{
		{ID: "private-target", Content: "migration sentinel owner-only knowledge", Scope: "same_worker_private", Agent: "worker"},
		{ID: "foreign", Content: "migration sentinel foreign-worker knowledge", Scope: "other_worker"},
	}
	c.Expect.ForbiddenContext = []string{"foreign"}
	c.Expect.ContextDelivery[0].Relevant = []string{"private-target"}
	for _, profile := range []string{"legacy", "candidate"} {
		c.QueryProfileOverride = profile
		c.Expect.ContextDelivery[0].Include, c.Expect.ContextDelivery[0].Exclude = nil, nil
		if profile == "candidate" {
			c.Expect.ContextDelivery[0].Include = []string{"private-target"}
		} else {
			c.Expect.ContextDelivery[0].Exclude = []string{"private-target"}
		}
		provider := newScriptedProvider(&ProviderFixture{Steps: steps})
		result, err := runCaseWithHandler(t.Context(), suite, c, provider, provider.scriptFindings)
		if err != nil || !result.Passed {
			t.Fatalf("%s: err=%v findings=%+v", profile, err, result.Findings)
		}
		if profile == "candidate" {
			metrics := result.Metrics.ContextDelivery.Requests[0]
			if metrics.QueryAttribution == nil || metrics.QueryAttribution.Mode != team.QueryModeMixed || metrics.DeliveryRecall == nil || *metrics.DeliveryRecall != 1 {
				t.Fatal("private candidate was not delivered with effective mixed attribution")
			}
			found := false
			for _, source := range metrics.QueryAttribution.Sources {
				if source.Source == team.QueryPrivatePersistent {
					found = source.Mode == team.QueryModeActive
				}
			}
			if !found {
				t.Fatal("actual private recall is not attributed")
			}
		}
	}
}
