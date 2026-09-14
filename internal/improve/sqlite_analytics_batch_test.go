package improve

import "testing"

func TestAnalyticsBatchRowsHonorsVariableLimit(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		columns   int
		candidate int
		want      int
		wantErr   bool
	}{
		{name: "candidate", limit: 999, columns: 26, candidate: 32, want: 32},
		{name: "variable cap", limit: 999, columns: 26, candidate: 256, want: 38},
		{name: "invalid limit", limit: 0, columns: 1, candidate: 1, wantErr: true},
		{name: "invalid columns", limit: 999, columns: 0, candidate: 1, wantErr: true},
		{name: "invalid candidate", limit: 999, columns: 1, candidate: 0, wantErr: true},
		{name: "row cannot fit", limit: 3, columns: 4, candidate: 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := analyticsBatchRows(test.limit, test.columns, test.candidate)
			if test.wantErr {
				if err == nil {
					t.Fatalf("analyticsBatchRows() = %d, want error", got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("analyticsBatchRows() = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestSQLiteMaxVariableNumberIsPositive(t *testing.T) {
	session := newTestSession(t)
	limit, err := sqliteMaxVariableNumber(t.Context(), session.conn)
	if err != nil {
		t.Fatal(err)
	}
	if limit < fallbackSQLiteVariableLimit {
		t.Fatalf("SQLite variable limit = %d, want at least fallback %d", limit, fallbackSQLiteVariableLimit)
	}
}
