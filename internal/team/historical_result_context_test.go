package team

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

func historicalContextFixture(t *testing.T) (*Coordinator, *TodoItem, WorkerContextInput) {
	t.Helper()
	return configuredHistoricalContextFixture(t, func(c *Coordinator) { c.session.Config.ResultTransport = agent.ResultTransportTool })
}

func configuredHistoricalContextFixture(t *testing.T, configure func(*Coordinator)) (*Coordinator, *TodoItem, WorkerContextInput) {
	t.Helper()
	c, rows, _ := historicalGrantPreparationFixture(t)
	configure(c)
	var err error
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + (*rows)[0].ResultID})
	if err != nil {
		t.Fatal(err)
	}
	c, _, _, _ = historicalGrantPreparedReplayFixture(t, c, prepared)
	c.executionRunID = "consumer-cycle"
	spec := historicalBoundSpecs(t, c, []TodoSpec{{Agent: "review worker", Desc: "consume historical reference", Goal: "consume historical reference",
		Source: TaskSourceCoordinator, ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: "test"},
		Execution: ExecutionContract{RequiresResult: true}}}, []string{"1"})[0]
	projection, err := taskOccurrenceProjectionFromSpec(spec, "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.admitTaskOccurrence(t.Context(), projection, "1", 1); err != nil {
		t.Fatal(err)
	}
	items, err := c.CommitTaskCreationResolved(t.Context(), []TodoSpec{spec}, []string{"1"})
	if err != nil || len(items) != 1 {
		t.Fatalf("create bound context task: %d, %v", len(items), err)
	}
	item := items[0]
	task := taskDefFromTodoItem(item)
	input := buildWorkerContextInput(c.newTaskContextRequest(task, item.ID, 1, ContextTriggerTaskDispatch, "review worker", "worker", nil),
		task, c.session.Agents["review worker"], "", "", "", "", nil)
	input.ModelContext = ModelContextSpec{ModelID: "test", ContextWindow: 32_768, MaxOutputTokens: 256, SafetyMarginTokens: 64, Estimator: "estimated"}
	input.DisableMemory, input.DisableCanonicalMemory = true, true
	return c, item, input
}

func historicalContextManifest(t *testing.T, c *Coordinator, item *TodoItem, input WorkerContextInput) (CompiledContext, ContextInjectionManifest) {
	t.Helper()
	compiled, err := c.compileWorkerContext(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, item.Agent, time.Now(), c.session.Config.MemoryLearning)
	if err != nil {
		t.Fatal(err)
	}
	return compiled, manifest
}

func TestHistoricalResultContextIncludesCompleteFrozenPresentation(t *testing.T) {
	c, item, input := historicalContextFixture(t)
	compiled, manifest := historicalContextManifest(t, c, item, input)
	for _, want := range []string{"Historical reference data only", "complete deliverable", "future_detail", "9007199254740993", "all diagnostics", "1500000000"} {
		if !strings.Contains(compiled.Prompt, want) {
			t.Fatalf("complete historical context lost %q", want)
		}
	}
	if strings.Contains(compiled.Prompt, "RAW_OUTPUT_MUST_NOT_BE_IMPORTED") || manifest.ModelCalled || manifest.Outcome != "prepared" || manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
		t.Fatalf("historical context imported raw output or claimed dispatch: %+v", manifest)
	}
	if err := ValidateContextQueryManifest(manifest); err != nil {
		t.Fatal(err)
	}
	summary := SummarizeContextManifests([]ContextInjectionManifest{manifest})
	if len(summary.HistoricalResults) != 1 || summary.HistoricalResults[0].ModelCalled || summary.HistoricalResults[0].Attribution.Source.PresentationBytes <= 0 {
		t.Fatalf("historical prepared summary = %+v", summary)
	}
	clone := cloneContextInjectionManifest(&manifest)
	for i := range clone.Items {
		if clone.Items[i].HistoricalResult != nil {
			clone.Items[i].HistoricalResult.Binding.ResultIDs[0] = "forged"
		}
	}
	if reflect.DeepEqual(clone.Items, manifest.Items) {
		t.Fatal("cloned historical attribution retained the live binding slice")
	}
}

