package improve

import "time"

type analyticsDiagnosticStage uint8

const (
	diagnosticOpen analyticsDiagnosticStage = iota
	diagnosticLoadExecution
	diagnosticLoadAudit
	diagnosticLoadMemory
	diagnosticBuildIndexes
	diagnosticSelectRuns
	diagnosticProjectTasks
	diagnosticAggregateExecution
	diagnosticAggregateAudit
	diagnosticAggregateMemory
	diagnosticAggregateGroups
	diagnosticAggregateTrend
	diagnosticTotal
)

// AnalyticsDiagnostics records internal pipeline work for benchmarks and
// regression investigations. It is deliberately excluded from Report so
// instrumentation cannot change the stable public result contract.
type AnalyticsDiagnostics struct {
	ExecutionLinesRead int64
	ExecutionRows      int64
	AuditLinesRead     int64
	AuditRows          int64
	MemoryRows         int64

	SelectedRuns   int
	ProjectedTasks int

	Open               time.Duration
	LoadExecution      time.Duration
	LoadAudit          time.Duration
	LoadMemory         time.Duration
	BuildIndexes       time.Duration
	SelectRuns         time.Duration
	ProjectTasks       time.Duration
	AggregateExecution time.Duration
	AggregateAudit     time.Duration
	AggregateMemory    time.Duration
	AggregateGroups    time.Duration
	AggregateTrend     time.Duration
	Total              time.Duration
}

func (d *AnalyticsDiagnostics) reset() {
	if d != nil {
		*d = AnalyticsDiagnostics{}
	}
}

func (d *AnalyticsDiagnostics) record(stage analyticsDiagnosticStage, started time.Time) {
	if d == nil {
		return
	}
	duration := time.Since(started)
	switch stage {
	case diagnosticOpen:
		d.Open += duration
	case diagnosticLoadExecution:
		d.LoadExecution += duration
	case diagnosticLoadAudit:
		d.LoadAudit += duration
	case diagnosticLoadMemory:
		d.LoadMemory += duration
	case diagnosticBuildIndexes:
		d.BuildIndexes += duration
	case diagnosticSelectRuns:
		d.SelectRuns += duration
	case diagnosticProjectTasks:
		d.ProjectTasks += duration
	case diagnosticAggregateExecution:
		d.AggregateExecution += duration
	case diagnosticAggregateAudit:
		d.AggregateAudit += duration
	case diagnosticAggregateMemory:
		d.AggregateMemory += duration
	case diagnosticAggregateGroups:
		d.AggregateGroups += duration
	case diagnosticAggregateTrend:
		d.AggregateTrend += duration
	case diagnosticTotal:
		d.Total += duration
	}
}
