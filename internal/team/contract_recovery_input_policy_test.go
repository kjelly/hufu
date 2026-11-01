package team

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/golangruntime"
)

func configureResolverPolicyRecovery(c *Coordinator) {
	configureInputBoundContractRecovery(c)
	c.session.RunInputDefinitions[0].Resolver = &RunInputResolverSpec{ID: "scope-v1", Capability: "resolve-scope", Type: "scope", Source: "invocation_prompt", SideEffect: "none", Timeout: 5}
	c.session.Config.ActionProviders = map[string]agent.ActionProviderConfig{"resolve-scope": {Runtime: "golang", Source: "./resolver", Mode: golangruntime.TrustedStaticMode}}
	c.session.ProviderRegistry = NewProviderRegistry()
	c.session.ProviderRegistry.Register("resolve-scope", &golangActionProvider{capability: "resolve-scope", program: golangruntime.Program{Digest: runInputHash([]byte("original source"))}})
}

func changeResolverPolicy(c *Coordinator) {
	c.session.ProviderRegistry.Register("resolve-scope", &golangActionProvider{capability: "resolve-scope", program: golangruntime.Program{Digest: runInputHash([]byte("fixed source"))}})
}

func TestContractRecoveryInputPolicyPreservesFrozenEvidence(t *testing.T) {
	for _, test := range []struct {
		name      string
		metadata  bool
		inputOnly bool
	}{
		{name: "versioned"},
		{name: "metadata", metadata: true},
		{name: "input_only", metadata: true, inputOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var c *Coordinator
			var f *versionFixture
			var source *ResumeSource
			ctx := WithFrozenInputPolicyRecovery(t.Context())
			if test.metadata {
				c, f, source = metadataContractRecoveryFixture(t, configureResolverPolicyRecovery)
				ctx = WithMetadataOnlyContractRecovery(ctx)
			} else {
				c, f, source = contractRecoveryFixture(t, configureResolverPolicyRecovery)
			}
			if test.inputOnly {
				original, err := CompileResultContractDocument("result.json", []byte(`{"type":"object","required":["value"],"properties":{"value":{"type":"string"}},"additionalProperties":false}`))
				if err != nil {
					t.Fatal(err)
				}
				c.session.ResultContracts[original.ID] = original
				ref := original.ref(true)
				for i := range c.session.ContractTasks {
					c.session.ContractTasks[i].ResultContract = &ref
				}
			}
			changeResolverPolicy(c)
			before := mustReadEvents(t, f.events)
			if _, err := c.AssessContractRecovery(t.Context(), source); err == nil {
				t.Fatal("resolver source upgrade admitted without explicit selection")
			}
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if plan.InputPolicyUpgrade == nil || plan.Retained != 1 || plan.Retried != 2 || !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatalf("invalid or mutating preview: %+v", plan)
			}
			if _, err := c.ApplyContractRecovery(ctx, source, "wrong-plan", strings.Repeat("0", 64)); err == nil {
				t.Fatal("stale plan accepted")
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "policy-recovery", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(f.control, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			original, retained := source.Session.Tasks[0], child.Session.Tasks[0]
			if !child.inputPolicyRecovery || !sameCanonicalEvidence(original.ExecutionReceipts, retained.ExecutionReceipts) || !sameCanonicalEvidence(original.ExecutionReceipt, retained.ExecutionReceipt) ||
				!sameCanonicalEvidence(original.TypedResult.RuntimeOutputs, retained.TypedResult.RuntimeOutputs) || original.Action.Payload != retained.Action.Payload ||
				original.ContractHash == retained.ContractHash || !sameCanonicalEvidence(source.Session.RunInputSnapshots, child.Session.RunInputSnapshots) {
				t.Fatal("recovery lost the policy proof, input identity or original native evidence")
			}
			parent, err := ReadResumeSource(f.control, source.BranchID, "")
			if err != nil || !sameCanonicalEvidence(parent.Session.Tasks, source.Session.Tasks) || !sameCanonicalEvidence(parent.Session.ExecutionPolicySnapshot, source.Session.ExecutionPolicySnapshot) {
				t.Fatal("recovery changed the parent")
			}
			// A fresh coordinator must use ordinary admission, without either
			// recovery opt-in, and reuse the original input-bound native result.
			fresh := newTestCoordinator(t)
			fresh.session, fresh.projectDir = c.session, f.subject
			fresh.providerManager, fresh.maxConcurrent = c.providerManager, c.maxConcurrent
			fresh.SetSessionData(child.Session)
			fresh.executionRunID = "ordinary-policy-resume"
			if err := fresh.prepareRecoveryValidation(); err != nil {
				t.Fatal(err)
			}
			if err := fresh.AdmitExecutionPolicy(); err != nil {
				t.Fatal(err)
			}
			if err := fresh.resolveRunInputsForInvocation(WithSessionResume(t.Context()), "continue"); err != nil {
				t.Fatal(err)
			}
			if err := fresh.validateRecoveredArtifactOrigin(retained.TypedResult.Artifacts[0], retained.ID, retained.TypedResult); err != nil {
				t.Fatalf("retained artifact origin: %v", err)
			}
			if err := fresh.validateResumeSources(WithSessionResume(t.Context())); err != nil {
				t.Fatal(err)
			}
			verification, err := fresh.executeTaskOutputAssertVerification(t.Context(), VerificationSpec{Type: VerifyTaskOutputAssert, WorksetSourceTask: "source", TaskOutputName: "scope", Assertions: []TaskOutputAssertion{{Pointer: "/satisfied", Op: "equals", Value: true}}})
			if err != nil || verification.ExitCode != 0 {
				t.Fatalf("recovered native output failed acceptance: %+v %v", verification, err)
			}
			fresh.session.ContractTasks[0].SideEffect = SideEffectExternalWrite
			if err := fresh.validateResumeSources(WithSessionResume(t.Context())); err == nil {
				t.Fatal("ordinary resume lost the read-only recovery restriction")
			}
		})
	}
}

