package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/kjelly/hufu/internal/agent"
)

type returnedCollectionResponse struct {
	InvocationSHA256 string         `json:"invocation_sha256"`
	BodySHA256       string         `json:"body_sha256"`
	Body             []byte         `json:"body"`
	Usage            ExecutionUsage `json:"usage"`
}

func (c *Coordinator) collectionResponseDocument(checkpoint *ResultCollectionCheckpoint) (string, string, error) {
	if checkpoint == nil || checkpoint.State != ResultCollectionInFlight {
		return "", "", errResultCollectionStale
	}
	digest, err := collectionDigest(struct {
		Branch     string
		Checkpoint *ResultCollectionCheckpoint
	}{c.activeBranchID(), checkpoint})
	return "result-collection-response:" + digest, digest, err
}

func (c *Coordinator) recordCollectionResponseUsage(ctx context.Context, expected *ResultCollectionCheckpoint, response []byte, usage ExecutionUsage, issue *agent.StructuredResponseIssue) (*ResultCollectionCheckpoint, error) {
	name, digest, err := c.collectionResponseDocument(expected)
	if err != nil || expected.InvocationRunID != c.executionRunID {
		return nil, errResultCollectionStale
	}
	body, limit, err := encodeCollectionBlob(expected, "response", response, issue, nil)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	if len(body) > limit {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, fmt.Errorf("result collection response exceeds its byte limit"))
	}
	returned := returnedCollectionResponse{InvocationSHA256: digest, BodySHA256: sha256HexBytes(body), Body: body, Usage: usage}
	encoded, err := json.Marshal(returned, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	// Preserve the redacted provider return independently of both CAS and the
	// journal. A failed Received publication must not spend another turn.
	if err := c.sealCollectionDocument(ctx, name, encoded); err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	return c.publishCollectionResponse(ctx, expected, returned)
}

func (c *Coordinator) restoreCollectionResponse(ctx context.Context, expected *ResultCollectionCheckpoint) (*ResultCollectionCheckpoint, error) {
	returned, err := c.readReturnedCollectionResponse(expected)
	if err != nil || returned == nil {
		return nil, err
	}
	return c.publishCollectionResponse(ctx, expected, *returned)
}

func (c *Coordinator) readReturnedCollectionResponse(expected *ResultCollectionCheckpoint) (*returnedCollectionResponse, error) {
	name, digest, err := c.collectionResponseDocument(expected)
	if err != nil {
		return nil, err
	}
	body, found, err := workspaceDocs(c.artifactStoreRootPath()).getDocument(name)
	if err != nil || !found {
		return nil, err
	}
	var returned returnedCollectionResponse
	if err := json.Unmarshal(body, &returned, json.RejectUnknownMembers(true)); err != nil ||
		digest != returned.InvocationSHA256 || len(returned.Body) > resultCollectionResponseMaxBytes*2 || sha256HexBytes(returned.Body) != returned.BodySHA256 {
		return nil, errResultCollectionStale
	}
	if _, err := decodeCollectionBlob(expected, "response", returned.Body, resultCollectionResponseMaxBytes); err != nil {
		return nil, err
	}
	return &returned, nil
}

func (c *Coordinator) publishCollectionResponse(ctx context.Context, expected *ResultCollectionCheckpoint, returned returnedCollectionResponse) (*ResultCollectionCheckpoint, error) {
	ref, err := c.putCollectionBytes(ctx, expected, "response", returned.Body, resultCollectionResponseMaxBytes*2)
	if err != nil {
		return nil, rejectLocalResultPersistence(err)
	}
	return c.updateResultCollection(ctx, expected, func(next *ResultCollectionCheckpoint, _ *ExecutionReceipt) error {
		next.State, next.Response = ResultCollectionReceived, &ref
		if next.ObservedUsage == nil {
			next.ObservedUsage = &ExecutionUsage{}
		}
		usage := returned.Usage
		next.ObservedUsage.InputTokens += usage.InputTokens
		next.ObservedUsage.OutputTokens += usage.OutputTokens
		next.ObservedUsage.TotalTokens += usage.TotalTokens
		next.ObservedUsage.ProgressTokens += usage.ProgressTokens
		next.ObservedUsage.CacheReadTokens += usage.CacheReadTokens
		next.ObservedUsage.CacheCreationTokens += usage.CacheCreationTokens
		return nil
	})
}
