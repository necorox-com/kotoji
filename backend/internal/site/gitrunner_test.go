package site

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner is a gitRunner that records the argv of each call and returns canned
// stdout / errors so gitService's branching + error-mapping logic is unit-testable
// WITHOUT a real git binary (site-service.md §13.B golden-arg + error-mapping).
type fakeRunner struct {
	calls    [][]string // recorded argv (WITHOUT the -C repoDir prefix)
	repoDirs []string   // repoDir per call
	envCalls [][]string // recorded extra env per RunEnv call
	respond  func(args []string) ([]byte, error)
}

func (r *fakeRunner) Run(ctx context.Context, repoDir string, stdin []byte, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	r.repoDirs = append(r.repoDirs, repoDir)
	if r.respond != nil {
		return r.respond(args)
	}
	return nil, nil
}

func (r *fakeRunner) RunEnv(ctx context.Context, repoDir string, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	r.repoDirs = append(r.repoDirs, repoDir)
	r.envCalls = append(r.envCalls, append([]string(nil), extraEnv...))
	if r.respond != nil {
		return r.respond(args)
	}
	return nil, nil
}

var _ envRunner = (*fakeRunner)(nil)

// findCall returns the first recorded call whose first arg equals subcmd.
func (r *fakeRunner) findCall(subcmd string) ([]string, bool) {
	for _, c := range r.calls {
		if len(c) > 0 && c[0] == subcmd {
			return c, true
		}
	}
	return nil, false
}

// TestGitService_RevParseArgs asserts the golden argv for the rev-parse used in
// the optimistic-lock tip read (CANONICAL §5: BaseSHA compare uses rev-parse).
func TestGitService_RevParseArgs(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		return []byte("abc123\n"), nil
	}}
	g := NewService(newMemStore(), fr, Config{Root: t.TempDir()})
	sha, err := g.revParse(context.Background(), uuid.New(), "draft")
	require.NoError(t, err)
	assert.Equal(t, "abc123", sha)
	call, ok := fr.findCall("rev-parse")
	require.True(t, ok, "rev-parse must be invoked")
	assert.Equal(t, []string{"rev-parse", "--verify", "--quiet", "draft^{commit}"}, call)
}

// TestGitService_CommitInjectsAuthorEnv asserts the commit path injects the
// GIT_AUTHOR_*/GIT_COMMITTER_* env from the Actor (site-service.md §13.B).
func TestGitService_CommitInjectsAuthorEnv(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "rev-parse" {
			return []byte("newsha\n"), nil
		}
		if len(args) > 0 && args[0] == "show" {
			// logFormat record for commitInfo parse.
			return []byte("newsha\x1fnewsha\x1fTester\x1ft@e.com\x1f2026-01-02T03:04:05Z\x1f\x1fmsg\x1e"), nil
		}
		return nil, nil
	}}
	clk := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	g := NewServiceWithClock(newMemStore(), fr, Config{Root: t.TempDir()}, clk)
	actor := Actor{UserID: uuid.New(), Name: "Tester", Email: "t@e.com", Via: SourceEditor}
	_, err := g.commitStaged(context.Background(), uuid.New(), BranchDraft, actor, "msg")
	require.NoError(t, err)
	require.NotEmpty(t, fr.envCalls, "commit must go through RunEnv with author env")
	env := fr.envCalls[0]
	assert.Contains(t, env, "GIT_AUTHOR_NAME=Tester")
	assert.Contains(t, env, "GIT_AUTHOR_EMAIL=t@e.com")
	assert.Contains(t, env, "GIT_COMMITTER_NAME=Tester")
}

