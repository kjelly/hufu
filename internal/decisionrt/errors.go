package decisionrt

type ErrorKind string

const (
	ErrorInvalidRequest       ErrorKind = "invalid_request"
	ErrorConfiguration        ErrorKind = "configuration"
	ErrorBackendUnavailable   ErrorKind = "backend_unavailable"
	ErrorBackendFailure       ErrorKind = "backend_failure"
	ErrorInvalidBackendOutput ErrorKind = "invalid_backend_output"
)

type RuntimeError struct {
	Kind    ErrorKind
	Backend string
	Err     error
}

func (e *RuntimeError) Error() string {
	if e == nil {
		return "decisionrt: <nil>"
	}
	return "decisionrt: " + string(e.Kind)
}

func (e *RuntimeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func runtimeError(kind ErrorKind, backend string, err error) error {
	return &RuntimeError{Kind: kind, Backend: backend, Err: err}
}
