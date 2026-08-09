package memory

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/philippgille/chromem-go"

	"github.com/kjelly/hufu/internal/config"
)

const collectionName = "memory"
const metaFileName = "embedding_meta.json"

type Result struct {
	ID         string
	Content    string
	Similarity float32
	Metadata   map[string]string
}

type QueryOptions struct {
	Query          string
	N              int
	Category       string
	Project        string
	Team           string
	TaskID         string
	FilePaths      []string
	MinConfidence  float64
	IncludeStatus  []string          // If empty, defaults to excluding superseded, expired, and rejected
	MetadataFilter map[string]string // Additional chromem metadata key-value filters (AND logic) for backward compat (R1)
}

type QueryResult struct {
	Record     MemoryRecord
	Similarity float32
	Score      float64
}

type MemoryStore struct {
	db         *chromem.DB
	collection *chromem.Collection
	embedFunc  chromem.EmbeddingFunc
	mu         sync.RWMutex
	basePath   string

	// lazy init fields
	initOnce    sync.Once
	initErr     error
	storePath   string
	embedModel  string
	ollamaURL   string
	initialized bool
}

type embeddingMeta struct {
	EmbeddingModel string `json:"embedding_model"`
	CreatedAt      string `json:"created_at"`
}

func projectDirHash(projectDir string) string {
	h := sha256.Sum256([]byte(projectDir))
	return fmt.Sprintf("%x", h)[:16]
}

func dataDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("failed to get home directory: %w", err)
	}
	return filepath.Join(homeDir, ".local", "share", "hufu", "memory"), nil
}

func NewMemoryStore(projectDir, ollamaURL, embedModel string) (*MemoryStore, error) {
	return newLazyMemoryStore(projectDir, ollamaURL, embedModel, false)
}

func NewGlobalMemoryStore(ollamaURL, embedModel string) (*MemoryStore, error) {
	return newLazyMemoryStore("", ollamaURL, embedModel, true)
}

func newLazyMemoryStore(projectDir, ollamaURL, embedModel string, isGlobal bool) (*MemoryStore, error) {
	if ollamaURL == "" {
		ollamaURL = config.DefaultOllamaAPIURL
	}
	if embedModel == "" {
		embedModel = config.DefaultEmbeddingModel
	}

	embedFunc := chromem.NewEmbeddingFuncOllama(embedModel, ollamaURL)

	basePath, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("failed to determine memory data directory: %w", err)
	}

	var storePath string
	if isGlobal {
		storePath = filepath.Join(basePath, "_global")
	} else {
		hash := projectDirHash(projectDir)
		storePath = filepath.Join(basePath, hash)
	}

	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create memory store directory %s: %w", storePath, err)
	}

	return &MemoryStore{
		embedFunc:  embedFunc,
		basePath:   basePath,
		storePath:  storePath,
		embedModel: embedModel,
		ollamaURL:  ollamaURL,
	}, nil
}

func (s *MemoryStore) init() error {
	s.initOnce.Do(func() {
		s.initErr = s.doInit()
	})
	return s.initErr
}

func (s *MemoryStore) doInit() error {
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer probeCancel()
	if err := probeEmbeddingModel(probeCtx, s.embedFunc); err != nil {
		return fmt.Errorf("embedding model %q is not available: %w", s.embedModel, err)
	}

	db, err := chromem.NewPersistentDB(s.storePath, true)
	if err != nil {
		return fmt.Errorf("failed to create persistent memory database: %w", err)
	}

	mismatch, err := checkEmbeddingModelMismatch(db, s.storePath, s.embedModel)
	if err != nil {
		log.Printf("warning: failed to check embedding model mismatch: %v", err)
	}
	if mismatch {
		log.Printf("embedding model changed; previous collection was deleted and will be recreated")
	}

	collection, err := db.GetOrCreateCollection(collectionName, map[string]string{"embedding_model": s.embedModel}, s.embedFunc)
	if err != nil {
		return fmt.Errorf("failed to create memory collection: %w", err)
	}

	s.db = db
	s.collection = collection
	s.initialized = true
	return nil
}

