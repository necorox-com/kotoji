// Package preview owns the ONE shared codec key for signed preview grants so the
// control plane (the /preview-grant endpoint in internal/api) and the data plane
// (serve.GrantAuthz, wired in internal/app) derive an IDENTICAL HMAC secret and
// can never drift in grant format.
//
// Why a separate package: both internal/api and internal/app need the same key,
// but internal/app must not import internal/api (it is the composition root that
// imports api), and api must not reach into app. A tiny shared package that both
// import is the decoupled seam. The grant value format itself lives in
// serve.GrantAuthz.SignGrant / verifyGrant — this package contributes only the
// secret derivation, keeping a single source of truth for both halves.
package preview

import (
	"crypto/hmac"
	"crypto/sha256"

	"github.com/necorox-com/kotoji/backend/internal/secretbox"
)

// secretSeedPrefix domain-separates the preview-grant key from any other HMAC
// derivation that might reuse the same config inputs.
const secretSeedPrefix = "kotoji-preview-grant|"

// Secret derives the HMAC-SHA256 key for preview grants. It is derived from
// secretbox.ResolveKey — the SINGLE source of truth for key material — so an
// explicit KOTOJI_SECRET_KEY (REQUIRED in production by the H2 policy) makes this
// grant key as strong as the at-rest key. Without an explicit key, ResolveKey
// falls back to a sha256 over the stable server seed (admin password / OIDC
// client secret plus the control + base domains), so a process restart with the
// same config keeps in-flight preview cookies valid (mirroring the auth
// login-state key derivation). Grants are short-lived, so even a per-deploy key
// roll is harmless. The base key is domain-separated here via HMAC with the
// secretSeedPrefix label so the preview-grant key never collides with the at-rest
// or login-state derivations that share those inputs.
//
// All inputs are plumbed explicitly (not a config.Config) so this package stays
// importable from both planes without a config import cycle.
func Secret(secretKeyEnv, adminPassword, oidcClientSecret, controlBaseURL, baseDomain string) []byte {
	base := secretbox.ResolveKey(secretKeyEnv, adminPassword, oidcClientSecret, controlBaseURL, baseDomain)
	mac := hmac.New(sha256.New, base)
	mac.Write([]byte(secretSeedPrefix + adminPassword + "|" + oidcClientSecret + "|" + controlBaseURL + "|" + baseDomain))
	return mac.Sum(nil)
}