func TestContractRecoveryInputPolicyRejectsAuthorityDrift(t *testing.T) {
	for _, name := range []string{"model", "schema", "default", "action", "future_write", "dynamic", "mutation"} {
		t.Run(name, func(t *testing.T) {
			c, f, source := metadataContractRecoveryFixture(t, configureResolverPolicyRecovery)
			changeResolverPolicy(c)
			switch name {
			case "model":
				c.session.Agents["worker"].Generation.Model = "ollama/other"
			case "schema":
				c.session.RunInputDefinitions[0].Schema.AdditionalProperties = new(false)
			case "default":
				c.session.RunInputDefinitions[0].Default = []byte(`{"count":99}`)
			case "action":
				c.session.ContractTasks[0].Action.Payload = `{"scope":{"count":1},"limit":99}`
			case "future_write":
				c.session.ContractTasks[0].SideEffect = SideEffectExternalWrite
			case "dynamic":
				c.session.Config.Delegation.BindTaskGoalContracts = false
			case "mutation":
				c.session.ContractTasks[0].Execution.Steps = []ExecutionStep{{ID: "mutate", Effect: ExecutionEffectMutate}}
			}
			before := mustReadEvents(t, f.events)
			ctx := WithFrozenInputPolicyRecovery(t.Context())
			if _, err := c.AssessContractRecovery(ctx, source); err == nil {
				t.Fatal("unsafe execution or input authority accepted")
			}
			if !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
				t.Fatal("refused preview changed history")
			}
		})
	}
}

func TestContractRecoveryInputPolicyReplayRejectsSubstitution(t *testing.T) {
	c, f, source := metadataContractRecoveryFixture(t, configureResolverPolicyRecovery)
	changeResolverPolicy(c)
	ctx := WithFrozenInputPolicyRecovery(t.Context())
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"valid", "missing_proof", "old_policy", "new_policy", "snapshot_id", "snapshot_hash", "schema", "source_write", "source_inputs", "input_bytes"} {
		t.Run(name, func(t *testing.T) {
			copy := *plan
			copy.Policy = cloneExecutionPolicySnapshot(plan.Policy)
			copy.InputPolicyUpgrade = new(*plan.InputPolicyUpgrade)
			preceding := mustReadEvents(t, f.events)
			switch name {
			case "missing_proof":
				copy.InputPolicyUpgrade = nil
			case "old_policy":
				copy.InputPolicyUpgrade.OriginalSHA256 = runInputHash([]byte("other"))
			case "new_policy":
				copy.InputPolicyUpgrade.AcceptedSHA256 = runInputHash([]byte("other"))
			case "snapshot_id":
				copy.InputPolicyUpgrade.SnapshotID = "foreign"
			case "snapshot_hash":
				copy.InputPolicyUpgrade.SnapshotSHA256 = runInputHash([]byte("other"))
			case "schema":
				copy.Policy.RunInputSchemaHash = runInputHash([]byte("other"))
			case "source_write", "source_inputs":
				item := cloneTodoItem(source.Session.Tasks[0])
				if name == "source_write" {
					item.SideEffect = SideEffectExternalWrite
				} else {
					item = cloneTodoItem(source.Session.Tasks[1])
					item.RunInputSnapshotHash = runInputHash([]byte("other"))
				}
				for i := range preceding {
					if preceding[i].Type == string(EventTaskCreated) && preceding[i].TaskID == item.ID {
						preceding[i].Payload = mustProjectionJSON(t, item)
					}
				}
				if name == "source_inputs" {
					// A later task snapshot can restore input fields from the
					// creation event. Substitute the latest canonical observation.
					preceding = append(preceding, RunEvent{SchemaVersion: eventStoreSchemaVersion, Type: string(EventTaskStarted), ID: "substituted-inputs", TaskID: item.ID, Payload: mustProjectionJSON(t, item)})
				}
			case "input_bytes":
				snapshot := CloneRunInputSnapshot(&source.Session.RunInputSnapshots[0])
				snapshot.Inputs[0].CanonicalValue = []byte(`{"count":99}`)
				for i := range preceding {
					if preceding[i].Type == string(EventRunInputsResolved) {
						preceding[i].Payload = mustProjectionJSON(t, snapshot)
					}
				}
			}
			copy.Policy.ConfigurationHash, err = executionPolicyConfigurationHash(copy.Policy)
			if err != nil {
				t.Fatal(err)
			}
			copy.SHA256, err = recoveryPlanHash(copy)
			if err != nil {
				t.Fatal(err)
			}
			event := RunEvent{SchemaVersion: eventStoreSchemaVersion, BranchID: "recovered", Payload: mustProjectionJSON(t, copy)}
			_, err := validateContractRecoveryLineage(event, preceding, source.Session.ExecutionPolicySnapshot)
			if (err == nil) != (name == "valid") {
				t.Fatalf("substitution %s: %v", name, err)
			}
		})
	}
}