// SaveRecord stores a MemoryRecord with full provenance, status, and confidence (§20.1).
func (s *MemoryStore) SaveRecord(ctx context.Context, rec MemoryRecord) error {
	if err := s.init(); err != nil {
		return err
	}
	if rec.ID == "" {
		rec.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	if rec.Status == "" {
		rec.Status = StatusCandidate
	}
	if rec.Confidence <= 0 {
		if rec.Status == StatusConfirmed {
			rec.Confidence = 1.0
		} else {
			rec.Confidence = 0.8
		}
	}

	meta, err := recordToMetadata(rec)
	if err != nil {
		return err
	}

	doc := chromem.Document{
		ID:       rec.ID,
		Content:  rec.Content,
		Metadata: meta,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.AddDocuments(ctx, []chromem.Document{doc}, runtime.NumCPU())
}

// Save is a backward-compatible wrapper that stores content and metadata as a confirmed MemoryRecord.
// Unknown metadata keys (not part of the MemoryRecord schema) are preserved in ExtraMeta for
// chromem filter compatibility (R1).
func (s *MemoryStore) Save(ctx context.Context, id, content string, metadata map[string]string) error {
	if metadata == nil {
		metadata = make(map[string]string)
	}
	cat := metadata["category"]
	st := metadata["status"]
	if st == "" {
		st = StatusConfirmed
	}

	// Known schema keys mapped to MemoryRecord fields
	knownKeys := map[string]bool{
		"category": true, "status": true, "project": true, "team": true,
		"source_task_id": true, "source_agent": true, "commit_hash": true,
		"saved_at": true,
	}

	rec := MemoryRecord{
		ID:           id,
		Content:      content,
		Category:     cat,
		Project:      metadata["project"],
		Team:         metadata["team"],
		SourceTaskID: metadata["source_task_id"],
		SourceAgent:  metadata["source_agent"],
		CommitHash:   metadata["commit_hash"],
		Status:       st,
		Confidence:   1.0,
	}

	if savedAtStr, ok := metadata["saved_at"]; ok && savedAtStr != "" {
		if t, err := time.Parse(time.RFC3339, savedAtStr); err == nil {
			rec.CreatedAt = t
		}
	}

	// Preserve unknown metadata keys in ExtraMeta (R1)
	for k, v := range metadata {
		if !knownKeys[k] && v != "" {
			if rec.ExtraMeta == nil {
				rec.ExtraMeta = make(map[string]string)
			}
			rec.ExtraMeta[k] = v
		}
	}

	return s.SaveRecord(ctx, rec)
}

// GetRecord retrieves a single MemoryRecord by ID.
func (s *MemoryStore) GetRecord(ctx context.Context, id string) (*MemoryRecord, error) {
	if err := s.init(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Use chromem GetByID for direct lookup (R3: was similarity query workaround)
	doc, err := s.collection.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("memory record with id %q not found: %w", id, err)
	}

	rec, err := metadataToRecord(doc.ID, doc.Content, doc.Metadata)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// ConfirmRecord updates a record's lifecycle status to confirmed.
func (s *MemoryStore) ConfirmRecord(ctx context.Context, id string) error {
	rec, err := s.GetRecord(ctx, id)
	if err != nil {
		return err
	}
	rec.Status = StatusConfirmed
	rec.LastConfirmedAt = time.Now()
	if rec.Confidence < 1.0 {
		rec.Confidence = 1.0
	}
	return s.SaveRecord(ctx, *rec)
}

// SupersedeRecord marks target records as superseded and links them to the new record.
func (s *MemoryStore) SupersedeRecord(ctx context.Context, newRecord MemoryRecord, targetIDs []string) error {
	newRecord.Supersedes = targetIDs
	if newRecord.Status == "" {
		newRecord.Status = StatusConfirmed
	}
	// Accumulate errors when marking targets as superseded (R2: was silently ignored)
	var firstErr error
	for _, targetID := range targetIDs {
		rec, err := s.GetRecord(ctx, targetID)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to retrieve target record %q for supersession: %w", targetID, err)
			}
			continue
		}
		if rec != nil {
			rec.Status = StatusSuperseded
			if err := s.SaveRecord(ctx, *rec); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("failed to mark target record %q as superseded: %w", targetID, err)
				}
			}
		}
	}
	if err := s.SaveRecord(ctx, newRecord); err != nil {
		return err
	}
	return firstErr
}

// ExpireRecord marks a record as expired.
func (s *MemoryStore) ExpireRecord(ctx context.Context, id string) error {
	rec, err := s.GetRecord(ctx, id)
	if err != nil {
		return err
	}
	rec.Status = StatusExpired
	now := time.Now()
	rec.ExpiresAt = &now
	return s.SaveRecord(ctx, *rec)
}

