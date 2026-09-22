package rule

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/decisionrt"
)

type DecideFunc func(context.Context, decisionrt.Request) (decisionrt.BackendResult, error)

type Func struct {
	BackendName string
	DecideFunc  DecideFunc
}

func (f Func) Name() string {
	return f.BackendName
}

func (f Func) Decide(ctx context.Context, request decisionrt.Request) (decisionrt.BackendResult, error) {
	if strings.TrimSpace(f.BackendName) == "" || f.DecideFunc == nil {
		return decisionrt.BackendResult{}, &decisionrt.RuntimeError{
			Kind: decisionrt.ErrorConfiguration,
			Err:  fmt.Errorf("invalid rule function"),
		}
	}
	return f.DecideFunc(ctx, request)
}

func AlwaysAbstain() decisionrt.Backend {
	return alwaysAbstain{}
}

type alwaysAbstain struct{}

func (alwaysAbstain) Name() string {
	return "rule"
}

func (alwaysAbstain) Decide(context.Context, decisionrt.Request) (decisionrt.BackendResult, error) {
	return decisionrt.BackendResult{
		Status:              decisionrt.StatusAbstained,
		ConfidenceSemantics: decisionrt.ConfidenceNone,
	}, nil
}
