package memory

import "context"

// MemoryService defines the interface for long-term memory operations,
// provenance management, lifecycle tracking, and hybrid retrieval.
type MemoryService interface {
	SaveRecord(ctx context.Context, rec MemoryRecord) error
	GetRecord(ctx context.Context, id string) (*MemoryRecord, error)
	ConfirmRecord(ctx context.Context, id string) error
	SupersedeRecord(ctx context.Context, newRecord MemoryRecord, targetIDs []string) error
	ExpireRecord(ctx context.Context, id string) error
	RejectRecord(ctx context.Context, id string) error
	QueryRecords(ctx context.Context, opt QueryOptions) ([]QueryResult, error)
	Close() error
}

var _ MemoryService = (*MemoryStore)(nil)
