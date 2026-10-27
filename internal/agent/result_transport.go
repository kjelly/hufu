package agent

// ResultTransport selects the local worker's result protocol. Public parsing
// and the new-session default are enabled after collector integration.
type ResultTransport string

const (
	ResultTransportRuntime ResultTransport = "runtime"
	ResultTransportTool    ResultTransport = "tool"
)

func (transport ResultTransport) Valid() bool {
	return transport == ResultTransportRuntime || transport == ResultTransportTool
}
