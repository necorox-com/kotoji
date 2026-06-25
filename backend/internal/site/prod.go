package site

import "github.com/necorox-com/kotoji/backend/internal/db"

// NewProductionService is the exported composition-root factory for the real
// git-backed Service. It bundles the two production collaborators the gitService
// needs — the metadata Store adapted over *db.Store, and the os/exec-backed git
// runner — both of which are UNEXPORTED inside this package. Without this factory
// the composition root could not construct the production Service (it cannot reach
// newExecRunner / dbStoreAdapter), so this is the single sanctioned seam for
// Integration to build the prod impl while keeping the runner/adapter internal.
//
// cfg.Root MUST be the writable sites base dir (CANONICAL §1: /data/sites). The
// caller fills cfg from internal/config (DataDir+"/sites", GitBin, zip limits).
func NewProductionService(store *db.Store, cfg Config) Service {
	// Apply Config defaults here too so the runner receives the RESOLVED git-op
	// timeout (T1): NewService defaults cfg for the service, but the runner is built
	// from cfg before that, so a zero GitOpTimeout must be defaulted before use.
	cfg = cfg.withDefaults()
	return NewService(NewStore(store), newExecRunner(cfg.GitBin, cfg.GitOpTimeout), cfg)
}
