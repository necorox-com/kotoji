package site

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
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
	gitBin string   // resolved "git" path; the Docker image MUST include the binary
	env    []string // base scrubbed environment shared by every invocation
}

// newExecRunner builds an execRunner. gitBin defaults to "git" (resolved via
// PATH by exec) when empty. The base environment is scrubbed to a deterministic
// minimum that disables every interactive/credential prompt so a misconfigured
// remote can never hang a request waiting on a terminal.
func newExecRunner(gitBin string) *execRunner {
	if gitBin == "" {
		gitBin = "git"
	}
	return &execRunner{
		gitBin: gitBin,
		env:    scrubbedEnv(),
	}
}

// scrubbedEnv is the deterministic base environment for every git invocation.
// GIT_TERMINAL_PROMPT=0 and the askpass/no-prompt settings guarantee git never
// blocks on credential input; a fixed config-nosystem keeps host git config out
// of the picture. PATH is preserved minimally so git can find its helpers.
func scrubbedEnv() []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",       // never prompt on the terminal for creds
		"GIT_ASKPASS=/bin/false",      // and never shell out to an askpass helper
		"GIT_CONFIG_NOSYSTEM=1",       // ignore /etc/gitconfig (deterministic behavior)
		"GIT_CONFIG_GLOBAL=/dev/null", // ignore ~/.gitconfig as well
		"HOME=/nonexistent",           // no home-dir config / credential cache
		"GIT_FLUSH=1",                 // deterministic flushing for captured output
		"LC_ALL=C",                    // stable, locale-independent git messages
		"PATH=/usr/local/bin:/usr/bin:/bin", // git + its subcommands
	}
}

// Run implements gitRunner against the real binary. The context bounds the
// child's lifetime; on cancellation exec kills the process and we return the
// context error (preserved for errors.Is). A non-zero exit becomes a *GitError
// carrying argv + exit code + stderr.
func (r *execRunner) Run(ctx context.Context, repoDir string, stdin []byte, args ...string) ([]byte, error) {
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