// RejectRecord marks a candidate record as rejected.
func (s *MemoryStore) RejectRecord(ctx context.Context, id string) error {
	rec, err := s.GetRecord(ctx, id)
	if err != nil {
		return err
	}
	rec.Status = StatusRejected
	return s.SaveRecord(ctx, *rec)
}

// calculateHybridScore computes the ranking score for a memory record (§20.4).
func calculateHybridScore(r *chromem.Result, rec *MemoryRecord, opt QueryOptions) float64 {
	baseScore := float64(r.Similarity)
	bonus := 0.0

	// Lexical match bonus
	queryTokens := strings.Fields(strings.ToLower(opt.Query))
	contentLower := strings.ToLower(rec.Content)
	lexicalHits := 0
	for _, tok := range queryTokens {
		if len(tok) > 2 && strings.Contains(contentLower, tok) {
			lexicalHits++
		}
	}
	if lexicalHits > 0 {
		bonus += float64(lexicalHits) * 0.05
		if bonus > 0.15 {
			bonus = 0.15
		}
	}

	// Recency bonus
	if !rec.CreatedAt.IsZero() && time.Since(rec.CreatedAt) < 7*24*time.Hour {
		bonus += 0.05
	}
	if !rec.LastConfirmedAt.IsZero() && time.Since(rec.LastConfirmedAt) < 7*24*time.Hour {
		bonus += 0.05
	}

	// File relevance bonus
	if len(opt.FilePaths) > 0 && len(rec.FilePaths) > 0 {
		if filePathsOverlap(opt.FilePaths, rec.FilePaths) {
			bonus += 0.10
		}
	}

	// Task relevance bonus
	if opt.TaskID != "" && rec.SourceTaskID == opt.TaskID {
		bonus += 0.10
	}

	// Status bonus & penalty
	switch rec.EffectiveStatus() {
	case StatusConfirmed:
		bonus += 0.10
	case StatusCandidate:
		// no change
	case StatusSuperseded, StatusExpired:
		bonus -= 0.50
	case StatusRejected:
		bonus -= 1.00
	}

	score := baseScore + bonus
	if rec.Confidence > 0 {
		score *= rec.Confidence
	}
	return score
}

// filePathsOverlap checks if any path in req overlaps with any path in rec.
func filePathsOverlap(req, rec []string) bool {
	for _, reqPath := range req {
		for _, recPath := range rec {
			if reqPath == recPath || strings.HasSuffix(recPath, reqPath) || strings.HasSuffix(reqPath, recPath) {
				return true
			}
		}
	}
	return false
}

