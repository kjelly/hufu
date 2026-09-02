package improve

// AnalyticsError classifies which stage of the SQLite analytics pipeline
// failed (spec.md §18). Wrapping every analytics boundary in improve.go lets
// a caller distinguish, for example, "the execution telemetry file could not
// be read" from "the grouped-metrics query failed" via errors.As, instead of
// parsing a free-text message.
//
// Error() never includes task prompts, tool arguments, audit payloads, or
// secrets (spec.md §18) — it only names the failing operation, matching the
// existing fmt.Errorf wraps this type sits on top of.
type AnalyticsError struct {
	Stage AnalyticsErrorStage
	Err   error
}

// AnalyticsErrorStage names the pipeline boundary that failed. The set below
// is spec.md §18's list verbatim, plus load_memory: §18 predates WP-7 and
// does not enumerate a stage for event_store.jsonl ingestion, which is the
// same kind of boundary as load_execution/load_audit.
type AnalyticsErrorStage string

const (
	AnalyticsStageOpen               AnalyticsErrorStage = "open"
	AnalyticsStageSchema             AnalyticsErrorStage = "schema"
	AnalyticsStageLoadExecution      AnalyticsErrorStage = "load_execution"
	AnalyticsStageLoadAudit          AnalyticsErrorStage = "load_audit"
	AnalyticsStageLoadMemory         AnalyticsErrorStage = "load_memory"
	AnalyticsStageSelectRuns         AnalyticsErrorStage = "select_runs"
	AnalyticsStageAggregateExecution AnalyticsErrorStage = "aggregate_execution"
	AnalyticsStageAggregateGroups    AnalyticsErrorStage = "aggregate_groups"
	AnalyticsStageAttachContext      AnalyticsErrorStage = "attach_context"
	AnalyticsStageAggregateMemory    AnalyticsErrorStage = "aggregate_memory"
)

func (e *AnalyticsError) Error() string {
	return string(e.Stage) + ": " + e.Err.Error()
}

func (e *AnalyticsError) Unwrap() error {
	return e.Err
}

// newAnalyticsError tags err with stage. Returns nil when err is nil so call
// sites can wrap unconditionally: `return nil, newAnalyticsError(stage, err)`
// after an `if err != nil` still works if that check is dropped by mistake.
func newAnalyticsError(stage AnalyticsErrorStage, err error) error {
	if err == nil {
		return nil
	}
	return &AnalyticsError{Stage: stage, Err: err}
}
