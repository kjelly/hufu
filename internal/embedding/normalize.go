package embedding

import "math"

func normalizeL2(vector []float32) error {
	var squared float64
	for _, value := range vector {
		v := float64(value)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return ErrInvalidEmbedding
		}
		squared += v * v
	}
	if squared == 0 || math.IsNaN(squared) || math.IsInf(squared, 0) {
		return ErrInvalidEmbedding
	}
	scale := float32(1 / math.Sqrt(squared))
	for i := range vector {
		vector[i] *= scale
	}
	return nil
}
