package inspect

import (
	"errors"
	"testing"
)

func TestInspectQueryValidation(t *testing.T) {
	tests := []struct {
		name    string
		kind    Kind
		query   InspectQuery
		wantErr bool
	}{
		{name: "run", kind: KindRun, query: InspectQuery{RunID: "run-1"}},
		{name: "run missing id", kind: KindRun, query: InspectQuery{}, wantErr: true},
		{name: "task", kind: KindTask, query: InspectQuery{RunID: "run-1", TaskID: "task-1", Attempt: 2}},
		{name: "task missing run", kind: KindTask, query: InspectQuery{TaskID: "task-1"}, wantErr: true},
		{name: "context", kind: KindContext, query: InspectQuery{RunID: "run-1", TaskID: "task-1", ProjectID: "project"}},
		{name: "context missing project", kind: KindContext, query: InspectQuery{RunID: "run-1", TaskID: "task-1"}, wantErr: true},
		{name: "run rejects attempt", kind: KindRun, query: InspectQuery{RunID: "run-1", Attempt: 1}, wantErr: true},
		{name: "negative attempt", kind: KindTask, query: InspectQuery{RunID: "run-1", TaskID: "task-1", Attempt: -1}, wantErr: true},
		{name: "unknown kind", kind: Kind("unknown"), query: InspectQuery{}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.query.Validate(test.kind)
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantErr && !errors.Is(err, ErrInvalidQuery) {
				t.Fatalf("Validate() error = %v, want ErrInvalidQuery", err)
			}
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	if got := ExitCodeFor(nil); got != ExitSuccess {
		t.Fatalf("nil exit code = %d, want %d", got, ExitSuccess)
	}
	if got := ExitCodeFor(errors.Join(ErrIntegrity, errors.New("broken chain"))); got != ExitIntegrity {
		t.Fatalf("integrity exit code = %d, want %d", got, ExitIntegrity)
	}
	for _, err := range []error{ErrInvalidQuery, ErrNotFound, ErrAmbiguous, ErrUnauthorized} {
		if got := ExitCodeFor(err); got != ExitUsage {
			t.Fatalf("%v exit code = %d, want %d", err, got, ExitUsage)
		}
	}
}
