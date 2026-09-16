package embedding

import (
	"context"
	"fmt"
)

func meanPoolNormalized(
	ctx context.Context,
	ids []int,
	table []float32,
	dimensions int,
	skip map[int]struct{},
) ([]float32, error) {
	result := make([]float32, dimensions)
	count := 0
	for i, id := range ids {
		if i%64 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if _, ok := skip[id]; ok {
			continue
		}
		if id < 0 || id >= len(table)/dimensions {
			return nil, fmt.Errorf("token ID %d is outside embedding table", id)
		}
		start := id * dimensions
		for dim := range dimensions {
			result[dim] += table[start+dim]
		}
		count++
	}
	if count == 0 {
		return nil, ErrNoEmbeddableTokens
	}
	scale := float32(1) / float32(count)
	for i := range result {
		result[i] *= scale
	}
	if err := normalizeL2(result); err != nil {
		return nil, err
	}
	return result, nil
}
