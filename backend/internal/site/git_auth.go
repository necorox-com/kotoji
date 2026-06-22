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
func (g *gitService) mirrorAuthEnv() []string {
	token := g.cfg.GitHubToken
	if token == "" {
		return nil
	}
	user := g.cfg.GitHubUser
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

// runMirror runs a network git command (push/fetch) with the per-invocation
// GitHub auth header layered into the environment when a token is configured. It
// uses the envRunner seam (execRunner supports it); a runner without RunEnv (the
// test fake without env support) falls back to a plain Run, which is fine because
// such runners do not actually hit the network. The returned error carries argv
// via *GitError but NEVER the token (it lives only in env, which is not recorded).
func (g *gitService) runMirror(ctx context.Context, id uuid.UUID, args ...string) (string, error) {
	env := g.mirrorAuthEnv()
	if er, ok := g.git.(envRunner); ok {
		out, err := er.RunEnv(ctx, g.repoDir(id), env, nil, args...)
		return string(out), err
	}
	out, err := g.git.Run(ctx, g.repoDir(id), nil, args...)
	return string(out), err
}
