package team

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestInvocationBindingFencesAllProviderAdmissionStates(t *testing.T) {
	ctx := WithInvocationBinding(t.Context(), func(context.Context, InvocationIdentity) error { return nil })
	binding, err := claimInvocationBinding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []uint32{0, 1, 3} {
		binding.state.Store(state)
		if err := (providerRequestAdmission{}).AdmitProviderRequest(ctx, agent.ProviderRequest{}); !errors.Is(err, errInvocationUnbound) {
			t.Fatalf("state=%d error=%v", state, err)
		}
		if err := (&Coordinator{}).startProviderExecutionBoundary(ctx); !errors.Is(err, errInvocationUnbound) {
			t.Fatalf("boundary state=%d error=%v", state, err)
		}
	}
	binding.state.Store(2)
	if err := requireInvocationBinding(ctx); err != nil {
		t.Fatal(err)
	}
	if err := requireInvocationBinding(t.Context()); err != nil {
		t.Fatal("legacy invocation changed")
	}
}

func newBindingTestCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	root := t.TempDir()
	session := &TeamSession{Dir: root, Workspace: root, Config: agent.TeamConfig{Name: "binding-contract"},
		Agents: map[string]*agent.AgentDef{"worker": {Name: "worker", Role: "worker", Tools: "submit_result"}}}
	c, err := NewCoordinator(session, "http://fixture.invalid/v1", "", nil, nil, RoleModels{}, 1, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	c.SetSessionData(NewSession())
	t.Cleanup(func() { _ = c.Close() })
	if err := c.FreezeExecutionPolicyAtStartup(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInvocationBindingUsesDurableChildIdentityAndIsSingleUse(t *testing.T) {
	c := newBindingTestCoordinator(t)
	var calls atomic.Int64
	var observed InvocationIdentity
	parent := WithInvocationBinding(t.Context(), func(ctx context.Context, id InvocationIdentity) error {
		calls.Add(1)
		observed = id
		if err := requireInvocationBinding(ctx); !errors.Is(err, errInvocationUnbound) {
			t.Error("provider allowed within uncommitted callback")
		}
		return c.verifyInvocationBindingIdentity(ctx, id)
	})
	ctx, end, err := c.beginPublicInvocationExecutionRun(parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireInvocationBinding(ctx); err != nil {
		t.Fatal(err)
	}
	if observed.InvocationID == "" || observed.RunID == "" || observed.BranchID == "" || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", observed, calls.Load())
	}
	c.finalizePublicInvocationFailure(errors.New("test closes before dispatch"))
	end()
	if err := requireInvocationBinding(ctx); !errors.Is(err, errInvocationUnbound) {
		t.Fatal("closed context still admitted")
	}
	if _, end, err := c.beginPublicInvocationExecutionRun(parent); err == nil {
		end()
		t.Fatal("binding context reused")
	}
}

func TestInvocationBindingFailurePrecedesEveryPublicProviderPath(t *testing.T) {
	for _, entry := range []string{"run", "direct", "task"} {
		t.Run(entry, func(t *testing.T) {
			c := newBindingTestCoordinator(t)
			var starts atomic.Int64
			c.providerBoundaryStart = func(context.Context, string) error { starts.Add(1); return nil }
			failure := errors.New("parent journal unavailable")
			ctx := WithInvocationBinding(t.Context(), func(context.Context, InvocationIdentity) error { return failure })
			var err error
			switch entry {
			case "run":
				_, err = c.Run(ctx, "do task")
			case "direct":
				_, err = c.RunDirectAgent(ctx, "worker", "do task")
			case "task":
				_, err = c.RunTask(ctx, TaskDef{Agent: "worker", Goal: "do task"})
			}
			if !errors.Is(err, failure) || starts.Load() != 0 {
				t.Fatalf("err=%v provider starts=%d", err, starts.Load())
			}
		})
	}
}
