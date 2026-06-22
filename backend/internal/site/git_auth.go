package site

import (
	"context"
	"encoding/base64"
	"strconv"

	"github.com/google/uuid"
)

// This file implements GitHub mirror AUTHENTICATION. The locked design rule
// (task LOCKED DECISIONS): the token is injected per git invocation WITHOUT ever
// touching .git/config or the remote URL on disk, and it MUST NEVER appear in any
// command output / log / process arg list.
//
// Mechanism: git's config-via-environment (GIT_CONFIG_COUNT + GIT_CONFIG_KEY_n /
// GIT_CONFIG_VALUE_n, git >= 2.31). We set `http.<base>.extraHeader` to an HTTP
// Basic credential scoped to github.com so only pushes/fetches to that host carry
// it. Because the credential travels through the ENVIRONMENT (not argv), it never
// lands in *GitError.Args (which records argv only) — so a non-zero push exit can
// be logged verbatim with zero risk of leaking the secret. The header value is
// base64("<user>:<token>"), exactly what `Authorization: Basic` expects.

const (
	// githubBase scopes the credential so it is only attached to github.com URLs.
	// (git matches http.<URL>.* by URL prefix; https://github.com covers every
	// owner/repo under that host.)
	githubBase = "https://github.com"
	// defaultGitHubUser is the canonical basic-auth username for app/installation
	// tokens; GitHub also accepts it for classic PATs (username is ignored, the
	// token in the password slot is what authenticates).
	defaultGitHubUser = "x-access-token"
)

// mirrorAuthEnv builds the GIT_CONFIG_* environment that injects an
// `http.https://github.com.extraHeader: Authorization: Basic <base64>` config for
// a single git invocation, or nil when no token is configured. The token is
// carried ONLY in the environment, never in argv, so it cannot leak through
// *GitError.Args or a process listing.
//
// The credential is resolved PER call: when cfg.MirrorToken is set it is the
// dynamic source (DB-then-env, so a runtime admin change applies without a
// restart); otherwise the static cfg.GitHubToken/GitHubUser are used (env-only
// deployments). The ctx flows into the dynamic provider so a DB read honors the
// request deadline.
func (g *gitService) mirrorAuthEnv(ctx context.Context) []string {
	token, user := g.mirrorCreds(ctx)
	if token == "" {
		return nil
	}
	if user == "" {
		user = defaultGitHubUser
	}
	// Authorization: Basic base64("user:token"). GitHub authenticates on the token
	// in the password slot; the username is conventional only.
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	header := "Authorization: Basic " + cred
	// Two config entries: scope the extraHeader to github.com only.
	const count = 1
	return []string{
		"GIT_CONFIG_COUNT=" + strconv.Itoa(count),
		"GIT_CONFIG_KEY_0=http." + githubBase + ".extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

// mirrorCreds resolves the effective mirror credential for one git call. The
// dynamic cfg.MirrorToken provider (DB-then-env, wired by the composition root)
// takes precedence so a runtime config change applies immediately; the static
// cfg.GitHubToken/GitHubUser are the fallback for env-only deployments and tests.
func (g *gitService) mirrorCreds(ctx context.Context) (token, user string) {
	if g.cfg.MirrorToken != nil {
		return g.cfg.MirrorToken(ctx)
	}
	return g.cfg.GitHubToken, g.cfg.GitHubUser
}

// mirrorEnabled reports whether mirror pushes should be attempted at all. The
// dynamic cfg.MirrorEnabled gate (DB-overrides-env) wins when set so a runtime
// toggle applies without a restart; otherwise the static cfg.MirrorOn flag is
// used (env-only deployments / tests).
func (g *gitService) mirrorEnabled(ctx context.Context) bool {
	if g.cfg.MirrorEnabled != nil {
		return g.cfg.MirrorEnabled(ctx)
	}
	return g.cfg.MirrorOn
}

// runMirror runs a network git command (push/fetch) with the per-invocation
// GitHub auth header layered into the environment when a token is configured. It
// uses the envRunner seam (execRunner supports it); a runner without RunEnv (the
// test fake without env support) falls back to a plain Run, which is fine because
// such runners do not actually hit the network. The returned error carries argv
// via *GitError but NEVER the token (it lives only in env, which is not recorded).
func (g *gitService) runMirror(ctx context.Context, id uuid.UUID, args ...string) (string, error) {
	env := g.mirrorAuthEnv(ctx)
	if er, ok := g.git.(envRunner); ok {
		out, err := er.RunEnv(ctx, g.repoDir(id), env, nil, args...)
		return string(out), err
	}
	out, err := g.git.Run(ctx, g.repoDir(id), nil, args...)
	return string(out), err
}
