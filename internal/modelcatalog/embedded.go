package modelcatalog

import _ "embed"

//go:generate go run generate_embedded.go -input ../../.models.dev-api.json -output embedded_models.json -manifest embedded_manifest.json

//go:embed embedded_models.json
var embeddedSnapshot []byte

//go:embed embedded_manifest.json
var embeddedManifest []byte
