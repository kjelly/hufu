package team

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

func TestMediaProviderTransportRequiresExactCurrentDurablePermission(t *testing.T) {
	for _, scenario := range []string{"allowed", "not committed", "different invocation", "changed text", "changed capacity", "revoked lease", "cancelled", "text compatibility"} {
		t.Run(scenario, func(t *testing.T) {
			c, ctx, item, request := nativeMediaProviderFixture(t)
			admission := providerRequestAdmission{c: c}
			request.InvocationID = "transport-fixture"
			if scenario != "not committed" {
				if err := admission.CommitProviderInvocation(ctx, request); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "different invocation":
				request.InvocationID = "other-invocation"
			case "changed text":
				request.Call.Prompt[0].Content[0] = fantasy.TextPart{Text: "This changed prompt has a substantially different token count and was never admitted to this provider invocation."}
			case "changed capacity":
				request.AdmissionContext.ContextWindow++
			case "revoked lease":
				if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "verifying", "", nil); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "text compatibility":
				request = agent.ProviderRequest{Call: &fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("text")}}}
			}
			err := admission.AuthorizeProviderTransport(ctx, request)
			if scenario == "allowed" || scenario == "text compatibility" {
				if err != nil {
					t.Fatal("valid transport permission rejected", err)
				}
				return
			}
			if scenario == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancelled transport lost its cancellation", err)
				}
			} else if _, ok := errors.AsType[*media.Error](err); !ok {
				t.Fatal("missing or changed permission did not fail closed", err)
			}
		})
	}
}

type mediaTransportReadHook struct {
	AtomicEventJournal
	afterRead func()
}

func (j *mediaTransportReadHook) ReadEvents(ctx context.Context) ([]RunEvent, error) {
	events, err := j.AtomicEventJournal.ReadEvents(ctx)
	if err == nil && j.afterRead != nil {
		hook := j.afterRead
		j.afterRead = nil
		hook()
	}
	return events, err
}

func TestMediaProviderTransportRechecksLeaseAfterJournalRead(t *testing.T) {
	c, ctx, item, request := nativeMediaProviderFixture(t)
	request.InvocationID = "transport-journal-wait"
	admission := providerRequestAdmission{c: c}
	if err := admission.CommitProviderInvocation(ctx, request); err != nil {
		t.Fatal(err)
	}
	c.SetEventJournal(&mediaTransportReadHook{AtomicEventJournal: c.EventJournal().(AtomicEventJournal), afterRead: func() {
		if err := c.CommitTaskTransition(ctx, item.ID, TaskInProgress, TaskVerifying, "revoked while reading journal", "", nil); err != nil {
			t.Fatal(err)
		}
	}})
	requireMediaCode(t, admission.AuthorizeProviderTransport(ctx, request), media.AccessDenied)
}
