// Package api holds openapi.yaml, the OpenAPI description of the server's
// HTTP API. The Editor's API client in the edity repository is generated
// from it, and the server's tests check its handlers against it
// (internal/apitest).
package api

import _ "embed"

// OpenAPI is the content of openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
