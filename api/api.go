// Package api carries the service's OpenAPI description. It is embedded so the
// running binary can serve its own documentation, and lives here rather than
// beside the handlers so the contract is easy to find and feed to other tools.
package api

import _ "embed"

// OpenAPISpec is the OpenAPI 3.0 description of the HTTP API, as YAML.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
