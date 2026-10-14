package inspect

import (
	jsonv1 "encoding/json"
	"encoding/json/v2"
)

// Runtime DTOs retain their existing nanosecond duration representation.
func marshalHistoricalJSON(value any) ([]byte, error) {
	return json.Marshal(value, jsonv1.FormatDurationAsNano(true))
}

func unmarshalHistoricalJSON(data []byte, value any) error {
	return json.Unmarshal(data, value, jsonv1.FormatDurationAsNano(true))
}
