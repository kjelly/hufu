package actionstate

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestSessionCheckpoint(t *testing.T) {
	tests := []struct {
		name     string
		seed     []byte
		want     string
		notExist bool
	}{
		{name: "no runtime store", notExist: true},
		{name: "store without a checkpoint", seed: nil, notExist: true},
		{name: "checkpoint", seed: []byte(`{"rounds":2}`), want: `{"rounds":2}`},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if i > 0 {
				store, err := runtimestore.Open(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				if tt.seed != nil {
					if err := store.PutDocument(context.Background(), runtimestore.DocumentSession, tt.seed); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			got, err := SessionCheckpoint(root)
			if tt.notExist {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("SessionCheckpoint error = %v, want os.ErrNotExist", err)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("SessionCheckpoint = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}
