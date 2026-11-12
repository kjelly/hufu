package taskdistill

import (
	jsonv1 "encoding/json"
	json "encoding/json/v2"
)

// MarshalSnapshot preserves the runtime's existing JSON wire contract while
// using the v2 codec. In particular, persisted time.Duration values are
// nanoseconds, not the format-required durations of new v2-only DTOs.
func MarshalSnapshot(value any) ([]byte, error) {
	return json.Marshal(value, jsonv1.DefaultOptionsV1(), json.Deterministic(true))
}

func UnmarshalSnapshot(raw []byte, value any) error {
	return json.Unmarshal(raw, value, jsonv1.DefaultOptionsV1())
}
