package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/modelprofile"
)

func admittedMediaProviderEvents(t *testing.T) (*Coordinator, context.Context, []RunEvent) {
	t.Helper()
	c, ctx, _, request := nativeMediaProviderFixture(t)
	c.costManager = nil
	for _, id := range []string{"provider-media-first", "provider-media-second"} {
		request.InvocationID = id
		if err := (providerRequestAdmission{c: c}).CommitProviderInvocation(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	view, err := c.mediaLineage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return c, ctx, view.events
}

func TestMediaProviderAdmissionIsDurableWithoutCostManagerAndAuditable(t *testing.T) {
	c, ctx, events := admittedMediaProviderEvents(t)
	profiles := make(map[string]bool)
	count := 0
	for _, event := range events {
		if event.Type == string(EventModelProfileResolved) {
			var profile modelprofile.TelemetryProjection
			if err := json.Unmarshal(event.Payload, &profile); err != nil {
				t.Fatal(err)
			}
			profiles[profile.InvocationID] = true
		}
		if event.Type != string(EventMediaProviderAdmitted) {
			continue
		}
		payload, err := decodeMediaProviderEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if !profiles[payload.ProviderInvocationID] || payload.ImageCount != 2 || payload.ImageTokens != 16384 || payload.TextTokens <= 0 || payload.RequestTokens != payload.ImageTokens+payload.TextTokens || payload.EstimateSource != mediaOperatorEstimate {
			t.Fatal("durable image admission lost preceding profile or repeated-image estimates")
		}
		count++
	}
	if count != 2 {
		t.Fatal("missing image admissions when cost manager is disabled")
	}
	if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err != nil {
		t.Fatal(err)
	}
}

func TestMediaProviderAdmissionAuditRejectsChangedOrMissingEvidence(t *testing.T) {
	for _, variant := range []string{"profile missing", "profile denied", "target changed", "lease revision changed", "dispatch changed", "binding changed", "count changed", "capacity changed"} {
		t.Run(variant, func(t *testing.T) {
			c, ctx, original := admittedMediaProviderEvents(t)
			events := slices.Clone(original)
			if variant == "profile missing" {
				events = slices.DeleteFunc(events, func(event RunEvent) bool { return event.Type == string(EventModelProfileResolved) })
			} else if variant == "profile denied" {
				for index, event := range events {
					if event.Type != string(EventModelProfileResolved) {
						continue
					}
					var profile modelprofile.TelemetryProjection
					if err := json.Unmarshal(event.Payload, &profile); err != nil {
						t.Fatal(err)
					}
					capability := profile.Capabilities["image_input"]
					capability.State = modelprofile.CapabilityUnknown
					profile.Capabilities["image_input"] = capability
					encoded, err := json.Marshal(profile)
					if err != nil {
						t.Fatal(err)
					}
					events[index].Payload = encoded
				}
			} else {
				for index := len(events) - 1; index >= 0; index-- {
					if events[index].Type != string(EventMediaProviderAdmitted) {
						continue
					}
					payload, err := decodeMediaProviderEvent(events[index])
					if err != nil {
						t.Fatal(err)
					}
					switch variant {
					case "target changed":
						payload.ProviderConfigurationHash = "changed"
					case "lease revision changed":
						payload.OccurrenceRevision++
					case "dispatch changed":
						payload.DispatchID = "different-dispatch"
					case "binding changed":
						payload.BindingHash = "changed"
					case "count changed":
						payload.ImageCount = 1
					case "capacity changed":
						payload.ContextWindow = 1
					}
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					events[index].Payload = encoded
					break
				}
			}
			if err := VerifyNativeMediaLineage(ctx, c.session.Workspace, events); err == nil {
				t.Fatal("image provider audit trusted changed or missing canonical evidence")
			}
		})
	}
}

func TestMediaProviderAdmissionFailsBeforeTransportOnPersistenceFailure(t *testing.T) {
	for _, afterCommit := range []bool{false, true} {
		name := "before_commit"
		if afterCommit {
			name = "uncertain_after_commit"
		}
		t.Run(name, func(t *testing.T) {
			c, ctx, _, request := nativeMediaProviderFixture(t)
			request.InvocationID = "provider-media-failure"
			failure := errors.New("injected media admission persistence failure")
			original := c.EventJournal().(AtomicEventJournal)
			c.SetEventJournal(mediaTaskAppendFailure{AtomicEventJournal: original, afterCommit: afterCommit, failure: failure})
			if err := (providerRequestAdmission{c: c}).CommitProviderInvocation(ctx, request); !errors.Is(err, failure) {
				t.Fatal("failed image admission commit returned transport permission", err)
			}
		})
	}
}

func TestMediaProviderAdmissionRequiresProfileAndCurrentLease(t *testing.T) {
	c, ctx, item, request := nativeMediaProviderFixture(t)
	request.InvocationID = "provider-media-no-profile"
	if err := c.commitMediaProviderAdmission(ctx, request); err == nil {
		t.Fatal("image admission skipped the preceding profile evidence")
	}
	if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "verify", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := (providerRequestAdmission{c: c}).CommitProviderInvocation(ctx, request); err == nil {
		t.Fatal("image admission accepted a revoked attempt lease")
	}
}
