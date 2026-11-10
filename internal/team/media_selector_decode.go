package team

import (
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/media"
)

func validateModelImageInputField(key string, data []byte) error {
	switch strings.ToLower(key) {
	case "imageinputs", "image-inputs":
		return fmt.Errorf("use image_inputs with ordered turn_id and part_index selectors")
	case "image_inputs":
		var selections []media.ImageInputSelector
		if err := json.Unmarshal(data, &selections); err != nil {
			return err
		}
		if len(selections) == 0 || len(selections) > media.MaxCount {
			return media.Reject(media.LimitExceeded, "image_inputs requires a nonempty bounded selection")
		}
	}
	return nil
}
