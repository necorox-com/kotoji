package ops

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

// OSFS is the production FS backed by the real os package. It is the only place in
// ops that touches the filesystem directly; the job logic depends on the FS
// interface so tests substitute a t.TempDir-backed real OSFS or an in-memory fake.
type OSFS struct{}

// compile-time guarantee OSFS satisfies FS.
var _ FS = OSFS{}

// Exists reports whether path exists. A non-NotExist stat error is surfaced.
func (OSFS) Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// ListDir returns the names directly under dir. A missing dir yields an empty
// slice + nil (a fresh instance with no sites dir is not an error).
func (OSFS) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// MkdirAll creates dir and parents with 0o755.
func (OSFS) MkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

// RemoveAll removes path and its children.
func (OSFS) RemoveAll(path string) error { return os.RemoveAll(path) }

// Remove removes a single file. A missing file is not an error (idempotent lock
// clearing).
func (OSFS) Remove(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Rename atomically renames oldpath to newpath.
func (OSFS) Rename(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }

// ExecGitRunner is the production GitRunner: it shells out to the git binary with
// arg arrays only (no shell — no injection surface) and a scrubbed environment so
// git never blocks on a credential prompt. It mirrors site.execRunner's hardening
// for the bundle + gc passes.
type ExecGitRunner struct {
	gitBin string
}

// compile-time guarantee ExecGitRunner satisfies GitRunner.
var _ GitRunner = (*ExecGitRunner)(nil)

// NewExecGitRunner builds an ExecGitRunner. gitBin defaults to "git" when empty.
func NewExecGitRunner(gitBin string) *ExecGitRunner {
	if gitBin == "" {
		gitBin = "git"
	}
	return &ExecGitRunner{gitBin: gitBin}
}

// scrubbedEnv is the deterministic base environment for git (no prompts, no host
// config) — the ops mirror of site.scrubbedEnv.
func scrubbedEnv() []string {
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"HOME=/nonexistent",
		"LC_ALL=C",
		"PATH=/usr/local/bin:/usr/bin:/bin",
	}
}

// Run executes `git -C repoDir <args...>` and returns stdout. A non-zero exit
// surfaces the stderr-bearing error; ctx bounds the child lifetime.
func (r *ExecGitRunner) Run(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", repoDir}, args...)
	cmd := exec.CommandContext(ctx, r.gitBin, full...)
	cmd.Env = scrubbedEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return stdout.Bytes(), cerr
		}
		return stdout.Bytes(), &gitError{args: args, stderr: stderr.String(), wrapped: err}
	}
	return stdout.Bytes(), nil
}

// gitError carries the failing argv + stderr for diagnostics.
type gitError struct {
	args    []string
	stderr  string
	wrapped error
}

func (e *gitError) Error() string {
	return "ops: git " + join(e.args) + ": " + e.stderr
}
func (e *gitError) Unwrap() error { return e.wrapped }

// join space-joins args without importing strings (tiny helper).
func join(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += " "
		}
		out += a
	}
	return out
}
