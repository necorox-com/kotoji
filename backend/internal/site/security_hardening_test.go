package site

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- H1: GetLog argument-injection in the `before` cursor -----------------

// TestGetLog_MaliciousBeforeCursorRejected proves an attacker-controlled `before`
// pagination cursor that looks like a git OPTION (leading dash / --output=) is
// laundered through revParse and FAILS CLOSED (ErrNotFound) — it never reaches the
// `git log` argv as an option (arbitrary-file-write / option-injection gate).
func TestGetLog_MaliciousBeforeCursorRejected(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	store := newMemStore()
	clk := fixedClock()
	store.clock = clk
	svc := NewServiceWithClock(store, newExecRunner("git", defaultGitOpTimeout), Config{Root: root}, clk)

	s := newSite(t, svc, "loginj")

	malicious := []string{
		"--output=/tmp/kotoji-pwned",
		"-foo",
		"--all",
		"--output=/tmp/x;rm -rf /",
	}
	for _, before := range malicious {
		_, err := svc.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 50, Before: before})
		assert.Truef(t, errors.Is(err, ErrNotFound),
			"malicious before cursor %q must fail closed (ErrNotFound), got %v", before, err)
	}
}

// TestGetLog_ValidBeforeCursorWorks proves the laundered happy path still works: a
// real commit SHA used as `before` returns the strictly-older commits (the cursor
// commit itself is excluded), so pagination is unaffected by the H1 fix.
func TestGetLog_ValidBeforeCursorWorks(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git binary not available")
	}
	ctx := context.Background()
	root := t.TempDir()
	store := newMemStore()
	clk := fixedClock()
	store.clock = clk
	svc := NewServiceWithClock(store, newExecRunner("git", defaultGitOpTimeout), Config{Root: root}, clk)

	s := newSite(t, svc, "logok")
	tip := draftTip(t, svc, s.ID)
	// Lay down a few commits so there is something to paginate.
	for i := 0; i < 3; i++ {
		ci, err := svc.WriteFile(ctx, WriteFileInput{
			SiteID: s.ID, Branch: BranchDraft, Path: "p" + itoa(i) + ".html",
			Content: []byte("v"), BaseSHA: tip, Commit: true, Actor: testActor(),
		})
		require.NoError(t, err)
		tip = ci.SHA
	}
	older, err := svc.GetLog(ctx, LogOptions{SiteID: s.ID, Branch: BranchDraft, Limit: 50, Before: tip})
	require.NoError(t, err)
	require.NotEmpty(t, older, "a valid before cursor must return strictly-older commits")
	for _, c := range older {
		assert.NotEqual(t, tip, c.SHA, "before cursor commit must be excluded")
	}
}

// ---- S1: mirror remote URL allowlist (SSRF / file:// read) -----------------

// TestValidateRemoteURL_RejectsDangerous proves the strict allowlist rejects every
// non-GitHub-https form: file:// (local read), http:// internal IPs (SSRF /
// link-local), and arbitrary non-GitHub hosts. Failing closed here keeps a
// tenant-set github_repo from ever becoming a remote git fetch/push reads.
func TestValidateRemoteURL_RejectsDangerous(t *testing.T) {
	bad := []string{
		"file:///etc/passwd",
		"file:///etc",
		"http://169.254.169.254/latest/meta-data/",
		"https://169.254.169.254/owner/repo",
		"http://github.com/owner/repo",            // http is not https
		"https://10.0.0.5/owner/repo",             // private range IP literal
		"https://127.0.0.1/owner/repo",            // loopback
		"https://[::1]/owner/repo",                // ipv6 loopback
		"https://attacker.internal/owner/repo",    // non-GitHub host
		"https://github.com.evil.com/owner/repo",  // host-confusion suffix
		"https://evilgithub.com/owner/repo",       // not a github.com subdomain
		"ssh://git@github.com/owner/repo",         // ssh:// scheme not allowed (only https + scp shorthand)
		"ext::sh -c touch /tmp/pwned",             // ext transport (command exec)
		"https://user:pass@github.com/owner/repo", // embedded credentials
		"https://github.com/owner/repo/extra",     // extra path segment
		"https://github.com/owner",                // missing repo
		"-https://github.com/owner/repo",          // leading dash (option-injection)
		"git@evil.com:owner/repo",                 // ssh shorthand to non-GitHub host
		"",                                        // empty
	}
	for _, u := range bad {
		err := validateRemoteURL(u)
		assert.Truef(t, errors.Is(err, ErrValidation),
			"remote URL %q must be rejected (ErrValidation), got %v", u, err)
	}
}

