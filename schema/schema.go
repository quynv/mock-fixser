// Package schema embeds the JSON Schema of the mockfix configuration. The
// backend validates against it and serves it to the UI at /api/schema.
package schema

import _ "embed"

//go:embed config.schema.json
var ConfigJSON []byte
