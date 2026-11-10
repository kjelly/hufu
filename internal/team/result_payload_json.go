package team

import "encoding/json/v2"

// UnmarshalJSON restores the canonical value after JSON transport indentation.
// The recorded hash and all lexical content remain unchanged; ordinary result
// validation still rejects tampered values, contracts and evidence bindings.
func (payload *ResultPayload) UnmarshalJSON(data []byte) error {
	type wirePayload ResultPayload
	var decoded wirePayload
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if len(decoded.Value) > 0 {
		if err := decoded.Value.Compact(); err != nil {
			return err
		}
	}
	*payload = ResultPayload(decoded)
	return nil
}
