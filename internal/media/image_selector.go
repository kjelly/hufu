package media

import "encoding/json/v2"

// ImageInputSelector selects an image occurrence from the caller's admitted
// view. It carries no CAS identifier, path, ownership or byte-access grant.
type ImageInputSelector struct {
	TurnID    string `json:"turn_id"`
	PartIndex int    `json:"part_index"`
}

func (s ImageInputSelector) Validate() error {
	if !validIdentity(s.TurnID) || s.PartIndex < 0 {
		return Reject(BindingConflict, "image input selector requires a turn and nonnegative part index")
	}
	return nil
}

// UnmarshalJSON keeps model-facing selectors strict even when an enclosing
// legacy task decoder allows unknown fields. Missing zero-valued indices must
// not silently select the first part of a mixed-content turn.
func (s *ImageInputSelector) UnmarshalJSON(data []byte) error {
	var fields struct {
		TurnID    *string `json:"turn_id"`
		PartIndex *int    `json:"part_index"`
	}
	if err := json.Unmarshal(data, &fields, json.RejectUnknownMembers(true)); err != nil {
		return Reject(BindingConflict, "invalid image input selector fields")
	}
	if fields.TurnID == nil || fields.PartIndex == nil {
		return Reject(BindingConflict, "image input selector requires turn_id and part_index")
	}
	selection := ImageInputSelector{TurnID: *fields.TurnID, PartIndex: *fields.PartIndex}
	if err := selection.Validate(); err != nil {
		return err
	}
	*s = selection
	return nil
}
