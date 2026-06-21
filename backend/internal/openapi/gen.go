// Package openapi holds the Go DTOs generated from the FROZEN OpenAPI 3.1 spec
// at docs/contracts/openapi.yaml (CANONICAL §9 decision #1: the spec is the
// source of truth; Go types are oapi-codegen output, hand-written chi handlers
// consume them).
//
// Generation: oapi-codegen v2.4.1 ships kin-openapi v0.127, which does not yet
// parse OpenAPI 3.1's nullable type unions (`type: ["string","null"]`). Rather
// than edit the frozen spec, the go:generate step below shells a tiny in-tree
// preprocessor (spec31to30.py) that emits a 3.0-compatible COPY to a temp file
// (converting `type:[X,null]` -> `type:X, nullable:true` and the version header)
// and feeds THAT to oapi-codegen. The original openapi.yaml is never modified.
//
//	go generate ./internal/openapi/...
//
//go:generate sh -c "python3 spec31to30.py ../../../docs/contracts/openapi.yaml /tmp/kotoji-openapi-30.yaml && cd ../.. && go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -config oapi-codegen.yaml /tmp/kotoji-openapi-30.yaml"
package openapi