// TestValidateRemoteURL_AcceptsGitHub proves the allowlist accepts the legitimate
// GitHub forms: the owner/repo shorthand, github.com https (with/without .git),
// github.com subdomains, the *.ghe.com enterprise suffix, and the
// git@github.com:owner/repo ssh shorthand.
func TestValidateRemoteURL_AcceptsGitHub(t *testing.T) {
	good := []string{
		"owner/repo",
		"owner/repo.git",
		"my-org/my.repo_name",
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"https://www.github.com/owner/repo",
		"https://my-co.ghe.com/owner/repo.git",
		"git@github.com:owner/repo",
		"git@github.com:owner/repo.git",
	}
	for _, u := range good {
		assert.NoErrorf(t, validateRemoteURL(u), "remote URL %q must be accepted", u)
	}
}

// TestSetRemote_RejectsDangerousURL proves the gate is enforced at the SetRemote
// boundary (not just the pure validator): a file:// remote is rejected and the
// store is never updated, so no dangerous remote can be persisted at runtime.
func TestSetRemote_RejectsDangerousURL(t *testing.T) {
	ctx := context.Background()
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) { return nil, nil }}
	store := newMemStore()
	rec, _ := store.CreateSiteWithOwner(ctx, StoreCreateSite{
		Handle: "rem", OwnerID: uuid.New(), Visibility: "private", PublishMode: "direct",
	})
	svc := NewService(store, fr, Config{Root: t.TempDir()})

	err := svc.SetRemote(ctx, rec.ID, "file:///etc/passwd")
	assert.True(t, errors.Is(err, ErrValidation), "SetRemote must reject file:// URLs")
	// The dangerous remote must never have reached `git remote add`.
	if call, ok := fr.findCall("remote"); ok {
		for _, a := range call {
			assert.NotContains(t, a, "file://", "rejected URL must never reach git argv")
		}
	}
}

// TestCreateSite_RejectsDangerousGitHubRepo proves the create-time path also gates
// the tenant-supplied GitHubRepo before it is stored or fed to `git remote add`.
func TestCreateSite_RejectsDangerousGitHubRepo(t *testing.T) {
	ctx := context.Background()
	fr := &fakeRunner{respond: func(args []string) ([]byte, error) { return nil, nil }}
	svc := NewService(newMemStore(), fr, Config{Root: t.TempDir()})

	_, err := svc.CreateSite(ctx, CreateSiteInput{
		Handle:     Handle("badmirror"),
		OwnerID:    uuid.New(),
		GitHubRepo: "http://169.254.169.254/owner/repo",
		Actor:      testActor(),
	})
	assert.True(t, errors.Is(err, ErrValidation), "CreateSite must reject an SSRF GitHubRepo")
}

// TestScrubbedEnv_ProtocolHardening asserts the git env restricts transports so a
// remote URL that somehow slipped past validation still cannot drive file:///ext::
// (S1 belt-and-suspenders).
func TestScrubbedEnv_ProtocolHardening(t *testing.T) {
	env := scrubbedEnv()
	assert.Contains(t, env, "GIT_ALLOW_PROTOCOL=https:ssh")
	assert.Contains(t, env, "GIT_PROTOCOL_FROM_USER=0")
}

// ---- P1: path validator leading-dash ---------------------------------------