// TestGitService_MirrorAuthEnvInjectsHeader asserts the mirror push carries the
// GitHub Basic auth header via the ENVIRONMENT (GIT_CONFIG_*), scoped to
// github.com, and that the token NEVER appears in the recorded argv (so it cannot
// leak through *GitError.Args or a process listing / log).
func TestGitService_MirrorAuthEnvInjectsHeader(t *testing.T) {
	const token = "ghp_supersecrettoken"
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		return nil, nil
	}}
	store := newMemStore()
	rec, _ := store.CreateSiteWithOwner(context.Background(), StoreCreateSite{
		Handle: "mir", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	g := NewService(store, fr, Config{Root: t.TempDir(), MirrorOn: true, GitHubToken: token})

	err := g.MirrorPush(context.Background(), rec.ID, BranchPublished)
	require.NoError(t, err)

	// The push must be the recorded call, and the token must NOT be in any argv.
	push, ok := fr.findCall("push")
	require.True(t, ok, "push must be invoked")
	assert.Equal(t, []string{"push", "--force-with-lease", "origin", "published"}, push)
	for _, call := range fr.calls {
		for _, a := range call {
			assert.NotContains(t, a, token, "token must never appear in argv")
		}
	}

	// The auth header must be injected via the environment, base64 of user:token,
	// scoped to github.com only.
	require.NotEmpty(t, fr.envCalls, "mirror push must go through RunEnv with auth env")
	env := fr.envCalls[len(fr.envCalls)-1]
	assert.Contains(t, env, "GIT_CONFIG_COUNT=1")
	assert.Contains(t, env, "GIT_CONFIG_KEY_0=http.https://github.com.extraHeader")
	wantCred := base64.StdEncoding.EncodeToString([]byte(defaultGitHubUser + ":" + token))
	assert.Contains(t, env, "GIT_CONFIG_VALUE_0=Authorization: Basic "+wantCred)
	// And the raw token must never appear verbatim in the env either (it is base64'd).
	for _, e := range env {
		assert.NotContains(t, e, token, "raw token must not appear in env (base64 only)")
	}
}

// TestGitService_MirrorNoTokenNoAuthEnv asserts that with no token configured the
// push still runs (best-effort) but carries NO auth header — so a private remote
// simply fails rather than the save erroring or hanging on a prompt.
func TestGitService_MirrorNoTokenNoAuthEnv(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) { return nil, nil }}
	store := newMemStore()
	rec, _ := store.CreateSiteWithOwner(context.Background(), StoreCreateSite{
		Handle: "mir2", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	g := NewService(store, fr, Config{Root: t.TempDir(), MirrorOn: true}) // no GitHubToken

	require.NoError(t, g.MirrorPush(context.Background(), rec.ID, BranchPublished))
	require.NotEmpty(t, fr.envCalls, "push goes through RunEnv even without a token")
	for _, env := range fr.envCalls {
		for _, e := range env {
			assert.NotContains(t, e, "GIT_CONFIG_COUNT", "no auth header when no token")
		}
	}
}

// TestGitService_GitErrorDoesNotLeakToken proves a failed push wraps a *GitError
// whose Args carry the argv but NOT the token (the token lives only in env, which
// GitError never records), so error logs are safe to emit verbatim.
func TestGitService_GitErrorDoesNotLeakToken(t *testing.T) {
	const token = "ghp_leakcheck"
	gerr := &GitError{Args: []string{"push", "--force-with-lease", "origin", "published"}, ExitCode: 128, Stderr: "fatal: auth"}
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		if len(args) > 0 && args[0] == "push" {
			return nil, gerr
		}
		return nil, nil
	}}
	store := newMemStore()
	rec, _ := store.CreateSiteWithOwner(context.Background(), StoreCreateSite{
		Handle: "mir3", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	g := NewService(store, fr, Config{Root: t.TempDir(), MirrorOn: true, GitHubToken: token})

	err := g.MirrorPush(context.Background(), rec.ID, BranchPublished)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGit))
	var ge *GitError
	require.True(t, errors.As(err, &ge))
	for _, a := range ge.Args {
		assert.NotContains(t, a, token, "GitError.Args must never contain the token")
	}
	assert.NotContains(t, ge.Stderr, token, "GitError.Stderr must never contain the token")
}

// TestGitService_NonZeroExitWrapsErrGit asserts a runner GitError surfaces as
// errors.Is(err, ErrGit) through the service (error-mapping).
func TestGitService_NonZeroExitWrapsErrGit(t *testing.T) {
	gerr := &GitError{Args: []string{"for-each-ref"}, ExitCode: 128, Stderr: "fatal: bad"}
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		return nil, gerr
	}}
	store := newMemStore()
	// Insert a live site so requireSite passes and we reach the git call.
	rec, _ := store.CreateSiteWithOwner(context.Background(), StoreCreateSite{
		Handle: "x", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	g := NewService(store, fr, Config{Root: t.TempDir()})
	_, err := g.ListBranches(context.Background(), rec.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrGit), "non-zero exit must map to ErrGit")
	var ge *GitError
	require.True(t, errors.As(err, &ge))
	assert.Equal(t, 128, ge.ExitCode)
}

// TestGitService_CtxCancellationPropagates asserts a cancelled context surfaces
// as context.Canceled (NOT misread as a git logic failure).
func TestGitService_CtxCancellationPropagates(t *testing.T) {
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) {
		return nil, context.Canceled
	}}
	store := newMemStore()
	rec, _ := store.CreateSiteWithOwner(context.Background(), StoreCreateSite{
		Handle: "y", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	g := NewService(store, fr, Config{Root: t.TempDir()})
	_, err := g.ListBranches(context.Background(), rec.ID)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled))
}

// TestExecRunner_ArgArrayNoShell proves the real runner passes args as an array
// (no shell): a path containing shell metacharacters is treated literally, so a
// "rm -rf" payload as a ref name simply fails rev-parse rather than executing.
func TestExecRunner_ArgArrayNoShell(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	dir := t.TempDir()
	r := newExecRunner("git")
	_, err := r.Run(context.Background(), dir, nil, "init", "-b", "draft")
	require.NoError(t, err)
	// A malicious "ref" with shell metacharacters must NOT execute; rev-parse just
	// reports an unknown revision (non-zero exit -> GitError), nothing runs.
	_, err = r.Run(context.Background(), dir, nil, "rev-parse", "--verify", "--quiet", "$(touch /tmp/pwned)")
	require.Error(t, err)
	var ge *GitError
	assert.True(t, errors.As(err, &ge), "metachar ref should fail as a GitError, not execute")
}

// TestExecRunner_ScrubbedEnv asserts the prompt-disabling env entries are present
// so a misconfigured remote can never hang on credential input.
func TestExecRunner_ScrubbedEnv(t *testing.T) {
	env := scrubbedEnv()
	assert.Contains(t, env, "GIT_TERMINAL_PROMPT=0")
	assert.Contains(t, env, "GIT_CONFIG_NOSYSTEM=1")
}
