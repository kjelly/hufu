package evalharness

import (
	"os"
	"path/filepath"
	"slices"
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
	var legacy ContextRequestMetrics
	for _, profile := range []string{"legacy", "candidate", "team_active"} {
		c.QueryProfileOverride = profile
		if profile == "team_active" {
			activateFixtureContextQuery(t, suite)
			c.QueryProfileOverride = ""
		}
		c.Expect.ContextDelivery[0].Include, c.Expect.ContextDelivery[0].Exclude = nil, nil
		if profile == "legacy" {
			c.Expect.ContextDelivery[0].Exclude = []string{"private-target"}
		}
		provider := newScriptedProvider(&ProviderFixture{Steps: steps})
		result, err := runCaseWithHandler(t.Context(), suite, c, provider, provider.scriptFindings)
		if err != nil || !result.Passed {
			t.Fatalf("%s: err=%v findings=%+v delivery=%s", profile, err, result.Findings, queryFixtureJSON(t, result.Metrics.ContextDelivery))
		}
		if result.Metrics.ContextDelivery == nil || len(result.Metrics.ContextDelivery.Requests) != 1 {
			t.Fatal("missing paired receiver evidence")
		}
		metrics := result.Metrics.ContextDelivery.Requests[0]
		if profile == "legacy" {
			legacy = metrics
			continue
		}
		if !metrics.ManifestMatched || metrics.ManifestSchema != 3 || metrics.QueryAttribution == nil {
			t.Fatal("private recall lost its durable query manifest")
		}
		// Real SQLite under race/checkptr may exhaust the production 100ms
		// child deadline. This is a valid atomic rollback, not an active recall
		// success. Deterministic runtime tests require active owner delivery.
		if metrics.QueryAttribution.Mode == team.QueryModeLegacy {
			assertPrivateQueryDeadlineRollback(t, legacy, metrics)
			t.Logf("%s: attributed supplemental timeout retained exact legacy delivery", profile)
			continue
		}
		if metrics.QueryAttribution.Mode != team.QueryModeMixed || metrics.DeliveryRecall == nil || *metrics.DeliveryRecall != 1 || !slices.Contains(metrics.DeliveredIDs, "private-target") {
			t.Fatalf("private candidate was not delivered and attributed: %s", queryFixtureJSON(t, metrics))
		}
		found := false
		for _, source := range metrics.QueryAttribution.Sources {
			if source.Source == team.QueryPrivatePersistent {
				found = source.Mode == team.QueryModeActive && source.Fallback == team.QueryFallbackNone
			}
		}
		if !found {
			t.Fatal("actual private recall is not attributed")
		}
	}
}

func assertPrivateQueryDeadlineRollback(t *testing.T, legacy, metrics ContextRequestMetrics) {
	t.Helper()
	if !slices.Equal(legacy.DeliveredIDs, metrics.DeliveredIDs) || legacy.IrrelevantOptionalTokens != metrics.IrrelevantOptionalTokens || metrics.DeliveryRecall == nil || *metrics.DeliveryRecall != 0 || metrics.RequiredDeliveryRecall == nil || *metrics.RequiredDeliveryRecall != 1 {
		t.Fatalf("deadline changed legacy provider delivery: %s", queryFixtureJSON(t, metrics))
	}
	if legacy.RetrievalCalls == nil || metrics.RetrievalCalls == nil || metrics.RetrievalCalls.Vector != legacy.RetrievalCalls.Vector || metrics.RetrievalCalls.Exact != legacy.RetrievalCalls.Exact || metrics.RetrievalCalls.Lexical < legacy.RetrievalCalls.Lexical || metrics.RetrievalCalls.Lexical > legacy.RetrievalCalls.Lexical+3 {
		t.Fatal("deadline repeated primary/vector retrieval or exceeded three supplemental scopes")
	}
	seen := make(map[team.ContextQuerySourceKind]bool)
	for _, source := range metrics.QueryAttribution.Sources {
		if seen[source.Source] || source.Mode != team.QueryModeLegacy || source.Fallback != team.QueryFallbackTimeout || source.SupplementalHash != "" {
			t.Fatalf("partial or falsely attributed deadline rollback: %+v", source)
		}
		seen[source.Source] = true
	}
	if len(seen) != 3 || !seen[team.QuerySharedPersistent] || !seen[team.QueryPrivateSession] || !seen[team.QueryPrivatePersistent] {
		t.Fatal("deadline rollback omitted a participating source")
	}
}
