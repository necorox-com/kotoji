package site

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// gitRunner is the thin os/exec seam UNDER the Service. gitService depends on it
// so gitService's own branching/error-mapping logic is unit-testable against a
// fake runner (no real git needed). CANONICAL §1 / site-service.md §9.
type gitRunner interface {
	// Run executes `git -C repoDir <args...>` with the given stdin, returning
	// stdout. A non-zero exit maps to *GitError{Args, ExitCode, Stderr}. The ctx
	// deadline/cancellation kills the child process and is surfaced as the wrapped
	// context error (errors.Is(err, context.Canceled) stays true).
	Run(ctx context.Context, repoDir string, stdin []byte, args ...string) (stdout []byte, err error)
}

// execRunner is the production gitRunner: it shells out to the real git binary
// with ARG ARRAYS only (never a shell string — no injection surface), a scrubbed
// environment, and per-call author/committer identity injected by the caller.
type execRunner struct {
	gitBin  string        // resolved "git" path; the Docker image MUST include the binary
	env     []string      // base scrubbed environment shared by every invocation
	timeout time.Duration // T1: hard server-side deadline per git child process (0 => none)
}

// newExecRunner builds an execRunner. gitBin defaults to "git" (resolved via
// PATH by exec) when empty. timeout (T1) is the hard server-side deadline applied
// to every git child process, independent of the request context; a value <= 0
// disables it. The base environment is scrubbed to a deterministic minimum that
// disables every interactive/credential prompt so a misconfigured remote can never
// hang a request waiting on a terminal.
func newExecRunner(gitBin string, timeout time.Duration) *execRunner {
	if gitBin == "" {
		gitBin = "git"
	}
	return &execRunner{
		gitBin:  gitBin,
		env:     scrubbedEnv(),
		timeout: timeout,
	}
}

// boundedContext (T1) derives a child context carrying the runner's hard git-op
// deadline ON TOP OF the inbound ctx. Because it is layered, the EFFECTIVE deadline
// is the earlier of the two: a tighter request deadline still wins, while a request
// with no/loose deadline is still bounded so a hung network git call cannot block a
// worker forever. The returned cancel MUST be called by the caller (defer) to free
// the timer; a zero/negative timeout returns ctx unchanged with a no-op cancel.
func (r *execRunner) boundedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if r.timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, r.timeout)
}

// scrubbedEnv is the deterministic base environment for every git invocation.
// GIT_TERMINAL_PROMPT=0 and the askpass/no-prompt settings guarantee git never
// blocks on credential input; a fixed config-nosystem keeps host git config out
// of the picture. PATH is preserved minimally so git can find its helpers.
//
// S1 protocol hardening: GIT_ALLOW_PROTOCOL restricts the transports git will use
// to https + ssh, so even a remote URL that somehow slipped past validateRemoteURL
// cannot drive the dangerous file:// (local-file read) or ext:: (command exec)
// transports — git refuses them outright. GIT_PROTOCOL_FROM_USER=0 additionally
// forbids any protocol that was NOT set by git's own config from being used by a
// user-provided URL (belt-and-suspenders against URL-driven SSRF / local reads).
func scrubbedEnv() []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",             // never prompt on the terminal for creds
		"GIT_ASKPASS=/bin/false",            // and never shell out to an askpass helper
		"GIT_CONFIG_NOSYSTEM=1",             // ignore /etc/gitconfig (deterministic behavior)
		"GIT_CONFIG_GLOBAL=/dev/null",       // ignore ~/.gitconfig as well
		"HOME=/nonexistent",                 // no home-dir config / credential cache
		"GIT_FLUSH=1",                       // deterministic flushing for captured output
		"LC_ALL=C",                          // stable, locale-independent git messages
		"GIT_ALLOW_PROTOCOL=https:ssh",      // only https/ssh transports (no file://, ext::, …)
		"GIT_PROTOCOL_FROM_USER=0",          // a user-supplied URL may not pick the protocol
		"PATH=/usr/local/bin:/usr/bin:/bin", // git + its subcommands
	}
}

// Run implements gitRunner against the real binary. The context bounds the
// child's lifetime; on cancellation exec kills the process and we return the
// context error (preserved for errors.Is). A non-zero exit becomes a *GitError
// carrying argv + exit code + stderr.
func (r *execRunner) Run(ctx context.Context, repoDir string, stdin []byte, args ...string) ([]byte, error) {
	// T1: bound the child's lifetime by the server-side git-op deadline (in addition
	// to any request deadline) so a hung fetch/push cannot run unbounded.
	ctx, cancel := r.boundedContext(ctx)
	defer cancel()
	// Prepend "-C repoDir" so the working directory is explicit and we never rely
	// on (or mutate) the process cwd — important for concurrent sites.
	full := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, r.gitBin, full...)
	cmd.Env = append([]string(nil), r.env...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		// Surface context cancellation/deadline verbatim so callers can branch on
		// it (errors.Is(err, context.Canceled / DeadlineExceeded)) instead of
		// misreading a killed process as a git logic failure.
		if cerr := ctx.Err(); cerr != nil {
			return stdout.Bytes(), cerr
		}
		exitCode := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
		return stdout.Bytes(), &GitError{
			Args:     args, // argv WITHOUT the -C repoDir prefix (stable for golden tests)
			ExitCode: exitCode,
			Stderr:   stderr.String(),
		}
	}
	return stdout.Bytes(), nil
}

// authorEnv builds the per-call GIT_AUTHOR_*/GIT_COMMITTER_* environment from an
// Actor and a deterministic commit timestamp. It is appended on top of the base
// scrubbed env for commit-producing invocations. The email is synthesized from
// the user id when absent (CANONICAL §2 Actor.Email contract).
func authorEnv(a Actor, when string) []string {
	name := a.Name
	if name == "" {
		name = "kotoji"
	}
	email := a.Email
	if email == "" {
		email = a.UserID.String() + "@kotoji.local"
	}
	env := []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
	if when != "" {
		env = append(env,
			"GIT_AUTHOR_DATE="+when,
			"GIT_COMMITTER_DATE="+when,
		)
	}
	return env
}

// runWithEnv is execRunner-with-extra-env: it is NOT part of the gitRunner
// interface (the fake does not need it) so commit-author injection stays a
// production concern. gitService type-asserts for it; a fake runner that wants to
// observe author env can implement envRunner too.
type envRunner interface {
	gitRunner
	RunEnv(ctx context.Context, repoDir string, extraEnv []string, stdin []byte, args ...string) (stdout []byte, err error)
}

// RunEnv runs git with additional environment entries layered over the scrubbed
// base (used to inject the commit author/committer identity per call).
func (r *execRunner) RunEnv(ctx context.Context, repoDir string, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	// T1: same server-side deadline as Run (commit/merge/fetch via env all bounded).
	ctx, cancel := r.boundedContext(ctx)
	defer cancel()
	full := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, r.gitBin, full...)
	cmd.Env = append(append([]string(nil), r.env...), extraEnv...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return stdout.Bytes(), cerr
		}
		exitCode := -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exitCode = ee.ExitCode()
		}
		return stdout.Bytes(), &GitError{Args: args, ExitCode: exitCode, Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// compile-time guarantee execRunner satisfies both seams.
var (
	_ gitRunner = (*execRunner)(nil)
	_ envRunner = (*execRunner)(nil)
)
