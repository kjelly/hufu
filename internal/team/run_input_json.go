package team

import "encoding/json/v2"

// UnmarshalJSON removes transport indentation from an already canonical value.
// It preserves member order, string and number spellings, and every recorded
// hash. Validation still rejects changed values and noncanonical content.
func (input *ResolvedRunInput) UnmarshalJSON(data []byte) error {
	type wireInput ResolvedRunInput
	var decoded wireInput
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.CanonicalValue) > 0 {
		compact := decoded.CanonicalValue
		if err := compact.Compact(); err != nil {
			return err
		}
		decoded.CanonicalValue = compact
	}
	*input = ResolvedRunInput(decoded)
	return nil
}
