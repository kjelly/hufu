package team

import (
	"context"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
)

// The frozen request is the durable channel record. Auto can move from the
// function channel to text once; neither restart nor a repair grant moves back.
func (c *Coordinator) collectionResponseChannel(ctx context.Context, checkpoint *ResultCollectionCheckpoint, supportsTools modelprofile.CapabilityState) (agent.CollectorResponseChannel, error) {
	policy, err := c.collectionPolicy(checkpoint.ExecutionTarget)
	if err != nil {
		return "", err
	}
	var mode agent.CollectorResponseChannel
	if policy.ResultCollection != nil {
		mode = policy.ResultCollection.CollectorResponseChannel
	}
	if mode == agent.CollectorResponseTextJSON || supportsTools == modelprofile.CapabilityNo {
		return agent.CollectorResponseTextJSON, nil
	}
	if mode != agent.CollectorResponseAuto || checkpoint.Request == nil {
		return agent.CollectorResponseFunction, nil
	}
	raw, err := c.readCollectionBlob(ctx, checkpoint, "request", checkpoint.Request)
	if err != nil {
		return "", err
	}
	previous, err := decodeFrozenCollectionRequest(raw, checkpoint)
	if err != nil {
		return "", err
	}
	if previous.ResponseChannel == agent.CollectorResponseTextJSON || previous.ClaimsSchema == nil {
		return agent.CollectorResponseTextJSON, nil
	}
	if checkpoint.Response != nil {
		response, err := c.readCollectionBlobRecord(ctx, checkpoint, "response", checkpoint.Response)
		if err != nil {
			return "", err
		}
		if response.ResponseIssue != nil {
			return agent.CollectorResponseTextJSON, nil
		}
	}
	return agent.CollectorResponseFunction, nil
}
