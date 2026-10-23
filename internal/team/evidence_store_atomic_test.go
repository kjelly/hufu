package team

import (
	"bytes"
	"sync"
	"testing"
)

func TestArtifactStoreConcurrentPublication(t *testing.T) {
	workspace := t.TempDir()
	content := bytes.Repeat([]byte("immutable evidence\n"), 64*1024)
	const writers = 8
	results := make(chan ArtifactPutResult, writers)
	failures := make(chan error, writers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range writers {
		wait.Go(func() {
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				failures <- err
				return
			}
			<-start
			result, err := store.Put(t.Context(), PutArtifactRequest{
				Kind: "test", Path: "evidence.txt", Content: content,
			})
			if err == nil {
				err = store.Verify(t.Context(), result.ArtifactRef)
			}
			failures <- err
			results <- result
		})
	}
	close(start)
	wait.Wait()
	close(failures)
	close(results)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	dataCreated, metadataCreated := 0, 0
	var id string
	for result := range results {
		if id != "" && id != result.ID {
			t.Fatal("identical content received distinct artifact IDs")
		}
		id = result.ID
		if result.CreatedData {
			dataCreated++
		}
		if result.CreatedMetadata {
			metadataCreated++
		}
	}
	if dataCreated != 1 || metadataCreated != 1 {
		t.Fatalf("created data/metadata = %d/%d, want 1/1", dataCreated, metadataCreated)
	}
}