// TestValidatePath_RejectsLeadingDash proves a path/dir segment beginning with a
// dash is rejected by the validator itself (independent of any call-site "--"
// discipline), so a value like "-foo" can never reach an ls-tree/add sink as an
// option (argument injection).
func TestValidatePath_RejectsLeadingDash(t *testing.T) {
	bad := []string{
		"-foo",
		"-foo.html",
		"--output=x",
		"dir/-evil.html",
		"a/b/--output=/tmp/x",
		"-rf",
	}
	for _, p := range bad {
		assert.Truef(t, errors.Is(validatePath(p), ErrValidation), "validatePath(%q) must reject leading dash", p)
		assert.Truef(t, errors.Is(validateReadPath(p), ErrValidation), "validateReadPath(%q) must reject leading dash", p)
		assert.Truef(t, errors.Is(validateDir(p), ErrValidation), "validateDir(%q) must reject leading dash", p)
	}
	// Sanity: a dash NOT at the start of a segment is still fine.
	assert.NoError(t, validatePath("my-file.html"))
	assert.NoError(t, validatePath("dir/sub-thing/file.css"))
	assert.NoError(t, validateDir("a-b"))
}

// ---- T1: git operation timeout ---------------------------------------------

// TestExecRunner_GitOpTimeout proves the runner KILLS a hung child INDEPENDENT of
// the (background, deadline-less) request context: the server-side git-op timeout
// fires and the wedged process is surfaced as context.DeadlineExceeded. We point
// the runner at a tiny "git" stand-in that ignores its args and sleeps far longer
// than the timeout, so the only thing that can end it is the bounded deadline.
func TestExecRunner_GitOpTimeout(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	// A fake git binary: ignores the "-C repoDir ..." args entirely and sleeps 30s.
	// Only the runner's bounded deadline can stop it within the test window.
	bin := filepath.Join(t.TempDir(), "slowgit")
	// `exec sleep` REPLACES the shell with sleep (no surviving grandchild), so when
	// the runner's deadline kills the process the sleep dies with it and Run returns
	// promptly rather than blocking on a held-open stdout pipe.
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755))

	r := newExecRunner(bin, 100*time.Millisecond)
	start := time.Now()
	// context.Background() has NO deadline; the kill MUST come from the runner's own
	// bounded context, proving the server-side timeout is independent of the request.
	_, err := r.Run(context.Background(), t.TempDir(), nil, "fetch", "origin")
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a hung git op must surface as context.DeadlineExceeded, got %v", err)
	assert.Less(t, elapsed, 5*time.Second, "the child must be killed near the timeout, not after the 30s sleep")
}

// TestExecRunner_BoundedContextAppliesTimeout proves boundedContext derives a child
// deadline from the runner's configured timeout even when the parent context has
// none — the core T1 guarantee (a hung git op cannot run unbounded).
func TestExecRunner_BoundedContextAppliesTimeout(t *testing.T) {
	r := newExecRunner("git", 75*time.Millisecond)
	ctx, cancel := r.boundedContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	require.True(t, ok, "bounded context must carry a deadline derived from the runner timeout")
	assert.WithinDuration(t, time.Now().Add(75*time.Millisecond), dl, 50*time.Millisecond)

	// A non-positive timeout is a pass-through (no artificial deadline imposed).
	r0 := newExecRunner("git", 0)
	ctx0, cancel0 := r0.boundedContext(context.Background())
	defer cancel0()
	_, ok0 := ctx0.Deadline()
	assert.False(t, ok0, "a zero timeout must not impose a deadline")
}

// TestConfig_GitOpTimeoutDefault asserts the Config default is applied (T1): a zero
// GitOpTimeout resolves to defaultGitOpTimeout (120s) via withDefaults.
func TestConfig_GitOpTimeoutDefault(t *testing.T) {
	got := Config{}.withDefaults()
	assert.Equal(t, defaultGitOpTimeout, got.GitOpTimeout)
	assert.Equal(t, 120*time.Second, got.GitOpTimeout)
	// An explicit value is preserved.
	custom := Config{GitOpTimeout: 5 * time.Second}.withDefaults()
	assert.Equal(t, 5*time.Second, custom.GitOpTimeout)
}
