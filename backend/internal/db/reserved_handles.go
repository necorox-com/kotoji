package db

// ReservedHandlesBaseline is the locked reserved-handle blocklist (CANONICAL §5,
// "ReservedHandles"). It is the single source the dev seed and the migration test
// check against, and it mirrors EXACTLY the rows inserted by migration
// 0002_seed_reserved.sql. Keep the two in sync — migrations_test.go asserts they
// match so drift fails the build.
//
// NOTE: Phase 2 will introduce the canonical `internal/handle` package whose
// ReservedHandles is THE runtime validator constant; this slice is the data-layer
// copy used to keep the migration seed honest. When the handle package lands, a
// test should assert handle.ReservedHandles == ReservedHandlesBaseline.
var ReservedHandlesBaseline = []string{
	"draft", "preview", "published", "www", "api", "internal",
	"host", "admin", "app", "static", "assets", "mcp",
}