// meetsRecordFilters checks whether a record passes all non-scoring filters.
func meetsRecordFilters(rec *MemoryRecord, opt QueryOptions) bool {
	effStatus := rec.EffectiveStatus()
	if len(opt.IncludeStatus) > 0 {
		matched := false
		for _, st := range opt.IncludeStatus {
			if effStatus == st {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	} else if effStatus == StatusSuperseded || effStatus == StatusExpired || effStatus == StatusRejected {
		return false
	}
	if opt.MinConfidence > 0 && rec.Confidence < opt.MinConfidence {
		return false
	}
	if opt.Project != "" && rec.Project != "" && rec.Project != opt.Project {
		return false
	}
	if opt.Team != "" && rec.Team != "" && rec.Team != opt.Team {
		return false
	}
	return true
}

// QueryRecords performs hybrid search and ranking (§20.4) over MemoryRecords.
func (s *MemoryStore) QueryRecords(ctx context.Context, opt QueryOptions) ([]QueryResult, error) {
	if err := s.init(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	n := opt.N
	if n <= 0 {
		n = 5
	}

	var filter map[string]string
	if opt.Category != "" {
		filter = map[string]string{"category": opt.Category}
	}
	if len(opt.MetadataFilter) > 0 {
		if filter == nil {
			filter = make(map[string]string)
		}
		for k, v := range opt.MetadataFilter {
			if k != "category" {
				filter[k] = v
			}
		}
	}

	totalDocs := s.collection.Count()
	if totalDocs == 0 {
		return nil, nil
	}

	fetchN := n * 5
	if fetchN < 50 {
		fetchN = 50
	}
	if fetchN > totalDocs {
		fetchN = totalDocs
	}

	results, err := s.collection.Query(ctx, opt.Query, fetchN, filter, nil)
	if err != nil {
		return nil, fmt.Errorf("memory query failed: %w", err)
	}

	var candidates []QueryResult
	for _, r := range results {
		rec, err := metadataToRecord(r.ID, r.Content, r.Metadata)
		if err != nil {
			continue
		}
		if !meetsRecordFilters(&rec, opt) {
			continue
		}
		candidates = append(candidates, QueryResult{
			Record:     rec,
			Similarity: r.Similarity,
			Score:      calculateHybridScore(&r, &rec, opt),
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})
	if len(candidates) > n {
		candidates = candidates[:n]
	}

	return candidates, nil
}

// Query is a backward-compatible query returning Result structs, using hybrid ranking under the hood.
// The full filter map is passed through via MetadataFilter for chromem-level filtering (R1).
func (s *MemoryStore) Query(ctx context.Context, query string, n int, filter map[string]string) ([]Result, error) {
	cat := ""
	if filter != nil {
		cat = filter["category"]
	}

	qResults, err := s.QueryRecords(ctx, QueryOptions{
		Query:          query,
		N:              n,
		Category:       cat,
		MetadataFilter: filter,
	})
	if err != nil {
		return nil, err
	}

	out := make([]Result, 0, len(qResults))
	for _, qr := range qResults {
		meta, _ := recordToMetadata(qr.Record)
		out = append(out, Result{
			ID:         qr.Record.ID,
			Content:    qr.Record.Content,
			Similarity: float32(qr.Score),
			Metadata:   meta,
		})
	}
	return out, nil
}

func (s *MemoryStore) Delete(ctx context.Context, id string) error {
	if err := s.init(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.collection.Delete(ctx, nil, nil, id)
}

func (s *MemoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	if err := s.init(); err != nil {
		return fmt.Errorf("failed to initialize memory store for close: %w", err)
	}
	s.db = nil
	s.collection = nil
	s.initialized = false
	return nil
}

// readEmbeddingMeta reads the embedding metadata sidecar file.
func readEmbeddingMeta(storePath string) (*embeddingMeta, error) {
	metaPath := filepath.Join(storePath, metaFileName)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read embedding meta: %w", err)
	}
	var meta embeddingMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("failed to parse embedding meta: %w", err)
	}
	return &meta, nil
}

// writeEmbeddingMeta writes the embedding metadata sidecar file.
func writeEmbeddingMeta(storePath string, meta *embeddingMeta) error {
	if err := os.MkdirAll(storePath, 0o755); err != nil {
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}
	metaPath := filepath.Join(storePath, metaFileName)
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal embedding meta: %w", err)
	}
	if err := os.WriteFile(metaPath, data, 0o644); err != nil {
		return fmt.Errorf("failed to write embedding meta: %w", err)
	}
	return nil
}

// probeEmbeddingModel verifies the embedding model is available by embedding a probe string.
func probeEmbeddingModel(ctx context.Context, embedFunc chromem.EmbeddingFunc) error {
	_, err := embedFunc(ctx, "probe")
	if err != nil {
		return fmt.Errorf("embedding probe failed: %w", err)
	}
	return nil
}

// checkEmbeddingModelMismatch checks if the stored embedding model differs from the current one.
func checkEmbeddingModelMismatch(db *chromem.DB, storePath, embedModel string) (bool, error) {
	meta, err := readEmbeddingMeta(storePath)
	if err != nil {
		return false, err
	}

	if meta == nil {
		log.Printf("Warning: existing memory store has no embedding model metadata; assuming %q matches. If results seem wrong, re-index with: rm -rf %s", embedModel, storePath)
		newMeta := &embeddingMeta{
			EmbeddingModel: embedModel,
			CreatedAt:      time.Now().Format(time.RFC3339),
		}
		if err := writeEmbeddingMeta(storePath, newMeta); err != nil {
			return false, err
		}
		return false, nil
	}

	if meta.EmbeddingModel == embedModel {
		return false, nil
	}

	log.Printf("embedding model mismatch: stored=%q, current=%q; deleting old collection", meta.EmbeddingModel, embedModel)
	if err := db.DeleteCollection(collectionName); err != nil {
		return false, fmt.Errorf("failed to delete collection during model mismatch cleanup: %w", err)
	}

	newMeta := &embeddingMeta{
		EmbeddingModel: embedModel,
		CreatedAt:      time.Now().Format(time.RFC3339),
	}
	if err := writeEmbeddingMeta(storePath, newMeta); err != nil {
		return false, err
	}

	return true, nil
}
