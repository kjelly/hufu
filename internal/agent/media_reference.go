package agent

import (
	"encoding/json/v2"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/media"
)

const ContentTypeMediaReference fantasy.ContentType = "media_ref"

// MediaReferencePart is a runtime history part, never a provider wire part.
// Its type has no byte or filename fields. Only the admitted request compiler
// may replace it with a short-lived SDK FilePart.
type MediaReferencePart struct {
	Reference media.HistoryReference
}

func (MediaReferencePart) GetType() fantasy.ContentType     { return ContentTypeMediaReference }
func (MediaReferencePart) Options() fantasy.ProviderOptions { return nil }

func (p MediaReferencePart) MarshalJSON() ([]byte, error) {
	if err := p.Reference.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Type fantasy.ContentType    `json:"type"`
		Data media.HistoryReference `json:"data"`
	}{ContentTypeMediaReference, p.Reference})
}

func DecodeMediaReferencePart(data []byte) (MediaReferencePart, error) {
	var envelope struct {
		Type fantasy.ContentType    `json:"type"`
		Data media.HistoryReference `json:"data"`
	}
	if err := json.Unmarshal(data, &envelope, json.RejectUnknownMembers(true)); err != nil {
		return MediaReferencePart{}, media.Reject(media.BindingConflict, "invalid history media envelope")
	}
	if envelope.Type != ContentTypeMediaReference {
		return MediaReferencePart{}, media.Reject(media.Unsupported, "unknown history media envelope")
	}
	if err := envelope.Data.Validate(); err != nil {
		return MediaReferencePart{}, err
	}
	return MediaReferencePart{Reference: envelope.Data}, nil
}
