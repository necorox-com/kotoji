// Package migrations embeds the goose SQL migration files so the backend can run
// them on boot (no separate goose CLI is needed in the runtime image). The
// embedded FS is consumed by internal/migrate; the raw .sql files are still used
// directly by the goose CLI (`make migrate`) and by the data-model tests.
package migrations

import "embed"

// FS holds the goose migration files (NNNN_name.sql) at its root, in lexical
// (== apply) order. internal/migrate sets this as goose's base FS.
//
//go:embed *.sql
var FS embed.FS
