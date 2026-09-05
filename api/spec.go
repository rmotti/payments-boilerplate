// Package apispec exposes the versioned OpenAPI contract embedded in binaries.
package apispec

import _ "embed"

// OpenAPI contains the source-of-truth HTTP contract.
//
//go:embed openapi.yaml
var OpenAPI []byte