func TestHistoricalResultContextFailsClosedOnSourceDrift(t *testing.T) {
	for _, failure := range []string{"body", "payload", "invalidated", "missing", "request_agent", "request_role", "request_attempt"} {
		t.Run(failure, func(t *testing.T) {
			c, _, input := historicalContextFixture(t)
			reader := c.HistoricalTaskReader()
			c.SetHistoricalTaskReader(historicalTaskReaderFunc(func(ctx context.Context, scope HistoricalTaskScope) ([]HistoricalTaskResult, error) {
				rows, err := reader.Load(ctx, scope)
				if err != nil || len(rows) == 0 {
					return rows, err
				}
				rows = append([]HistoricalTaskResult(nil), rows...)
				switch failure {
				case "body":
					rows[0].TypedResultJSON = []byte(strings.Replace(string(rows[0].TypedResultJSON), "complete deliverable", "replaced deliverable", 1))
				case "payload":
					rows[0].PayloadSHA256 = strings.Repeat("0", 64)
				case "invalidated":
					rows[0].Validity = "invalidated"
				case "missing":
					return nil, nil
				}
				return rows, nil
			}))
			switch failure {
			case "request_agent":
				input.Request.AgentName = "observer"
			case "request_role":
				input.Request.AgentRole = "judge"
			case "request_attempt":
				input.Request.Attempt++
			}
			input.Request.AssignRequestID()
			if _, err := c.compileWorkerContext(t.Context(), input); err == nil {
				t.Fatal("runtime replaced a required source or widened its recipient")
			}
		})
	}
}

type historicalContextTamperCompiler struct {
	ContextCompiler
	tamper func(*CompiledContext)
}

func (compiler historicalContextTamperCompiler) CompileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error) {
	compiled, err := compiler.ContextCompiler.CompileWorkerContext(ctx, input)
	if err == nil {
		compiler.tamper(&compiled)
	}
	return compiled, err
}

func TestHistoricalResultContextRejectsCompilerOmissionAndCompression(t *testing.T) {
	for _, failure := range []string{"omit", "compress", "body", "authority", "attribution", "prompt"} {
		t.Run(failure, func(t *testing.T) {
			c, _, input := historicalContextFixture(t)
			c.SetContextCompiler(historicalContextTamperCompiler{ContextCompiler: c.ContextCompiler(), tamper: func(compiled *CompiledContext) {
				for i := range compiled.IncludedItems {
					item := &compiled.IncludedItems[i]
					if item.HistoricalResult == nil {
						continue
					}
					switch failure {
					case "omit":
						compiled.IncludedItems = append(compiled.IncludedItems[:i], compiled.IncludedItems[i+1:]...)
					case "compress":
						item.Compressed = true
					case "body":
						item.Content = "summary only"
					case "authority":
						item.Authority = ContextAuthorityNormative
					case "attribution":
						item.HistoricalResult.Source.PayloadSHA256 = strings.Repeat("0", 64)
					case "prompt":
						compiled.Prompt = "omitted required context"
					}
					return
				}
			}})
			if _, err := c.compileWorkerContext(t.Context(), input); err == nil {
				t.Fatal("compiler changed required historical data")
			}
		})
	}
}

func TestHistoricalResultContextBudgetIsRequired(t *testing.T) {
	c, _, input := historicalContextFixture(t)
	input.ModelContext.ContextWindow = 64
	input.ModelContext.MaxOutputTokens, input.ModelContext.SafetyMarginTokens = 1, 1
	_, err := c.compileWorkerContext(t.Context(), input)
	if _, ok := errors.AsType[*RequiredContextBudgetError](err); !ok {
		t.Fatalf("required historical context was truncated or omitted: %v", err)
	}
}
