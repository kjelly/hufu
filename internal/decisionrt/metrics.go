package decisionrt

type Metrics interface {
	IncCalls(purpose, backend string)
	IncDecided(purpose, backend string)
	IncAbstained(purpose, backend, reasonCode string)
	IncErrors(purpose, backend string)
	IncFallback(purpose, backend string)
	ObserveDurationMS(purpose, backend string, ms uint64)
}

type noopMetrics struct{}

func (noopMetrics) IncCalls(string, string)                  {}
func (noopMetrics) IncDecided(string, string)                {}
func (noopMetrics) IncAbstained(string, string, string)      {}
func (noopMetrics) IncErrors(string, string)                 {}
func (noopMetrics) IncFallback(string, string)               {}
func (noopMetrics) ObserveDurationMS(string, string, uint64) {}
